package fleet

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunnerConfig is the subset of the runner's .runner file we use.
type RunnerConfig struct {
	Name      string `json:"agentName"`
	Pool      string `json:"poolName"`
	GitHubURL string `json:"gitHubUrl"`
}

// ReadRunnerConfig reads <runnerDir>/.runner (which is written with a UTF-8 BOM).
func ReadRunnerConfig(runnerDir string) (*RunnerConfig, error) {
	b, err := os.ReadFile(filepath.Join(runnerDir, ".runner"))
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var c RunnerConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ListenerEvent is the last state change the listener reported.
type ListenerEvent string

const (
	ListenerUnknown   ListenerEvent = "unknown"
	ListenerListening ListenerEvent = "listening" // "Listening for Jobs"
	ListenerBusy      ListenerEvent = "busy"      // "Running job: …"
	ListenerExited    ListenerEvent = "exited"    // listener process logged its shutdown
)

// ListenerState is derived from the newest Runner_*.log. It reflects what the
// listener last logged, not whether its process is alive; process liveness is
// checked separately (see runner control).
type ListenerState struct {
	Event   ListenerEvent `json:"event"`
	Since   time.Time     `json:"since,omitzero"`
	Job     string        `json:"job,omitempty"` // display name of the running job, when busy
	LogFile string        `json:"log_file,omitempty"`
}

// ParseRunnerLog extracts the listener state from a Runner_*.log stream.
func ParseRunnerLog(r io.Reader) (ListenerState, error) {
	st := ListenerState{Event: ListenerUnknown}
	sc := newLineScanner(r)
	for sc.Scan() {
		st = st.apply(sc.Text())
	}
	return st, sc.Err()
}

// apply folds one Runner_*.log line into the state.
func (st ListenerState) apply(line string) ListenerState {
	ts, component, msg, ok := parseLogLine(strings.TrimRight(line, "\r"))
	if !ok {
		return st
	}
	// Console output is mirrored as: WRITE LINE: 2026-11-17 16:11:24Z: Running job: Test (3/4)
	if component == "Terminal" && strings.HasPrefix(msg, "WRITE LINE: ") {
		text := strings.TrimPrefix(msg, "WRITE LINE: ")
		if _, rest, found := strings.Cut(text, "Z: "); found {
			text = rest
		}
		switch {
		case text == "Listening for Jobs":
			return ListenerState{Event: ListenerListening, Since: ts, LogFile: st.LogFile}
		case strings.HasPrefix(text, "Running job: "):
			return ListenerState{Event: ListenerBusy, Since: ts, Job: strings.TrimPrefix(text, "Running job: "), LogFile: st.LogFile}
		case strings.Contains(text, " completed with result: "):
			return ListenerState{Event: ListenerListening, Since: ts, LogFile: st.LogFile}
		}
		return st
	}
	// Every shutdown on 2.338.x ends with "[… Listener] Runner execution been
	// cancelled." or "… has finished with return code N". ("Deleting Runner
	// Session" alone is not an exit: it also precedes a session restart after
	// a config update.)
	if component == "Listener" && strings.HasPrefix(msg, "Runner execution") {
		return ListenerState{Event: ListenerExited, Since: ts, LogFile: st.LogFile}
	}
	return st
}
