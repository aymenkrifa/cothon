package fleet

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Processes are the live runner processes for one runner directory.
type Processes struct {
	ListenerPID int `json:"listener_pid,omitempty"`
	WorkerPID   int `json:"worker_pid,omitempty"`
	// SupervisorPID is run.sh / run-helper.sh. It stays alive while the
	// listener restarts (self-update, retryable errors), so "no listener but
	// a supervisor" means "restarting", not "stopped".
	SupervisorPID int `json:"supervisor_pid,omitempty"`
	// ListenerLog is the Runner_*.log the live listener has open. A failed
	// second listener (e.g. a session conflict) also creates a newer
	// Runner_*.log, so "newest file" is not a reliable choice.
	ListenerLog string `json:"listener_log,omitempty"`
	// Listeners counts live listener processes; more than one means a
	// session conflict is in progress.
	Listeners int `json:"listeners,omitempty"`
}

func (p Processes) Running() bool { return p.ListenerPID != 0 }
func (p Processes) Busy() bool    { return p.WorkerPID != 0 }

// Alive reports whether anything of the runner is up (listener or its supervisor).
func (p Processes) Alive() bool { return p.ListenerPID != 0 || p.SupervisorPID != 0 }

type procInfo struct {
	pid, ppid int
	start     uint64 // clock ticks since boot
}

// FindProcesses scans /proc for Runner.Listener / Runner.Worker processes
// (and their run.sh supervisor) started from runnerDir. Processes of any user
// are detected, so a runner installed as another user's service is still seen
// as running; controlling it is a separate, permission-checked step.
func FindProcesses(runnerDir string) Processes {
	var ps Processes
	dirs := []string{filepath.Clean(runnerDir)}
	if real, err := filepath.EvalSymlinks(runnerDir); err == nil && real != dirs[0] {
		dirs = append(dirs, real)
	}
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
		// With two listeners (a conflicting second start), the live one is the
		// worker's parent, else the oldest.
		best := listeners[0]
		for _, l := range listeners[1:] {
			if (worker.pid != 0 && l.pid == worker.ppid) || (best.pid != worker.ppid && l.start < best.start) {
				best = l
			}
		}
		ps.ListenerPID = best.pid
		ps.ListenerLog = openRunnerLog(best.pid)
	}
	return ps
}

func isShell(p string) bool {
	switch filepath.Base(p) {
	case "bash", "sh", "dash", "zsh":
		return true
	}
	return false
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

func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(path, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
