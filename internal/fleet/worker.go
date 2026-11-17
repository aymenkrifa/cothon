package fleet

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

const logTimeLayout = "2006-01-02 15:04:05"

// parseLogLine splits a diag log line of the form
//
//	[2026-11-17 16:11:25Z INFO JobRunner] message
//
// ok is false for continuation lines (e.g. the pretty-printed job message JSON).
// Hand-rolled instead of a regexp: it runs on every line of every log.
func parseLogLine(s string) (ts time.Time, component, msg string, ok bool) {
	// "[" + 19-char timestamp + "Z "
	if len(s) < 23 || s[0] != '[' || s[20] != 'Z' || s[21] != ' ' {
		return time.Time{}, "", "", false
	}
	ts, err := time.ParseInLocation(logTimeLayout, s[1:20], time.UTC) // runner logs are UTC ("Z")
	if err != nil {
		return time.Time{}, "", "", false
	}
	rest := s[22:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return time.Time{}, "", "", false
	}
	header := rest[:end] // "INFO JobRunner"
	_, component, found := strings.Cut(header, " ")
	if !found {
		return time.Time{}, "", "", false
	}
	msg = strings.TrimPrefix(rest[end+1:], " ")
	return ts, component, msg, true
}

// Files bigger than this are read head + tail only: the job message sits at
// the top and the result/completion lines at the very end.
const (
	fullReadLimit = 256 << 10
	tailBytes     = 64 << 10
	headLimit     = 8 << 20 // safety cap while looking for the end of the header
)

// ParseWorkerLogFile parses one Worker_*.log file. A final line without a
// newline is still being written by the runner and is left for the next scan.
func ParseWorkerLogFile(path string) (*Job, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return nil, errEmptyLog
	}

	p := newWorkerParser()
	br := bufio.NewReaderSize(f, 64*1024)
	if fi.Size() <= fullReadLimit {
		if _, err := readLines(br, p.line, func() bool { return false }); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else {
		// Head: everything up to the end of the job message and the job id.
		// The job message sits within the first lines; a continuation page has
		// none, so give up on the header early instead of reading 8 MB.
		consumed, err := readLines(br, p.line, func() bool {
			return p.headerDone() || p.read > headLimit || (!p.gotMsg && p.read > 64<<10)
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if consumed < fi.Size()-tailBytes {
			// Tail: the last tailBytes hold the result and completion lines.
			start := fi.Size() - tailBytes
			partialFirst := true
			var prev [1]byte
			if _, err := f.ReadAt(prev[:], start-1); err == nil && prev[0] == '\n' {
				partialFirst = false
			}
			if _, err := f.Seek(start, io.SeekStart); err != nil {
				return nil, err
			}
			br.Reset(f)
			p.endMessage()
			skip := partialFirst
			if _, err := readLines(br, func(l string) {
				if skip {
					skip = false
					return
				}
				p.line(l)
			}, func() bool { return false }); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		} else if _, err := readLines(br, p.line, func() bool { return false }); err != nil {
			// Head and tail overlap: just read the rest.
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	j, err := p.finish()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	j.LogFile = path
	return j, nil
}

var errEmptyLog = errors.New("empty log")

// readLines feeds complete lines (without the newline, CR trimmed) to fn
// until EOF or stop() reports true after a line, and returns the bytes
// consumed. A trailing partial line is not fed.
func readLines(br *bufio.Reader, fn func(string), stop func() bool) (int64, error) {
	var n int64
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return n, nil // partial last line (if any) is still being written
			}
			return n, err
		}
		n += int64(len(line))
		fn(strings.TrimRight(line, "\r\n"))
		if stop() {
			return n, nil
		}
	}
}

// ParseWorkerLog parses a whole worker log stream (used by tests and tools).
// It is tolerant: fields it cannot find are left empty rather than failing,
// because the log is an internal runner format that may change.
func ParseWorkerLog(r io.Reader) (*Job, error) {
	p := newWorkerParser()
	sc := newLineScanner(r)
	for sc.Scan() {
		p.line(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return p.finish()
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return sc
}

type workerParser struct {
	firstComp string // component of the first log line ("HostContext" for a fresh worker)
	read      int64  // bytes of lines seen
	j         *Job
	inMessage bool
	gotMsg    bool
	message   bytes.Buffer
	sawLine   bool
}

func newWorkerParser() *workerParser {
	return &workerParser{j: &Job{Status: StatusRunning}}
}

// headerDone reports whether everything that lives at the top of the log
// (job message and job id) has been read.
func (p *workerParser) headerDone() bool {
	return p.gotMsg && !p.inMessage && p.j.ID != ""
}

func (p *workerParser) endMessage() {
	if p.inMessage {
		p.inMessage = false
		applyJobMessage(p.j, p.message.Bytes())
		p.message.Reset()
	}
}

func (p *workerParser) line(line string) {
	p.read += int64(len(line)) + 1
	ts, component, msg, ok := parseLogLine(line)
	if !ok {
		if p.inMessage {
			p.message.WriteString(line)
			p.message.WriteByte('\n')
		}
		return
	}
	p.endMessage()
	j := p.j
	if !p.sawLine {
		j.Started = ts
		p.sawLine = true
		p.firstComp = component
	}
	switch {
	case component == "Worker" && msg == "Job message:":
		p.inMessage = true
		p.gotMsg = true
		p.message.Reset()
	case component == "JobRunner" && strings.HasPrefix(msg, "Job ID "):
		if j.ID == "" {
			j.ID = strings.TrimSpace(strings.TrimPrefix(msg, "Job ID "))
		}
	case strings.HasPrefix(msg, "Job result after all job steps finish: "):
		j.Result = strings.TrimSpace(strings.TrimPrefix(msg, "Job result after all job steps finish: "))
		j.Finished = ts
	case component == "Worker" && msg == "Job completed.":
		j.Status = StatusCompleted
		j.Finished = ts
	}
}

func (p *workerParser) finish() (*Job, error) {
	p.endMessage()
	p.j.hasHeader = p.gotMsg
	// A worker log that starts like a fresh worker but never got its job
	// message: the worker died before receiving the job. Not a page.
	p.j.startupOnly = !p.gotMsg && p.firstComp == "HostContext"
	if !p.sawLine {
		if p.read == 0 {
			return nil, errEmptyLog // only a partial first line so far
		}
		return nil, fmt.Errorf("not a runner diag log")
	}
	// A result line without "Job completed." still means the steps are done.
	if p.j.Status == StatusRunning && p.j.Result != "" {
		p.j.Status = StatusCompleted
	}
	return p.j, nil
}

// jobMessage is the subset of the runner's AgentJobRequestMessage we use.
type jobMessage struct {
	JobID          string                     `json:"jobId"`
	JobDisplayName string                     `json:"jobDisplayName"`
	ContextData    map[string]json.RawMessage `json:"contextData"`
}

// pipelineDict is the runner's serialized dictionary context: {"t":2,"d":[{"k":…,"v":…}]}.
type pipelineDict struct {
	D []struct {
		K string          `json:"k"`
		V json.RawMessage `json:"v"`
	} `json:"d"`
}

func applyJobMessage(j *Job, raw []byte) {
	var m jobMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		// Malformed or truncated JSON: fall back to scanning k/v pairs.
		applyKV(j, scanKV(raw))
		return
	}
	if m.JobID != "" {
		j.ID = m.JobID
	}
	j.Name = m.JobDisplayName

	kv := map[string]string{}
	for _, ctx := range []string{"github", "job"} {
		var d pipelineDict
		if err := json.Unmarshal(m.ContextData[ctx], &d); err != nil {
			continue
		}
		for _, e := range d.D {
			if _, seen := kv[e.K]; !seen {
				kv[e.K] = rawScalar(e.V)
			}
		}
	}
	applyKV(j, kv)
}

// rawScalar renders a JSON scalar as a plain string; numbers keep their
// literal text so large ids are not mangled by float64.
func rawScalar(v json.RawMessage) string {
	v = bytes.TrimSpace(v)
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	if len(v) > 0 && (v[0] == '{' || v[0] == '[') {
		return "" // nested context, not a scalar
	}
	return string(v)
}

var (
	kvPair        = regexp.MustCompile(`"k":\s*"([^"]+)",\s*"v":\s*("(?:[^"\\]|\\.)*"|[-0-9.eE+]+|true|false|null)`)
	kvDisplayName = regexp.MustCompile(`"jobDisplayName":\s*("(?:[^"\\]|\\.)*")`)
	kvJobID       = regexp.MustCompile(`"jobId":\s*"([^"]+)"`)
)

// scanKV extracts the first value of every "k"/"v" pair in a blob.
func scanKV(raw []byte) map[string]string {
	kv := map[string]string{}
	for _, m := range kvPair.FindAllSubmatch(raw, -1) {
		k := string(m[1])
		if _, seen := kv[k]; !seen {
			kv[k] = rawScalar(m[2])
		}
	}
	if m := kvDisplayName.FindSubmatch(raw); m != nil {
		kv["__jobDisplayName"] = rawScalar(m[1])
	}
	if m := kvJobID.FindSubmatch(raw); m != nil {
		kv["__jobId"] = string(m[1])
	}
	return kv
}

func applyKV(j *Job, kv map[string]string) {
	set := func(dst *string, key string) {
		if v := kv[key]; v != "" {
			*dst = v
		}
	}
	set(&j.ID, "__jobId")
	set(&j.Name, "__jobDisplayName")
	set(&j.Repository, "repository")
	set(&j.Workflow, "workflow")
	set(&j.RunID, "run_id")
	set(&j.RunNumber, "run_number")
	set(&j.RunAttempt, "run_attempt")
	set(&j.Ref, "ref")
	set(&j.SHA, "sha")
	set(&j.Actor, "actor")
	set(&j.Event, "event_name")
	set(&j.ServerURL, "server_url")
	if v := kv["check_run_id"]; v != "" {
		j.CheckRunID = integerID(v)
	}
}

// integerID turns "134178177665.0" into "134178177665"; anything that is not
// a plain integer (optionally with a zero fraction) is dropped.
func integerID(v string) string {
	whole, frac, _ := strings.Cut(v, ".")
	if whole == "" || strings.Trim(frac, "0") != "" {
		return ""
	}
	for _, c := range whole {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return whole
}
