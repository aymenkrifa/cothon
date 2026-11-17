// Package fleet reads a GitHub Actions self-hosted runner's _diag logs and
// turns them into job records. It is the shared engine used by the daemon,
// the TUI and (indirectly) the GNOME extension.
package fleet

import (
	"fmt"
	"time"
)

// Status is the lifecycle state of a job as seen from the local runner.
type Status string

const (
	StatusRunning   Status = "running"   // worker log has no completion yet
	StatusCompleted Status = "completed" // worker log reached "Job completed."
	StatusAbandoned Status = "abandoned" // never completed and a newer job started (worker crashed / runner killed)
)

// Job is one job execution on the local runner (one Worker_*.log file).
type Job struct {
	// Identity
	ID         string `json:"id"`           // runner job GUID (jobId); links Runner_*.log and Worker_*.log
	CheckRunID string `json:"check_run_id"` // REST API job id (…/actions/jobs/{id}); kept as string, the log writes it as a float
	Name       string `json:"name"`         // jobDisplayName, e.g. "Test (3/4)"

	// Workflow context (contextData.github)
	Repository string `json:"repository"` // owner/name
	Workflow   string `json:"workflow"`
	RunID      string `json:"run_id"`
	RunNumber  string `json:"run_number"`
	RunAttempt string `json:"run_attempt"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	Actor      string `json:"actor"`
	Event      string `json:"event"`
	ServerURL  string `json:"server_url"`

	// Outcome
	Status   Status    `json:"status"`
	Result   string    `json:"result,omitempty"` // Succeeded | Failed | Canceled | … (verbatim from the runner)
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`

	LogFile string `json:"log_file"`

	// hasHeader: the log contains the job message. The runner starts a new
	// Worker_*.log page after ~8 MB; later pages have no header and are
	// merged into the job they continue.
	hasHeader bool
	// startupOnly: the worker started but died before receiving a job.
	startupOnly bool
}

// Duration is the elapsed time of the job; for a running job it is measured up to now.
func (j *Job) Duration(now time.Time) time.Duration {
	if j.Started.IsZero() {
		return 0
	}
	end := j.Finished
	if end.IsZero() {
		end = now
	}
	return end.Sub(j.Started)
}

// RunURL links to the workflow run on GitHub, or "" if the context is unknown.
func (j *Job) RunURL() string {
	if j.Repository == "" || j.RunID == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%s", j.serverURL(), j.Repository, j.RunID)
}

// HTMLURL links to the job page on GitHub, falling back to the run page.
func (j *Job) HTMLURL() string {
	run := j.RunURL()
	if run == "" || j.CheckRunID == "" {
		return run
	}
	return run + "/job/" + j.CheckRunID
}

func (j *Job) serverURL() string {
	if j.ServerURL != "" {
		return j.ServerURL
	}
	return "https://github.com"
}
