package browser

import "strings"

func openArgv(command, url string) []string {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		argv = []string{"xdg-open"}
	}
	return append(argv, url)
}
