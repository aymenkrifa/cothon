package fleet

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// runPS runs ps with a C locale and a timeout; "" on any failure.
func runPS(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// FindProcesses lists processes with ps(1) (macOS has no /proc) and finds
// Runner.Listener / Runner.Worker processes (and their run.sh supervisor)
// started from runnerDir, matched by their absolute executable path.
//
// ListenerLog stays empty: the open files of a process aren't cheaply
// readable here, so callers use the newest Runner_*.log.
func FindProcesses(runnerDir string) Processes {
	out := runPS("-axww", "-o", "pid=,ppid=,etime=,command=")
	if out == "" {
		return Processes{}
	}
	ps, listeners, worker := classifyPS(parsePSRows(out), runnerDirs(runnerDir), uint64(time.Now().Unix()))
	ps.WorkerPID = worker.pid
	ps.Listeners = len(listeners)
	if len(listeners) > 0 {
		ps.ListenerPID = pickListener(listeners, worker).pid
	}
	return ps
}
