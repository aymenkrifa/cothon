package runnerctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The runner as a launchd user agent (~/Library/LaunchAgents): loaded at
// login, kept alive if it exits. Stopping goes through "launchctl bootout",
// which unloads the job until the next login or "cothon runner start".

// ServiceName names the service in messages.
const ServiceName = LaunchdLabel

const serviceDescription = "launchd agent " + LaunchdLabel

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func jobTarget() string { return guiDomain() + "/" + LaunchdLabel }

func launchctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// jobLoaded reports whether launchd has the job, and its `print` output.
func jobLoaded() (string, bool) {
	out, err := launchctl("print", jobTarget())
	return out, err == nil
}

// ServiceInstalled reports whether the plist exists (start goes through it).
func ServiceInstalled() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

// InService reports whether pid is the launchd job's process or one of its
// descendants. It asks launchd by label, so it also works after "disable"
// removed the plist while the job was still running.
func InService(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, ok := jobLoaded()
	if !ok {
		return false
	}
	root := parseLaunchctlPID(out)
	if root <= 0 {
		return false
	}
	if pid == root {
		return true
	}
	ps, err := exec.Command("/bin/ps", "-ax", "-o", "pid=,ppid=").Output()
	if err != nil {
		return false
	}
	return descends(pid, root, parsePPIDs(string(ps)))
}

func startService() (string, error) {
	var err error
	// Twice: right after a stop, the job may still be on its way out
	// (bootstrap: "already loaded"; kickstart: gone meanwhile).
	for try := 0; try < 2; try++ {
		if try > 0 {
			time.Sleep(2 * time.Second)
		}
		if _, loaded := jobLoaded(); loaded {
			_, err = launchctl("kickstart", jobTarget())
		} else {
			_, err = launchctl("bootstrap", guiDomain(), plistPath())
		}
		if err == nil {
			return "started the " + serviceDescription, nil
		}
	}
	return "", err
}

// stopService unloads the job (KeepAlive would restart a runner that merely
// exits). launchctl may wait for the runner to exit, so it isn't waited for
// beyond a few seconds; callers poll for the processes to go away.
func stopService() error {
	cmd := exec.Command("launchctl", "bootout", jobTarget())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("launchctl bootout %s: %v: %s", jobTarget(), err, strings.TrimSpace(out.String()))
		}
	case <-time.After(3 * time.Second):
	}
	return nil
}

func (c *Controller) startTransientUnit(string) (string, error) {
	return "", errors.New(`runner_start = "systemd" needs systemd (Linux); use runner_start = "setsid" on macOS, or "cothon runner enable" for a launchd agent`)
}

// Enable installs the LaunchAgent. It loads (and so starts) it right away
// only if no runner is running; a runner started some other way keeps running
// and the agent takes over at the next login (or "cothon runner restart"
// moves it now).
func (c *Controller) Enable(atBoot bool) (string, error) {
	if atBoot {
		return "", errors.New("--at-boot is not supported on macOS: a LaunchAgent runs while you are logged in (for boot without login, install the runner as a LaunchDaemon with its own svc.sh)")
	}
	runSh := filepath.Join(c.RunnerDir, "run.sh")
	if _, err := os.Stat(runSh); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return "", err
	}
	if c.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(c.LogFile), 0o700); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(plistPath(), []byte(launchAgentPlist(c.RunnerDir, runSh, c.LogFile)), 0o644); err != nil {
		return "", err
	}
	launchctl("enable", jobTarget()) // clear a leftover "disabled" override; harmless otherwise
	msg := "enabled: the runner starts when you log in and restarts if it crashes"
	p := c.Procs()
	switch {
	case !p.Alive():
		if _, loaded := jobLoaded(); loaded {
			launchctl("bootout", jobTarget()) // a stale job: reload it from the new plist
		}
		if _, err := launchctl("bootstrap", guiDomain(), plistPath()); err != nil {
			return "", err
		}
		msg += "; started now"
	case !InService(p.ListenerPID) && !InService(p.SupervisorPID):
		msg += "\nthe runner you started earlier keeps running; run \"cothon runner restart\" to move it under the service now"
	}
	return msg, nil
}

// Disable removes the LaunchAgent. A runner currently running under it keeps
// running until it is stopped.
func (c *Controller) Disable() (string, error) {
	if !ServiceInstalled() {
		return "", errors.New("the runner service is not installed")
	}
	if err := os.Remove(plistPath()); err != nil {
		return "", err
	}
	msg := "disabled: the runner no longer starts at login or restarts by itself"
	p := c.Procs()
	if InService(p.ListenerPID) || InService(p.SupervisorPID) {
		msg = "disabled: the runner no longer starts at login"
		msg += "\nit is still running under launchd (which restarts it until stopped); \"cothon runner stop\" stops it"
	} else if _, loaded := jobLoaded(); loaded {
		launchctl("bootout", jobTarget()) // nothing of it runs: unload it now
	}
	return msg, nil
}
