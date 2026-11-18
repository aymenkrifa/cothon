package fleet

import (
	"bufio"
	"path/filepath"
	"strconv"
	"strings"
)

// Parsers for BSD ps(1) output, used on macOS where there is no /proc. They
// are plain functions so they are tested on every OS. Lines that don't parse
// are skipped: a missed process only means a "—" in the UI.

// psRow is one line of `ps -axww -o pid=,ppid=,etime=,command=`.
type psRow struct {
	pid, ppid int
	elapsed   uint64 // seconds since the process started
	command   string // argv joined with spaces (paths may contain spaces too)
}

// parsePSRows parses `ps -axww -o pid=,ppid=,etime=,command=` output.
func parsePSRows(out string) []psRow {
	var rows []psRow
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f, rest := leadingFields(sc.Text(), 3)
		if f == nil {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		el, ok := parseClock(f[2])
		if err1 != nil || err2 != nil || !ok || pid <= 0 || rest == "" {
			continue
		}
		rows = append(rows, psRow{pid: pid, ppid: ppid, elapsed: el / 100, command: rest})
	}
	return rows
}

// parsePSUsage parses `ps -axS -o pid=,ppid=,rss=,time=` output (rss in KiB;
// with -S, time includes reaped children, like cutime+cstime on Linux).
func parsePSUsage(out string) map[int]procStat {
	m := map[int]procStat{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseUint(f[2], 10, 64)
		ticks, ok := parseClock(f[3])
		if err1 != nil || err2 != nil || err3 != nil || !ok || pid <= 0 {
			continue
		}
		m[pid] = procStat{ppid: ppid, ticks: ticks, rss: rss * 1024}
	}
	return m
}

// leadingFields splits off the first n whitespace-separated fields and
// returns them with the rest of the line (its inner spacing kept).
func leadingFields(line string, n int) ([]string, string) {
	f := make([]string, 0, n)
	s := line
	for len(f) < n {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return nil, ""
		}
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			f = append(f, s)
			s = ""
			continue
		}
		f = append(f, s[:i])
		s = s[i:]
	}
	return f, strings.TrimSpace(s)
}

// parseClock parses ps's "[[dd-]hh:]mm:ss[.cc]" (etime, time, cputime) into
// hundredths of a second.
func parseClock(s string) (uint64, bool) {
	var days uint64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		v, err := strconv.ParseUint(d, 10, 64)
		if err != nil {
			return 0, false
		}
		days, s = v, rest
	}
	var frac uint64
	if whole, f, ok := strings.Cut(s, "."); ok {
		if f == "" || len(f) > 9 {
			return 0, false
		}
		f = (f + "00")[:2]
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, false
		}
		frac, s = v, whole
	}
	parts := strings.Split(s, ":")
	if len(parts) < 1 || len(parts) > 3 {
		return 0, false
	}
	var secs uint64
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + v
	}
	return (days*86400+secs)*100 + frac, true
}

// findPath finds, in a ps command line, a path under one of dirs whose last
// element is name, as a whole word ("<dir>/bin/Runner.Listener run").
// Matching on known prefixes rather than splitting on spaces keeps runner
// directories with spaces working. Of several candidates it returns the one
// starting last (the shortest: "<dir>/…/node <dir>/bin/RunnerService.js"
// yields the script), with where it starts.
func findPath(cmd string, dirs []string, name string) (string, int, bool) {
	path, at, found := "", 0, false
	for _, d := range dirs {
		prefix := d + "/"
		for off := 0; ; {
			i := strings.Index(cmd[off:], prefix)
			if i < 0 {
				break
			}
			i += off
			off = i + 1
			if (i > 0 && cmd[i-1] != ' ') || (found && i <= at) {
				continue
			}
			// The first "/name" ending a word after the prefix.
			for j := i + len(prefix) - 1; ; {
				k := strings.Index(cmd[j:], "/"+name)
				if k < 0 {
					break
				}
				end := j + k + 1 + len(name)
				if end == len(cmd) || cmd[end] == ' ' {
					path, at, found = cmd[i:end], i, true
					break
				}
				j = end
			}
		}
	}
	return path, at, found
}

// classifyPS finds the runner's processes in a ps table (macOS). Processes
// whose command line can't be read (other users', on macOS) show as
// "(name)" and are not matched.
func classifyPS(rows []psRow, dirs []string, now uint64) (ps Processes, listeners []procInfo, worker procInfo) {
	for _, r := range rows {
		info := procInfo{pid: r.pid, ppid: r.ppid, start: now - min(now, r.elapsed)}
		if _, at, ok := findPath(r.command, dirs, "Runner.Listener"); ok && at == 0 {
			listeners = append(listeners, info)
			continue
		}
		if _, at, ok := findPath(r.command, dirs, "Runner.Worker"); ok && at == 0 {
			worker = info
			continue
		}
		for _, script := range []string{"run.sh", "run-helper.sh", "runsvc.sh"} {
			_, at, ok := findPath(r.command, dirs, script)
			if !ok {
				continue
			}
			// "bash -x <dir>/run.sh": a shell and only flags before the script.
			// A relative "./run.sh" can't be resolved without the cwd; run.sh
			// starts run-helper.sh by absolute path, which is found instead.
			argv := strings.Fields(r.command[:at])
			flagsOnly := len(argv) > 0
			for _, a := range argv[1:] {
				if !strings.HasPrefix(a, "-") {
					flagsOnly = false
				}
			}
			if flagsOnly && isShell(argv[0]) && (ps.SupervisorPID == 0 || script != "run-helper.sh") {
				ps.SupervisorPID = r.pid
			}
			break
		}
		if _, at, ok := findPath(r.command, dirs, "RunnerService.js"); ok && at > 0 {
			// Installed as a service (svc.sh): the node supervisor restarts the listener.
			if filepath.Base(strings.TrimSpace(r.command[:at])) == "node" {
				ps.SupervisorPID = r.pid
			}
		}
	}
	return ps, listeners, worker
}
