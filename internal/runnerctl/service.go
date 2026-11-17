package runnerctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// The runner as a systemd user service: started at login (or at boot with
// lingering), restarted if it crashes. run.sh exits 0 for most failures, so
// the unit restarts "always"; cothon's own stop goes through systemctl,
// which never triggers a restart.

// ServiceName is the systemd user unit cothon installs.
const ServiceName = "cothon-runner.service"

func unitPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user", ServiceName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", ServiceName)
}

// ServiceInstalled reports whether the unit file exists (start goes through it).
func ServiceInstalled() bool {
	_, err := os.Stat(unitPath())
	return err == nil
}

// InService reports whether pid runs inside the cothon-runner unit.
func InService(pid int) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	return err == nil && strings.Contains(string(b), "/"+ServiceName)
}

func unitFile(runnerDir string) string {
	return fmt.Sprintf(`# Installed by "cothon runner enable"; remove with "cothon runner disable".
[Unit]
Description=GitHub Actions runner (cothon)
After=network-online.target
Wants=network-online.target
# Don't spin forever if something is badly wrong (e.g. a session conflict).
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s
# SIGINT is the runner's graceful stop.
KillSignal=SIGINT
KillMode=control-group
TimeoutStopSec=120
# run.sh exits 0 on most failures, so restart unconditionally; stopping
# through systemctl (what cothon does) never restarts it.
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, runnerDir, filepath.Join(runnerDir, "run.sh"))
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Enable installs and enables the service. It starts it right away only if no
// runner is running; a runner started some other way keeps running and the
// service takes over at the next login (or "cothon runner restart" moves
// it now). atBoot also enables lingering, so it starts without logging in.
func (c *Controller) Enable(atBoot bool) (string, error) {
	if _, err := os.Stat(filepath.Join(c.RunnerDir, "run.sh")); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath()), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(unitPath(), []byte(unitFile(c.RunnerDir)), 0o644); err != nil {
		return "", err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return "", err
	}
	if err := systemctl("enable", ServiceName); err != nil {
		return "", err
	}
	msg := "enabled: the runner starts when you log in and restarts if it crashes"
	if atBoot {
		u, _ := user.Current()
		if out, err := exec.Command("loginctl", "enable-linger", u.Username).CombinedOutput(); err != nil {
			return "", fmt.Errorf("loginctl enable-linger: %v: %s", err, strings.TrimSpace(string(out)))
		}
		msg = "enabled: the runner starts at boot (no login needed) and restarts if it crashes"
	}
	p := c.Procs()
	switch {
	case !p.Alive():
		if err := systemctl("start", ServiceName); err != nil {
			return "", err
		}
		msg += "; started now"
	case !InService(p.ListenerPID) && !InService(p.SupervisorPID):
		msg += "\nthe runner you started earlier keeps running; run \"cothon runner restart\" to move it under the service now"
	}
	return msg, nil
}

// Disable stops auto-start and removes the unit file. A runner currently
// running under the service keeps running until it is stopped.
func (c *Controller) Disable() (string, error) {
	if !ServiceInstalled() {
		return "", errors.New("the runner service is not installed")
	}
	if err := systemctl("disable", ServiceName); err != nil {
		return "", err
	}
	if err := os.Remove(unitPath()); err != nil {
		return "", err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return "", err
	}
	msg := "disabled: the runner no longer starts at login or restarts by itself"
	p := c.Procs()
	if InService(p.ListenerPID) {
		msg += "\nit is still running; \"cothon runner stop\" stops it"
	}
	return msg, nil
}

// Manager describes who runs the runner right now.
func (c *Controller) Manager() string {
	p := c.Procs()
	switch {
	case !p.Alive():
		if ServiceInstalled() {
			return "stopped (service enabled: starts at login)"
		}
		return "stopped"
	case InService(p.ListenerPID) || InService(p.SupervisorPID):
		return "systemd user service " + ServiceName + " (restarts if it crashes)"
	case ServiceInstalled():
		return "started outside the service (service enabled: takes over at next login)"
	default:
		return "run.sh, started manually or by cothon (no auto-restart; \"cothon runner enable\" adds it)"
	}
}
