package fleet

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FindProcesses scans /proc for Runner.Listener / Runner.Worker processes
// (and their run.sh supervisor) started from runnerDir. Processes of any user
// are detected, so a runner installed as another user's service is still seen
// as running; controlling it is a separate, permission-checked step.
func FindProcesses(runnerDir string) Processes {
	var ps Processes
	dirs := runnerDirs(runnerDir)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ps
	}
	var listeners []procInfo
	var worker procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		base := "/proc/" + e.Name()
		raw, err := os.ReadFile(base + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue // gone, or a zombie
		}
		argv := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
		exe := argv[0]
		// Prefer the real executable path; argv0 may be relative ("bin/Runner.Listener").
		cwd, _ := os.Readlink(base + "/cwd")
		abs := func(p string) string {
			if !filepath.IsAbs(p) && cwd != "" {
				return filepath.Join(cwd, p)
			}
			return p
		}
		if target, err := os.Readlink(base + "/exe"); err == nil && strings.Contains(filepath.Base(target), "Runner.") {
			exe = strings.TrimSuffix(target, " (deleted)") // binary replaced by a self-update
		} else {
			exe = abs(exe)
		}
		script := ""
		for _, a := range argv[1:] { // first non-flag argument: "bash -x ./run.sh"
			if !strings.HasPrefix(a, "-") {
				script = abs(a)
				break
			}
		}
		switch {
		case underAny(exe, dirs) && filepath.Base(exe) == "Runner.Listener":
			info := procInfo{pid: pid}
			info.ppid, info.start = statInfo(base)
			listeners = append(listeners, info)
		case underAny(exe, dirs) && filepath.Base(exe) == "Runner.Worker":
			worker = procInfo{pid: pid}
			worker.ppid, worker.start = statInfo(base)
		case isShell(argv[0]) && underAny(script, dirs) &&
			(filepath.Base(script) == "run.sh" || filepath.Base(script) == "run-helper.sh" || filepath.Base(script) == "runsvc.sh"):
			if ps.SupervisorPID == 0 || filepath.Base(script) != "run-helper.sh" {
				ps.SupervisorPID = pid
			}
		case filepath.Base(argv[0]) == "node" && underAny(script, dirs) && filepath.Base(script) == "RunnerService.js":
			// Installed as a service (svc.sh): the node supervisor restarts the listener.
			ps.SupervisorPID = pid
		}
	}
	ps.WorkerPID = worker.pid
	ps.Listeners = len(listeners)
	if len(listeners) > 0 {
		best := pickListener(listeners, worker)
		ps.ListenerPID = best.pid
		ps.ListenerLog = openRunnerLog(best.pid)
	}
	return ps
}

// statInfo returns the parent pid and start time from /proc/<pid>/stat.
func statInfo(base string) (ppid int, start uint64) {
	raw, err := os.ReadFile(base + "/stat")
	if err != nil {
		return 0, 0
	}
	// Fields after the last ')' (the command name may contain spaces).
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, 0
	}
	f := strings.Fields(string(raw[i+1:]))
	// f[0]=state f[1]=ppid … f[19]=starttime (field 22 overall)
	if len(f) > 19 {
		ppid, _ = strconv.Atoi(f[1])
		start, _ = strconv.ParseUint(f[19], 10, 64)
	}
	return ppid, start
}

// openRunnerLog returns the _diag/Runner_*.log file a process has open
// (empty if it can't be read, e.g. another user's process).
func openRunnerLog(pid int) string {
	fdDir := "/proc/" + strconv.Itoa(pid) + "/fd"
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return ""
	}
	for _, fd := range fds {
		target, err := os.Readlink(fdDir + "/" + fd.Name())
		if err != nil {
			continue
		}
		base := filepath.Base(target)
		if filepath.Base(filepath.Dir(target)) == "_diag" && strings.HasPrefix(base, "Runner_") && strings.HasSuffix(base, ".log") {
			return target
		}
	}
	return ""
}
