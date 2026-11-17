// Package browser opens links in the user's browser of choice.
package browser

import (
	"os/exec"
	"strings"
	"syscall"
)

// Open opens url with command ("" = the desktop default via xdg-open, else a
// command line such as "firefox" or "google-chrome --new-window"). The
// browser runs in its own session, so Ctrl-C in cothon's terminal does
// not reach it, and cothon does not wait for it.
func Open(url, command string) error {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		argv = []string{"xdg-open"}
	}
	cmd := exec.Command(argv[0], append(argv[1:], url)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it if it exits while we run
	return nil
}
