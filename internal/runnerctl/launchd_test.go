package runnerctl

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchAgentPlist(t *testing.T) {
	dir := "/Users/me/a&b <runner>"
	p := launchAgentPlist(dir, dir+"/run.sh", "/Users/me/.local/state/cothon/runner.out")
	// Well-formed XML, with the paths escaped.
	dec := xml.NewDecoder(strings.NewReader(p))
	dec.Strict = true
	var strs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("plist is not well-formed: %v\n%s", err, p)
			}
			break
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	want := map[string]bool{LaunchdLabel: false, dir: false, dir + "/run.sh": false, "RUNNER_MANUALLY_TRAP_SIG": false, "KeepAlive": false}
	for _, s := range strs {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("plist lacks %q", s)
		}
	}
	if strings.Contains(launchAgentPlist(dir, dir+"/run.sh", ""), "StandardOutPath") {
		t.Error("no log file must mean no StandardOutPath")
	}
}

// On macOS, plutil checks the plist for real.
func TestLaunchAgentPlistLints(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	f := filepath.Join(t.TempDir(), LaunchdLabel+".plist")
	os.WriteFile(f, []byte(launchAgentPlist("/tmp/r", "/tmp/r/run.sh", "/tmp/r.out")), 0o644)
	if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v\n%s", err, out)
	}
}

func TestParseLaunchctlPID(t *testing.T) {
	out := `gui/501/com.github.aymenkrifa.cothon.runner = {
	active count = 1
	path = /Users/me/Library/LaunchAgents/com.github.aymenkrifa.cothon.runner.plist
	state = running

	program = /Users/me/actions-runner/run.sh
	environment = {
		RUNNER_MANUALLY_TRAP_SIG => 1
	}
	pid = 4242
	immediate reason = speculative
}
`
	if got := parseLaunchctlPID(out); got != 4242 {
		t.Errorf("pid = %d", got)
	}
	if got := parseLaunchctlPID("state = not running\n"); got != 0 {
		t.Errorf("pid of stopped job = %d", got)
	}
}

func TestDescends(t *testing.T) {
	ppid := parsePPIDs("  1 0\n 10 1\n 11 10\n 12 11\n 20 1\n bad\n 30 30\n")
	if !descends(12, 10, ppid) || !descends(10, 10, ppid) {
		t.Error("12 and 10 descend from 10")
	}
	if descends(20, 10, ppid) || descends(0, 10, ppid) || descends(30, 10, ppid) {
		t.Error("20, 0 and a self-parented pid don't descend from 10")
	}
}
