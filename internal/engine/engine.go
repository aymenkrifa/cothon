package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/aymenkrifa/cothon/internal/config"
	"github.com/aymenkrifa/cothon/internal/fleet"
	"github.com/aymenkrifa/cothon/internal/gh"
	"github.com/aymenkrifa/cothon/internal/runnerctl"
)

// Timings. The engine is event driven: it re-reads logs only when inotify
// reports a change, plus a slow liveness check for listeners that die
// without logging anything.
const (
	debounce      = 400 * time.Millisecond
	livenessEvery = 20 * time.Second
	pendingEvery  = 1 * time.Second
)

type Options struct {
	// Notify enables desktop notifications (daemon / web modes).
	Notify bool
	// Org enables org polling when the config allows it.
	Org bool
	Log *slog.Logger
}

type Engine struct {
	cfg      config.Config
	opts     Options
	log      *slog.Logger
	scanner  *fleet.Scanner
	cli      gh.CLI
	api      *gh.Client // org polling
	queueAPI *gh.Client // queue polling (own ETag cache)
	ctl      *runnerctl.Controller

	mu         sync.Mutex
	state      *State
	stateJSON  []byte // comparison form of state (see comparable)
	subs       map[chan struct{}]struct{}
	closed     bool // Run has returned: Watch hands out closed channels
	pending    string
	signaled   time.Time // when SIGINT was sent for the pending stop (zero: not yet)
	notice     *Notice
	org        *OrgState
	queue      *QueueState
	queueFails map[string]int // repo -> consecutive failed queue polls (poll goroutine only)
	usage      *Usage
	usageHist  map[string]UsageSummary // job ID -> what it used (saved to usageFile)
	usageFile  string
	runningKey string          // the job running at the last scan (a change pokes the queue)
	seen       map[string]bool // job keys already notified / known at startup
	suspect    map[string]bool // jobs seen abandoned once (confirmed on the next scan)
	primed     bool
	restarting bool      // a restart goroutine is in flight
	lastStart  time.Time // last time we launched run.sh (single-flight guard)

	refreshCh chan struct{}
	orgPoke   chan struct{}
	queuePoke chan struct{}
	usagePoke chan struct{}
	notify    func(title, body string, critical bool)
}

func New(cfg config.Config, opts Options) *Engine {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	e := &Engine{
		cfg:       cfg,
		opts:      opts,
		log:       opts.Log,
		scanner:   fleet.NewScanner(cfg.RunnerDir),
		cli:       gh.CLI{Path: cfg.GH},
		subs:      map[chan struct{}]struct{}{},
		seen:      map[string]bool{},
		suspect:   map[string]bool{},
		refreshCh: make(chan struct{}, 1),
		orgPoke:   make(chan struct{}, 1),
		queuePoke: make(chan struct{}, 1),
		usagePoke: make(chan struct{}, 1),
		notify:    notifySend,
	}
	e.scanner.Procs = func() fleet.Processes { return fleet.FindProcesses(cfg.RunnerDir) }
	e.api = gh.NewClient(e.cli.Token)
	e.queueAPI = gh.NewClient(e.cli.Token)
	e.ctl = &runnerctl.Controller{
		RunnerDir: cfg.RunnerDir,
		Mode:      cfg.RunnerStart,
		LogFile:   filepath.Join(stateDir(), "runner.out"),
	}
	e.state = &State{RunnerDir: cfg.RunnerDir}
	e.usageFile = filepath.Join(stateDir(), "usage.json")
	e.usageHist = loadUsage(e.usageFile)
	return e
}

// Run watches the runner until ctx is canceled. The first scan happens
// before Run returns its first state, so callers can call State right after
// Ready is closed.
func (e *Engine) Run(ctx context.Context, ready chan<- struct{}) error {
	// Whatever ends Run, subscribers (SSE streams, the TUI) must learn of it.
	defer e.closeSubs()
	e.refresh()
	if ready != nil {
		close(ready)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	diag := filepath.Join(e.cfg.RunnerDir, "_diag")
	if err := w.Add(diag); err != nil {
		return fmt.Errorf("watch %s: %w", diag, err)
	}

	if e.opts.Org && e.cfg.Org.Enabled {
		go e.orgLoop(ctx)
	}
	if e.opts.Org && e.cfg.Queue.Enabled {
		go e.queueLoop(ctx)
	}
	go e.usageLoop(ctx)

	var (
		debounceT *time.Timer
		debounceC <-chan time.Time
		liveness  = time.NewTimer(livenessEvery)
	)
	defer liveness.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			if !strings.HasSuffix(ev.Name, ".log") {
				continue
			}
			if debounceT == nil {
				debounceT = time.NewTimer(debounce)
				debounceC = debounceT.C
			}
		case err := <-w.Errors:
			e.log.Warn("watch error", "err", err)
		case <-debounceC:
			debounceT, debounceC = nil, nil
			e.refresh()
		case <-e.refreshCh:
			e.refresh()
		case <-liveness.C:
			e.refresh()
		}
		// Poll fast only while waiting for a signaled listener to exit; while a
		// job runs, its log writes (and "Job completed.") already wake us.
		e.mu.Lock()
		next := livenessEvery
		if e.pending != "" && !e.signaled.IsZero() {
			next = pendingEvery
		}
		e.mu.Unlock()
		if !liveness.Stop() {
			select {
			case <-liveness.C:
			default:
			}
		}
		liveness.Reset(next)
	}
}

func (e *Engine) kick() {
	select {
	case e.refreshCh <- struct{}{}:
	default:
	}
}

// refresh rescans the runner and publishes a new state if anything changed.
func (e *Engine) refresh() {
	snap, err := e.scanner.Scan()
	e.mu.Lock()
	defer e.mu.Unlock()

	next := &State{RunnerDir: e.cfg.RunnerDir, Jobs: []JobView{}}
	if err != nil {
		// Keep showing the last good view; don't treat a read error as "the
		// runner stopped" or "every job is new".
		prev := e.state
		next.Runner, next.Listener, next.Processes, next.Jobs = prev.Runner, prev.Listener, prev.Processes, prev.Jobs
		if next.Jobs == nil {
			next.Jobs = []JobView{}
		}
		next.Warnings = append(next.Warnings, "cannot read runner logs: "+err.Error())
	} else {
		next.Runner = snap.Runner
		next.Listener = snap.Listener
		if snap.Processes != nil {
			next.Processes = *snap.Processes
		}
		for i, j := range snap.Jobs {
			if i >= e.cfg.MaxJobs {
				break
			}
			jv := JobView{Job: j, HTMLURL: j.HTMLURL(), RunURL: j.RunURL()}
			if u, ok := e.usageHist[j.ID]; ok && j.Status != fleet.StatusRunning {
				jv.Usage = &u
			}
			next.Jobs = append(next.Jobs, jv)
		}
		next.Warnings = append(next.Warnings, snap.Errors...)
		if !e.lastStart.IsZero() && next.Processes.Alive() {
			e.lastStart = time.Time{} // the launched runner is up: the start guard is done
		}
		e.advancePending(next)
		e.notifyFinished(next)
	}
	next.Pending = e.pending
	next.Org = e.org
	next.Queue = e.visibleQueue(next)
	if e.queue != nil && e.queue.Warning != "" {
		next.Warnings = append(next.Warnings, e.queue.Warning)
	}
	// A job started or finished here: the queue just changed.
	if rk := runningKey(next); rk != e.runningKey {
		e.runningKey = rk
		go e.pokeQueue()
		go e.pokeUsage() // start (or finish) measuring right away
	}
	next.Notice = e.notice
	if e.org != nil && e.org.Warning != "" {
		next.Warnings = append(next.Warnings, e.org.Warning)
	}
	e.publish(next)
}

// publish swaps in next if it differs from the current state. Caller holds mu.
func (e *Engine) publish(next *State) {
	j := comparable(next)
	if e.state.Version != 0 && bytes.Equal(j, e.stateJSON) {
		return
	}
	next.Version = e.state.Version + 1
	next.Updated = time.Now()
	e.state, e.stateJSON = next, j
	for ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default: // subscriber already has a pending wakeup
		}
	}
}

// comparable serializes what clients can see, minus bookkeeping that changes
// without anything visible changing (version, timestamps, API quota).
func comparable(s *State) []byte {
	c := *s
	c.Version, c.Updated = 0, time.Time{}
	if s.Org != nil {
		o := *s.Org
		o.Updated, o.RateRemaining = time.Time{}, 0
		c.Org = &o
	}
	if s.Queue != nil {
		q := *s.Queue
		q.Updated = time.Time{}
		c.Queue = &q
	}
	b, _ := json.Marshal(c)
	return b
}

// signalTimeout bounds how long a signaled listener may take to exit.
const signalTimeout = 90 * time.Second

// advancePending executes a deferred stop/restart once the runner is idle.
// Caller holds mu.
func (e *Engine) advancePending(s *State) {
	if e.pending == "" {
		return
	}
	if !s.Processes.Alive() {
		switch {
		case e.pending == "stop":
			e.notice = &Notice{At: time.Now(), OK: true, Message: "runner stopped"}
		case e.pending == "restart" && !e.restarting:
			// It exited (on its own, or slower than the restart wanted to
			// wait): finish the restart now.
			e.restarting = true
			go e.restartAfterStop()
		}
		e.pending, e.signaled = "", time.Time{}
		return
	}
	if !e.signaled.IsZero() {
		if time.Since(e.signaled) > signalTimeout {
			e.notice = &Notice{At: time.Now(), OK: false, Message: e.pending + ": the runner did not exit within 90s after SIGINT"}
			e.pending, e.signaled = "", time.Time{}
		}
		return // waiting for the listener to exit
	}
	if s.Processes.Busy() || !s.Processes.Running() {
		return // job still running, or listener restarting under run.sh
	}
	// Idle now. A job could still be assigned in the instant before SIGINT
	// lands; the runner then reports it as canceled.
	if err := e.ctl.Interrupt(); err != nil {
		e.notice = &Notice{At: time.Now(), OK: false, Message: "stop failed: " + err.Error()}
		e.pending = ""
		return
	}
	e.log.Info("sent SIGINT to idle runner listener", "pending", e.pending)
	if e.pending == "restart" {
		e.pending = ""
		e.restarting = true
		go e.restartAfterStop()
		return
	}
	e.signaled = time.Now()
}

// restartAfterStop waits for the runner to exit, then starts it again.
// Callers set e.restarting (under mu) before starting it, so no second
// restart or stop can slip in between.
func (e *Engine) restartAfterStop() {
	defer func() {
		e.mu.Lock()
		e.restarting = false
		e.mu.Unlock()
		e.kick()
	}()

	var n *Notice
	if !e.ctl.WaitStopped(context.Background(), 60*time.Second) {
		// Still cancelling its job: hand over to the state machine, which
		// starts it once it exits (or reports after signalTimeout).
		e.mu.Lock()
		e.pending, e.signaled = "restart", time.Now()
		e.notice = &Notice{At: time.Now(), OK: true, Message: "restart: waiting for the runner to exit"}
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	e.lastStart = time.Time{} // a stop just happened; this start is not a duplicate
	e.mu.Unlock()
	if msg, err := e.startRunner(); err != nil {
		n = &Notice{At: time.Now(), OK: false, Message: "restart: " + err.Error()}
	} else {
		n = &Notice{At: time.Now(), OK: true, Message: "runner restarted: " + msg}
	}
	e.mu.Lock()
	e.notice = n
	e.mu.Unlock()
}

// startGrace: after launching run.sh, the listener takes a few seconds to
// appear; refuse another start meanwhile (a second listener = session conflict).
const startGrace = 30 * time.Second

func (e *Engine) startRunner() (string, error) {
	running := e.ctl.Procs().Running() // /proc walk outside the lock
	e.mu.Lock()
	if since := time.Since(e.lastStart); since < startGrace && !running {
		e.mu.Unlock()
		return "", fmt.Errorf("a start is already in progress (%ds ago)", int(since.Seconds()))
	}
	e.lastStart = time.Now()
	e.mu.Unlock()
	msg, err := e.ctl.Start()
	e.mu.Lock()
	if err != nil {
		e.lastStart = time.Time{}
	} else {
		e.pending, e.signaled = "", time.Time{} // a fresh runner has nothing pending
	}
	e.mu.Unlock()
	return msg, err
}

// notifyFinished sends a desktop notification for jobs that finished since the
// previous scan. Jobs present at startup are never notified. An "abandoned"
// verdict must hold on two consecutive scans (the worker may exit between
// reading its log and checking processes). Caller holds mu.
func (e *Engine) notifyFinished(s *State) {
	present := make(map[string]bool, len(s.Jobs))
	for _, j := range s.Jobs {
		key := j.ID + "|" + j.LogFile
		present[key] = true
		if j.Status == fleet.StatusRunning {
			delete(e.suspect, key) // the abandoned sighting was a blip
			continue
		}
		if e.seen[key] {
			continue
		}
		if j.Status == fleet.StatusAbandoned && e.primed && !e.suspect[key] {
			e.suspect[key] = true
			continue
		}
		delete(e.suspect, key)
		e.seen[key] = true
		if !e.primed || !e.opts.Notify || e.cfg.Notify == "none" {
			continue
		}
		failed := j.Status == fleet.StatusAbandoned || j.Result != "Succeeded"
		if e.cfg.Notify == "failed" && !failed {
			continue
		}
		result := j.Result
		if j.Status == fleet.StatusAbandoned {
			result = "Abandoned"
		}
		icon := map[string]string{"Succeeded": "✓", "Failed": "✗", "Canceled": "⊘"}[result]
		if icon == "" {
			icon = "!"
		}
		title := fmt.Sprintf("%s %s · %s", icon, shortRepo(j.Repository), j.Name)
		body := fmt.Sprintf("%s in %s — %s #%s", result, j.Duration(time.Now()).Round(time.Second), j.Workflow, j.RunNumber)
		go e.notify(title, body, failed)
	}
	// Forget jobs that left the window, but only those we saw; MaxJobs trims
	// the oldest, which never come back.
	for k := range e.seen {
		if !present[k] {
			delete(e.seen, k)
		}
	}
	for k := range e.suspect {
		if !present[k] {
			delete(e.suspect, k)
		}
	}
	e.primed = true
}

func shortRepo(r string) string {
	if _, name, ok := strings.Cut(r, "/"); ok {
		return name
	}
	return r
}

// State implements Backend.
func (e *Engine) State(context.Context) (*State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, nil
}

// Watch implements Backend.
func (e *Engine) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		close(ch)
		return ch
	}
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	go func() {
		<-ctx.Done()
		e.mu.Lock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
			close(ch)
		}
		e.mu.Unlock()
	}()
	return ch
}

func (e *Engine) closeSubs() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for ch := range e.subs {
		delete(e.subs, ch)
		close(ch)
	}
}

func runningKey(s *State) string {
	if j := s.RunningJob(); j != nil {
		return j.ID
	}
	return ""
}

// visibleQueue drops queued jobs that already started here (the poll lags
// behind the local logs). Caller holds mu.
func (e *Engine) visibleQueue(s *State) *QueueState {
	if e.queue == nil {
		return nil
	}
	running := map[string]bool{}
	for _, j := range s.Jobs {
		if j.CheckRunID != "" {
			running[j.CheckRunID] = true
		}
	}
	q := *e.queue
	q.Jobs = make([]QueuedJob, 0, len(e.queue.Jobs))
	for _, j := range e.queue.Jobs {
		if !running[j.JobID] {
			q.Jobs = append(q.Jobs, j)
		}
	}
	return &q
}
