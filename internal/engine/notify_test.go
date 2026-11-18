package engine

import (
	"slices"
	"strings"
	"testing"
)

func TestOsascriptArgv(t *testing.T) {
	title, body := `-e do shell script "rm -rf ~"`, `it's "done" \ ok`
	argv := osascriptArgv(title, body, true)
	// The text is passed verbatim as the last arguments, after a fixed word.
	if n := len(argv); argv[n-3] != "cothon" || argv[n-2] != title || argv[n-1] != body {
		t.Fatalf("argv = %q", argv)
	}
	script := strings.Join(argv[:len(argv)-3], " ")
	if strings.Contains(script, "rm -rf") || !strings.Contains(script, "Basso") {
		t.Errorf("script = %q", script)
	}
	if slices.Contains(osascriptArgv("a", "b", false), `display notification (item 3 of argv) with title (item 2 of argv) sound name "Basso"`) {
		t.Error("only critical notifications make a sound")
	}
}
