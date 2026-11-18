// Package browser opens links in the user's browser of choice.
package browser

import (
	"os/exec"
	"syscall"
)

// Open opens url with command ("" = the desktop default via xdg-open, or
// open(1) on macOS, else a command line such as "firefox" or
// "google-chrome --new-window"). The browser runs in its own session, so
// Ctrl-C in cothon's terminal does not reach it, and cothon does not wait
// for it.
func Open(url, command string) error {
	argv := openArgv(command, url)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it if it exits while we run
	return nil
}
