// Package runnerctl starts and stops a self-hosted runner that is run via
// run.sh (not installed as a service).
//
// Semantics, from run.sh / run-helper.sh (runner 2.338.x):
//   - SIGINT to Runner.Listener makes it exit 0, and run.sh then exits too.
//     Stopping only run.sh would leave the listener running.
//   - Exit codes 2/3/4 make run.sh relaunch the listener (self-update), so
//     the listener PID can change: always look it up by name.
//   - Exit code 5 is a session conflict: a second listener for the same
//     runner. Never start one while a listener is alive.
package runnerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aymenkrifa/cothon/internal/fleet"
)

// Unit is the transient systemd unit name used with Mode "systemd".
const Unit = "cothon-runner"

var ErrAlreadyRunning = errors.New("runner listener is already running")

type Controller struct {
	RunnerDir string
	Mode      string // "setsid" or "systemd"
	// LogFile receives run.sh output in setsid mode.
	LogFile string
	// Find returns live processes; defaults to fleet.FindProcesses.
	Find func() fleet.Processes
}

// Procs returns the live runner processes.
func (c *Controller) Procs() fleet.Processes {
	if c.Find != nil {
		return c.Find()
	}
	return fleet.FindProcesses(c.RunnerDir)
}

// Start launches run.sh detached from the caller so it survives the daemon,
// the terminal and the shell that started cothon.
func (c *Controller) Start() (string, error) {
	if p := c.Procs(); p.Alive() {
		return "", ErrAlreadyRunning // includes run.sh between listener restarts
	}
	runSh := filepath.Join(c.RunnerDir, "run.sh")
	if _, err := os.Stat(runSh); err != nil {
		return "", err
	}
	if ServiceInstalled() {
		if err := systemctl("start", ServiceName); err != nil {
			return "", err
		}
		return "started the " + ServiceName + " systemd user service", nil
	}

	if c.Mode == "systemd" {
		// A failed previous unit with the same name would block a new one.
		exec.Command("systemctl", "--user", "reset-failed", Unit).Run()
		cmd := exec.Command("systemd-run", "--user", "--unit="+Unit, "--collect",
			"--working-directory="+c.RunnerDir, "--description=GitHub Actions runner (cothon)", runSh)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("systemd-run: %v: %s", err, out)
		}
		return "started as systemd user unit " + Unit + " (logs: journalctl --user -u " + Unit + ")", nil
	}

	if err := os.MkdirAll(filepath.Dir(c.LogFile), 0o700); err != nil {
		return "", err
	}
	// Keep one previous log; never let it grow without bound.
	if fi, err := os.Stat(c.LogFile); err == nil && fi.Size() > 5<<20 {
		os.Rename(c.LogFile, c.LogFile+".1")
	}
	logf, err := os.OpenFile(c.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return "", err
	}
	defer devnull.Close()

	cmd := exec.Command(runSh)
	cmd.Dir = c.RunnerDir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // own session: no SIGHUP from our terminal
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pid := cmd.Process.Pid
	// Reap it if it exits while we're alive; otherwise it's re-parented to init.
	go cmd.Wait()
	return fmt.Sprintf("started run.sh (pid %d, output: %s)", pid, c.LogFile), nil
}

// Interrupt sends SIGINT to the listener (graceful stop). If a job is running
// GitHub marks it canceled; callers should prefer stopping when idle.
func (c *Controller) Interrupt() error {
	p := c.Procs()
	if !p.Running() {
		return errors.New("runner listener is not running")
	}
	if InService(p.ListenerPID) {
		// Through systemd, so Restart=always doesn't bring it back; the unit
		// sends SIGINT (KillSignal) to the runner's processes.
		return systemctl("stop", "--no-block", ServiceName)
	}
	return syscall.Kill(p.ListenerPID, syscall.SIGINT)
}

// WaitStopped polls until neither the listener nor run.sh is alive, the
// timeout expires, or ctx ends.
func (c *Controller) WaitStopped(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !c.Procs().Alive() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return !c.Procs().Alive()
}
