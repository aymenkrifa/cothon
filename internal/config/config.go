// Package config loads cothon's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// RunnerDir is the self-hosted runner directory (contains .runner and _diag).
	RunnerDir string `toml:"runner_dir"`
	// Listen is the daemon address. Only loopback addresses are accepted.
	Listen string `toml:"listen"`
	// MaxJobs caps how many recent jobs are kept in the API/UI state.
	MaxJobs int `toml:"max_jobs"`
	// Notify: "finished" (every job), "failed" (failed/canceled/abandoned only) or "none".
	Notify string `toml:"notify"`
	// GH is the GitHub CLI binary used for actions and for the API token.
	GH string `toml:"gh"`
	// Browser opens the web UI: "" = desktop default (xdg-open, or open on macOS), "none" =
	// only print the link, or a command such as "firefox".
	Browser string `toml:"browser"`
	// RunnerStart: "setsid" (detached, inherits the daemon's environment) or
	// "systemd" (transient `systemd-run --user` unit, survives logout of the shell; Linux only).
	RunnerStart string `toml:"runner_start"`

	Org   OrgConfig   `toml:"org"`
	Queue QueueConfig `toml:"queue"`
}

// QueueConfig: jobs waiting for this runner, asked from GitHub for the repos
// the runner worked on in the last 7 days (conditional requests).
type QueueConfig struct {
	Enabled      bool     `toml:"enabled"`
	PollInterval Duration `toml:"poll_interval"`
}

type OrgConfig struct {
	Enabled bool `toml:"enabled"`
	// Name of the organization; defaults to the owner in the runner's gitHubUrl.
	Name string `toml:"name"`
	// PollInterval between org polls (e.g. "60s").
	PollInterval Duration `toml:"poll_interval"`
	// RecentWindow limits polling to repos pushed within this window (e.g. "24h").
	RecentWindow Duration `toml:"recent_window"`
	// MaxRepos caps how many recently pushed repos are polled per cycle.
	MaxRepos int `toml:"max_repos"`
	// RunnerLabels decide which queued jobs this runner can pick up
	// (default: self-hosted, linux, x64 plus the runner name).
	RunnerLabels []string `toml:"runner_labels"`
}

// Duration is a time.Duration that reads from TOML strings like "60s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		RunnerDir:   filepath.Join(home, "actions-runner"),
		Listen:      "127.0.0.1:7777",
		MaxJobs:     200,
		Notify:      "finished",
		GH:          "gh",
		RunnerStart: "setsid",
		Queue:       QueueConfig{Enabled: true, PollInterval: Duration{30 * time.Second}},
		Org: OrgConfig{
			PollInterval: Duration{60 * time.Second},
			RecentWindow: Duration{24 * time.Hour},
			MaxRepos:     15,
		},
	}
}

// Dir is $XDG_CONFIG_HOME/cothon (or ~/.config/cothon).
func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "cothon")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cothon")
}

// Path is the config file location, overridable with COTHON_CONFIG.
func Path() string {
	if p := os.Getenv("COTHON_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.toml")
}

// Load reads the config file if present; a missing file yields the defaults.
func Load() (Config, error) {
	c := Default()
	path := Path()
	md, err := toml.DecodeFile(path, &c)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err == nil {
		if keys := md.Undecoded(); len(keys) > 0 {
			return c, fmt.Errorf("%s: unknown setting %q", path, keys[0].String())
		}
	}
	if d := os.Getenv("COTHON_RUNNER_DIR"); d != "" {
		c.RunnerDir = d
	}
	c.RunnerDir = expandHome(c.RunnerDir)
	// Process detection compares absolute paths.
	if abs, err := filepath.Abs(c.RunnerDir); err == nil {
		c.RunnerDir = abs
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	switch c.Notify {
	case "finished", "failed", "none":
	default:
		return fmt.Errorf("notify: want finished|failed|none, got %q", c.Notify)
	}
	switch c.RunnerStart {
	case "setsid", "systemd":
	default:
		return fmt.Errorf("runner_start: want setsid|systemd, got %q", c.RunnerStart)
	}
	if c.MaxJobs <= 0 || c.MaxJobs > 2000 {
		return fmt.Errorf("max_jobs must be between 1 and 2000")
	}
	if strings.TrimSpace(c.GH) == "" {
		return fmt.Errorf("gh must name the GitHub CLI binary")
	}
	if c.Org.RecentWindow.Duration <= 0 {
		return fmt.Errorf("org.recent_window must be positive (e.g. \"24h\")")
	}
	if c.Org.PollInterval.Duration < 15*time.Second {
		return fmt.Errorf("org.poll_interval must be at least 15s")
	}
	if c.Queue.PollInterval.Duration < 10*time.Second {
		return fmt.Errorf("queue.poll_interval must be at least 10s")
	}
	if c.Org.MaxRepos <= 0 || c.Org.MaxRepos > 50 {
		return fmt.Errorf("org.max_repos must be between 1 and 50")
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Example is a commented starter config, written by `cothon config init`.
const Example = `# cothon configuration

# Self-hosted runner directory (contains .runner and _diag/).
runner_dir = "~/actions-runner"

# Daemon address. Loopback only.
listen = "127.0.0.1:7777"

# How many recent jobs to keep in the UI.
max_jobs = 200

# Desktop notifications from the daemon: "finished", "failed" or "none".
notify = "finished"

# GitHub CLI binary (used for rerun/cancel and for the API token).
gh = "gh"

# Browser for "cothon web": "" = your default browser, "none" = only
# print the sign-in link, or a command, e.g. "firefox" or "google-chrome --new-window".
browser = ""

# How "runner start" launches run.sh:
#   "setsid"  - detached process that inherits the daemon's environment
#   "systemd" - transient "systemd-run --user" unit (logs: journalctl --user -u cothon-runner; Linux only)
runner_start = "setsid"

[queue]
# Show jobs waiting for this runner. Asks GitHub about the repos this runner
# worked on in the last 7 days; unchanged answers are free (HTTP 304).
enabled = true
poll_interval = "30s"

[org]
# Poll the organization for queued / in-progress runs (jobs waiting for a runner).
# If the token can't read the org, a warning is shown and cothon stays local-only.
enabled = false
# name = "my-org"        # defaults to the owner in the runner's gitHubUrl
poll_interval = "60s"
recent_window = "24h"    # only poll repos pushed within this window
max_repos = 15
# runner_labels = ["self-hosted", "linux", "x64"]   # used to spot queued jobs for this runner
`
