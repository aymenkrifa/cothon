package browser

import "strings"

// macApps maps browser commands as written on Linux to macOS app names.
var macApps = map[string]string{
	"firefox":        "Firefox",
	"google-chrome":  "Google Chrome",
	"chrome":         "Google Chrome",
	"chromium":       "Chromium",
	"brave":          "Brave Browser",
	"brave-browser":  "Brave Browser",
	"microsoft-edge": "Microsoft Edge",
	"edge":           "Microsoft Edge",
	"safari":         "Safari",
	"vivaldi":        "Vivaldi",
	"opera":          "Opera",
}

// macArgv is the command line that opens url on macOS: "" is open(1) (the
// default browser); a command found on $PATH runs as given; otherwise the
// first word names an app ("firefox" → open -a Firefox), with any further
// words passed to it ("google-chrome --new-window").
func macArgv(command, url string, lookPath func(string) (string, error)) []string {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return []string{"open", url}
	}
	if _, err := lookPath(argv[0]); err == nil {
		return append(argv, url)
	}
	app, ok := macApps[strings.ToLower(argv[0])]
	if !ok {
		app = argv[0] // e.g. "Arc"; open -a matches app names
	}
	out := []string{"open", "-a", app, url}
	if len(argv) > 1 {
		out = append(append(out, "--args"), argv[1:]...)
	}
	return out
}
