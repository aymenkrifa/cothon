package runnerctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The unit cothon installs must be valid systemd syntax.
func TestUnitFileVerifies(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not available")
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "actions-runner")
	os.MkdirAll(runner, 0o755)
	os.WriteFile(filepath.Join(runner, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	unit := filepath.Join(dir, ServiceName)
	os.WriteFile(unit, []byte(unitFile(runner)), 0o644)
	out, err := exec.Command("systemd-analyze", "--user", "verify", unit).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze: %v\n%s", err, out)
	}
	for _, want := range []string{"KillSignal=SIGINT", "Restart=always", "WantedBy=default.target", "ExecStart=" + filepath.Join(runner, "run.sh")} {
		if !strings.Contains(unitFile(runner), want) {
			t.Errorf("unit lacks %q", want)
		}
	}
}
