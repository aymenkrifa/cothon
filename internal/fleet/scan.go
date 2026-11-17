package fleet

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Snapshot is the full local view of a runner at one point in time.
type Snapshot struct {
	Runner    *RunnerConfig `json:"runner,omitempty"`
	Listener  ListenerState `json:"listener"`
	Processes *Processes    `json:"processes,omitempty"` // nil when liveness was not checked
	Jobs      []*Job        `json:"jobs"`                // newest first
	Errors    []string      `json:"errors,omitempty"`
	Taken     time.Time     `json:"taken"`
}

// Scanner reads a runner directory and caches parsed worker logs, so repeated
// scans only re-read files that changed (the running job's log), and only
// reads the new tail of the current Runner_*.log.
type Scanner struct {
	RunnerDir string
	// Procs, when set, reports live runner processes; it lets the scanner tell
	// a running job from one whose worker died. Nil in tests.
	Procs func() Processes

	mu        sync.Mutex
	cache     map[string]cachedJob
	runnerLog runnerLogCursor
}

type cachedJob struct {
	size  int64
	mtime time.Time
	job   *Job
	err   string
}

type runnerLogCursor struct {
	path        string
	offset      int64
	state       ListenerState
	listenerPID int
}

func NewScanner(runnerDir string) *Scanner {
	return &Scanner{RunnerDir: runnerDir, cache: map[string]cachedJob{}}
}

// Scan builds a snapshot. Per-file problems are reported in Snapshot.Errors
// instead of failing the whole scan.
func (s *Scanner) Scan() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := &Snapshot{Taken: time.Now()}
	if cfg, err := ReadRunnerConfig(s.RunnerDir); err == nil {
		snap.Runner = cfg
	} else {
		snap.Errors = append(snap.Errors, "runner config: "+err.Error())
	}

	diag := filepath.Join(s.RunnerDir, "_diag")
	if _, err := os.Stat(diag); err != nil {
		return nil, err
	}
	// Processes and the listener log first: a job that finishes while we read
	// the worker logs then looks finished, never "abandoned".
	if s.Procs != nil {
		p := s.Procs()
		snap.Processes = &p
	}
	preferred, listenerPID := "", 0
	if snap.Processes != nil {
		preferred, listenerPID = snap.Processes.ListenerLog, snap.Processes.ListenerPID
	}
	if err := s.readRunnerLog(diag, preferred, listenerPID); err != nil {
		snap.Errors = append(snap.Errors, err.Error())
	}
	snap.Listener = s.runnerLog.state

	workers, err := filepath.Glob(filepath.Join(diag, "Worker_*.log"))
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(workers))
	for _, path := range workers {
		seen[path] = true
		fi, err := os.Stat(path)
		if err != nil {
			continue // removed between glob and stat
		}
		if fi.Size() == 0 {
			continue // just created by the worker
		}
		c, ok := s.cache[path]
		if !ok || c.size != fi.Size() || !c.mtime.Equal(fi.ModTime()) {
			j, err := ParseWorkerLogFile(path)
			c = cachedJob{size: fi.Size(), mtime: fi.ModTime(), job: j}
			if err != nil {
				c.err = err.Error() // cached too: don't re-read a broken file every scan
			}
			s.cache[path] = c
		}
		if c.err != "" {
			snap.Errors = append(snap.Errors, c.err)
			continue
		}
		snap.Jobs = append(snap.Jobs, c.job)
	}
	for path := range s.cache {
		if !seen[path] {
			delete(s.cache, path)
		}
	}
	// Newest first; equal start seconds are ordered by file name (the names
	// embed a sortable timestamp), so pages always follow their job.
	sort.SliceStable(snap.Jobs, func(a, b int) bool {
		ja, jb := snap.Jobs[a], snap.Jobs[b]
		if !ja.Started.Equal(jb.Started) {
			return ja.Started.After(jb.Started)
		}
		return filepath.Base(ja.LogFile) > filepath.Base(jb.LogFile)
	})
	snap.Jobs = mergePages(snap.Jobs)

	// Decide which unfinished jobs are really still running. Copy before
	// changing so cached entries stay exactly as parsed.
	abandon := func(i int) {
		cp := *snap.Jobs[i]
		cp.Status = StatusAbandoned
		snap.Jobs[i] = &cp
	}
	for i, j := range snap.Jobs {
		if j.Status != StatusRunning {
			continue
		}
		switch {
		case i > 0:
			// The runner executes one job at a time: an unfinished job with a
			// newer job after it was abandoned (worker crash / runner killed).
			abandon(i)
		case snap.Processes != nil && snap.Processes.Running() && !snap.Processes.Busy():
			// The listener is visible but has no Runner.Worker: nothing runs.
			// (If no listener is visible at all, e.g. the runner belongs to a
			// user whose processes we can't see, don't guess.)
			abandon(i)
		case (snap.Listener.Event == ListenerListening || snap.Listener.Event == ListenerExited) &&
			snap.Listener.Since.After(j.Started):
			// The listener went back to listening (or exited) after this job started.
			abandon(i)
		}
	}
	// Workers that died before receiving their job count above (as "a newer
	// job exists"), but are not jobs to show.
	shown := snap.Jobs[:0:0]
	for _, j := range snap.Jobs {
		if !j.startupOnly {
			shown = append(shown, j)
		}
	}
	snap.Jobs = shown
	return snap, nil
}

// readRunnerLog advances the cursor over the listener's Runner_*.log (the
// preferred file if given, else the newest), reading only complete lines
// appended since the last call.
func (s *Scanner) readRunnerLog(diag, preferred string, listenerPID int) error {
	latest := preferred
	if latest == "" {
		// No live listener to ask: use the most recently written log (a
		// failed second listener can leave a newer-named but stale file).
		runners, _ := filepath.Glob(filepath.Join(diag, "Runner_*.log"))
		var newest time.Time
		for _, r := range runners {
			if fi, err := os.Stat(r); err == nil && fi.ModTime().After(newest) {
				newest, latest = fi.ModTime(), r
			}
		}
		if latest == "" {
			s.runnerLog = runnerLogCursor{state: ListenerState{Event: ListenerUnknown}}
			return nil
		}
	}

	c := &s.runnerLog
	fi, err := os.Stat(latest)
	if err != nil {
		return err
	}
	switch {
	case c.path != latest && preferred != "" && c.path != "" && c.listenerPID == listenerPID && listenerPID != 0:
		// Same listener, new log page (the runner pages logs at ~8 MB):
		// finish the old page, keep the state, read the new one from the start.
		if err := c.drain(); err != nil {
			return err
		}
		c.path, c.offset = latest, 0
		c.state.LogFile = latest
	case c.path != latest || fi.Size() < c.offset:
		*c = runnerLogCursor{path: latest, state: ListenerState{Event: ListenerUnknown, LogFile: latest}}
	}
	c.listenerPID = listenerPID
	if fi.Size() == c.offset {
		return nil
	}
	return c.drain()
}

// drain reads complete lines appended to the cursor's file since its offset.
func (c *runnerLogCursor) drain() error {
	f, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil // a partial line stays for the next read
			}
			return err
		}
		c.offset += int64(len(line))
		c.state = c.state.apply(strings.TrimRight(line, "\r\n"))
	}
}

// mergePages folds headerless Worker_*.log pages (the runner starts a new
// page after ~8 MB) into the job they continue: the closest older job with a
// header. jobs is sorted newest first; the result keeps that order.
func mergePages(jobs []*Job) []*Job {
	out := jobs[:0:0]
	var pending []*Job // headerless pages waiting for their job (newer than it)
	for _, j := range jobs {
		if !j.hasHeader && !j.startupOnly {
			pending = append(pending, j)
			continue
		}
		if len(pending) > 0 && !j.startupOnly {
			cp := *j
			cp.LogFile = pending[0].LogFile // the newest page is the live one
			for _, p := range pending {     // newest page first: it has the final word
				if p.Status == StatusCompleted || p.Result != "" {
					cp.Status, cp.Result, cp.Finished = p.Status, p.Result, p.Finished
					break
				}
			}
			j, pending = &cp, nil
		}
		out = append(out, j)
	}
	return out // pages with no job before them are dropped
}
