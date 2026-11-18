package browser

import "os/exec"

func openArgv(command, url string) []string {
	return macArgv(command, url, exec.LookPath)
}
