package fleet

import (
	"path/filepath"
	"strings"
)

// Processes are the live runner processes for one runner directory.
type Processes struct {
	ListenerPID int `json:"listener_pid,omitempty"`
	WorkerPID   int `json:"worker_pid,omitempty"`
	// SupervisorPID is run.sh / run-helper.sh. It stays alive while the
	// listener restarts (self-update, retryable errors), so "no listener but
	// a supervisor" means "restarting", not "stopped".
	SupervisorPID int `json:"supervisor_pid,omitempty"`
	// ListenerLog is the Runner_*.log the live listener has open. A failed
	// second listener (e.g. a session conflict) also creates a newer
	// Runner_*.log, so "newest file" is not a reliable choice. Empty when it
	// can't be told (another user's process, or macOS): callers then fall
	// back to the newest Runner_*.log.
	ListenerLog string `json:"listener_log,omitempty"`
	// Listeners counts live listener processes; more than one means a
	// session conflict is in progress.
	Listeners int `json:"listeners,omitempty"`
}

func (p Processes) Running() bool { return p.ListenerPID != 0 }
func (p Processes) Busy() bool    { return p.WorkerPID != 0 }

// Alive reports whether anything of the runner is up (listener or its supervisor).
func (p Processes) Alive() bool { return p.ListenerPID != 0 || p.SupervisorPID != 0 }

type procInfo struct {
	pid, ppid int
	start     uint64 // clock ticks since boot (Linux) or unix seconds (macOS); only compared
}

// runnerDirs is runnerDir, cleaned, plus its symlink-resolved form if different.
func runnerDirs(runnerDir string) []string {
	dirs := []string{filepath.Clean(runnerDir)}
	if real, err := filepath.EvalSymlinks(runnerDir); err == nil && real != dirs[0] {
		dirs = append(dirs, real)
	}
	return dirs
}

// pickListener chooses the live listener: with two listeners (a conflicting
// second start), the worker's parent, else the oldest.
func pickListener(listeners []procInfo, worker procInfo) procInfo {
	best := listeners[0]
	for _, l := range listeners[1:] {
		if (worker.pid != 0 && l.pid == worker.ppid) || (best.pid != worker.ppid && l.start < best.start) {
			best = l
		}
	}
	return best
}

func isShell(p string) bool {
	switch filepath.Base(p) {
	case "bash", "sh", "dash", "zsh":
		return true
	}
	return false
}

func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(path, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
