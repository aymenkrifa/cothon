package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aymenkrifa/cothon/internal/gh"
)

// Do implements Backend. Every action is checked against the current state:
// only runs and jobs cothon is showing can be acted on.
func (e *Engine) Do(ctx context.Context, a Action) (string, error) {
	msg, err := e.do(ctx, a)
	n := &Notice{At: time.Now(), OK: err == nil, Message: msg}
	if err != nil {
		n.Message = err.Error()
	}
	e.mu.Lock()
	e.notice = n
	e.mu.Unlock()
	e.kick()
	if err == nil && a.Repo != "" {
		e.pokeOrg()
	}
	return msg, err
}

func (e *Engine) do(ctx context.Context, a Action) (string, error) {
	switch a.Kind {
	case ActRerunJob:
		if !gh.ValidRepo(a.Repo) || !gh.ValidID(a.JobID) || !e.knownJob(a.Repo, a.JobID) {
			return "", errors.New("unknown job")
		}
		if err := e.cli.Post(ctx, fmt.Sprintf("repos/%s/actions/jobs/%s/rerun", a.Repo, a.JobID)); err != nil {
			return "", fmt.Errorf("rerun job: %w", err)
		}
		return "rerun requested for job " + a.JobID, nil

	case ActRerunFailed, ActRerunRun, ActCancelRun:
		if !gh.ValidRepo(a.Repo) || !gh.ValidID(a.RunID) || !e.knownRun(a.Repo, a.RunID) {
			return "", errors.New("unknown run")
		}
		endpoint, done := runEndpoint(a.Kind)
		if err := e.cli.Post(ctx, fmt.Sprintf("repos/%s/actions/runs/%s/%s", a.Repo, a.RunID, endpoint)); err != nil {
			return "", fmt.Errorf("%s: %w", a.Kind, err)
		}
		return fmt.Sprintf("%s for %s run %s", done, a.Repo, a.RunID), nil

	case ActRunnerStart:
		e.mu.Lock()
		restarting := e.restarting
		e.mu.Unlock()
		if restarting {
			return "", errors.New("a restart is already in progress")
		}
		return e.startRunner()

	case ActRunnerStop, ActRunnerRestart:
		op := "stop"
		if a.Kind == ActRunnerRestart {
			op = "restart"
		}
		e.mu.Lock()
		pending, signaled, restarting := e.pending, !e.signaled.IsZero(), e.restarting
		e.mu.Unlock()
		switch {
		case restarting:
			return "", errors.New("a restart is already in progress")
		case signaled:
			return "", errors.New("the runner is already stopping")
		}
		p := e.ctl.Procs() // live, not the last snapshot
		if !p.Alive() {
			if op == "restart" {
				return e.startRunner()
			}
			return "", errors.New("runner is not running")
		}
		if p.Busy() && !a.Force {
			e.mu.Lock()
			e.pending = op
			e.mu.Unlock()
			if pending != "" && pending != op {
				return fmt.Sprintf("changed: will %s (instead of %s) when the current job finishes", op, pending), nil
			}
			return fmt.Sprintf("will %s when the current job finishes", op), nil
		}
		if !p.Running() {
			return "", errors.New("the runner is restarting itself (self-update); try again in a few seconds")
		}
		if err := e.ctl.Interrupt(); err != nil {
			return "", err
		}
		e.mu.Lock()
		e.pending = ""
		if op == "stop" {
			e.pending, e.signaled = "stop", time.Now() // cleared once the processes are gone
		}
		e.mu.Unlock()
		if op == "restart" {
			e.mu.Lock()
			e.restarting = true
			e.mu.Unlock()
			go e.restartAfterStop()
			return "restarting runner", nil
		}
		if p.Busy() {
			return "stopping runner now; the running job will be canceled", nil
		}
		return "stopping runner", nil

	case ActRunnerAbort:
		e.mu.Lock()
		defer e.mu.Unlock()
		switch {
		case !e.signaled.IsZero():
			return "", errors.New("the runner was already signaled to stop")
		case e.pending == "":
			return "nothing pending", nil
		}
		was := e.pending
		e.pending = ""
		return "pending " + was + " canceled", nil
	}
	return "", fmt.Errorf("unknown action %q", a.Kind)
}

func runEndpoint(kind string) (endpoint, done string) {
	switch kind {
	case ActRerunFailed:
		return "rerun-failed-jobs", "rerun of failed jobs requested"
	case ActRerunRun:
		return "rerun", "rerun of all jobs requested"
	default:
		return "cancel", "cancel requested"
	}
}

func (e *Engine) knownJob(repo, jobID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.state.Jobs {
		if j.Repository == repo && j.CheckRunID == jobID {
			return true
		}
	}
	if e.org != nil {
		for _, r := range e.org.Runs {
			for _, j := range r.Jobs {
				if r.Repo == repo && j.ID == jobID {
					return true
				}
			}
		}
	}
	return false
}

func (e *Engine) knownRun(repo, runID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.queue != nil {
		for _, q := range e.queue.Jobs {
			if q.Repo == repo && q.RunID == runID {
				return true
			}
		}
	}
	for _, j := range e.state.Jobs {
		if j.Repository == repo && j.RunID == runID {
			return true
		}
	}
	if e.org != nil {
		for _, r := range e.org.Runs {
			if r.Repo == repo && r.RunID == runID {
				return true
			}
		}
	}
	return false
}

// stateDir is $XDG_STATE_HOME/cothon (or ~/.local/state/cothon).
func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "cothon")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "cothon")
}
