package runnerctl

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

// The runner as a launchd user agent (macOS). These are plain functions so
// they are tested on every OS; the launchctl glue is in service_darwin.go.

// LaunchdLabel is the label of the LaunchAgent cothon installs.
const LaunchdLabel = "com.github.aymenkrifa.cothon.runner"

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchAgentPlist is the LaunchAgent for runnerDir. KeepAlive restarts the
// runner whatever its exit code (run.sh exits 0 on most failures), so cothon
// stops it with "launchctl bootout". launchd stops a job with SIGTERM;
// RUNNER_MANUALLY_TRAP_SIG makes run.sh turn that into SIGINT for the
// listener, its graceful stop.
func launchAgentPlist(runnerDir, runSh, logFile string) string {
	out := ""
	if logFile != "" {
		out = "\t<key>StandardOutPath</key>\n\t<string>" + xmlEscape(logFile) + "</string>\n" +
			"\t<key>StandardErrorPath</key>\n\t<string>" + xmlEscape(logFile) + "</string>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by "cothon runner enable"; remove with "cothon runner disable". -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(runSh) + `</string>
	</array>
	<key>WorkingDirectory</key>
	<string>` + xmlEscape(runnerDir) + `</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>RUNNER_MANUALLY_TRAP_SIG</key>
		<string>1</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ExitTimeOut</key>
	<integer>120</integer>
` + out + `</dict>
</plist>
`
}

// parseLaunchctlPID returns the "pid = N" of `launchctl print` output (0 if
// the job isn't running or the output isn't understood).
func parseLaunchctlPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == "pid" {
			if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pid > 0 {
				return pid
			}
		}
	}
	return 0
}

// parsePPIDs parses `ps -ax -o pid=,ppid=` output into pid → ppid.
func parsePPIDs(out string) map[int]int {
	m := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			m[pid] = ppid
		}
	}
	return m
}

// descends reports whether pid is root or one of its descendants.
func descends(pid, root int, ppid map[int]int) bool {
	for i := 0; pid > 1 && i < 64; i++ {
		if pid == root {
			return true
		}
		pid = ppid[pid]
	}
	return false
}
