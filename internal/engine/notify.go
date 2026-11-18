package engine

// osascriptArgv is the osascript command line for a macOS notification. The
// text is passed as arguments to the script's run handler, never spliced
// into AppleScript source, so it needs no escaping. The first argument is a
// fixed word so that a title starting with "-" isn't taken for an option.
func osascriptArgv(title, body string, critical bool) []string {
	display := "display notification (item 3 of argv) with title (item 2 of argv)"
	if critical {
		display += ` sound name "Basso"`
	}
	return []string{"osascript", "-e", "on run argv", "-e", display, "-e", "end run", "cothon", title, body}
}
