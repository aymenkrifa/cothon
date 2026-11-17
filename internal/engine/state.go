// Package engine keeps the live state of a runner (local logs + optional org
// polling), publishes changes to subscribers and executes actions. The daemon,
// the web UI and the in-process TUI all sit on top of it.
package engine

import (
	"context"
	"time"

	"github.com/aymenkrifa/cothon/internal/fleet"
)

// State is an immutable snapshot published to clients. Version increases
// whenever anything visible changes.
type State struct {
	Version   uint64              `json:"version"`
	Updated   time.Time           `json:"updated"`
	Runner    *fleet.RunnerConfig `json:"runner,omitempty"`
	RunnerDir string              `json:"runner_dir"`
	Listener  fleet.ListenerState `json:"listener"`
	Processes fleet.Processes     `json:"processes"`
	// Pending is a deferred runner operation waiting for the current job to
	// finish: "", "stop" or "restart".
	Pending  string      `json:"pending,omitempty"`
	Jobs     []JobView   `json:"jobs"` // newest first
	Org      *OrgState   `json:"org,omitempty"`
	Queue    *QueueState `json:"queue,omitempty"` // jobs waiting for this runner (nil: queue polling off)
	Warnings []string    `json:"warnings,omitempty"`
	Notice   *Notice     `json:"notice,omitempty"` // result of the last action
}

// JobView is a job plus derived links.
type JobView struct {
	*fleet.Job
	HTMLURL string        `json:"html_url"`
	RunURL  string        `json:"run_url"`
	Usage   *UsageSummary `json:"usage,omitempty"` // finished jobs measured by cothon
}

// RunnerStatus summarizes the runner for headers and icons.
func (s *State) RunnerStatus() string {
	switch {
	case !s.Processes.Running():
		return "stopped"
	case s.Pending != "":
		return "stopping"
	case s.Processes.Busy():
		return "busy"
	case s.Listener.Event == fleet.ListenerListening:
		return "idle"
	default:
		return "starting"
	}
}

// RunningJob returns the job currently executing, if any.
func (s *State) RunningJob() *JobView {
	if len(s.Jobs) > 0 && s.Jobs[0].Status == fleet.StatusRunning {
		return &s.Jobs[0]
	}
	return nil
}

// OrgState is the optional organization-wide view.
type OrgState struct {
	Name          string    `json:"name"`
	Updated       time.Time `json:"updated,omitzero"`
	Warning       string    `json:"warning,omitempty"`
	Runs          []OrgRun  `json:"runs"` // queued / in-progress runs, newest first
	RateRemaining int       `json:"rate_remaining"`
}

type OrgRun struct {
	Repo      string    `json:"repo"`
	Workflow  string    `json:"workflow"`
	RunID     string    `json:"run_id"`
	RunNumber int       `json:"run_number"`
	Attempt   int       `json:"run_attempt"`
	Status    string    `json:"status"` // queued | in_progress | …
	Branch    string    `json:"branch"`
	Event     string    `json:"event"`
	Actor     string    `json:"actor"`
	HTMLURL   string    `json:"html_url"`
	Created   time.Time `json:"created"`
	Jobs      []OrgJob  `json:"jobs"`
}

type OrgJob struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion,omitempty"`
	RunnerName string    `json:"runner_name,omitempty"`
	Labels     []string  `json:"labels"`
	HTMLURL    string    `json:"html_url"`
	Started    time.Time `json:"started,omitzero"`
	// OnThisRunner: running here, or queued with labels this runner serves.
	OnThisRunner bool `json:"on_this_runner"`
}

// Notice is feedback for the last action.
type Notice struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
}

// Action is a request from a client.
type Action struct {
	Kind  string `json:"kind"`
	Repo  string `json:"repo,omitempty"`
	RunID string `json:"run_id,omitempty"`
	JobID string `json:"job_id,omitempty"` // REST job id (check_run_id)
	Force bool   `json:"force,omitempty"`  // runner stop/restart even if a job is running
}

const (
	ActRerunJob      = "rerun-job"
	ActRerunFailed   = "rerun-failed"
	ActRerunRun      = "rerun-run"
	ActCancelRun     = "cancel-run"
	ActRunnerStart   = "runner-start"
	ActRunnerStop    = "runner-stop"
	ActRunnerRestart = "runner-restart"
	ActRunnerAbort   = "runner-abort-pending"
)

// Backend is what UIs talk to: the in-process Engine or a remote daemon.
type Backend interface {
	State(ctx context.Context) (*State, error)
	// Watch delivers a value whenever the state may have changed. The channel
	// is closed when ctx ends or the backend goes away.
	Watch(ctx context.Context) <-chan struct{}
	Do(ctx context.Context, a Action) (string, error)
	// Usage is the live CPU/memory of the runner and its running job.
	Usage(ctx context.Context) (*Usage, error)
}
