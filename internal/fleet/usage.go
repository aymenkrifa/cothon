package fleet

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// ClockTicks is USER_HZ, the unit of CPU times in /proc/<pid>/stat (100 on
// every Linux architecture cothon targets).
const ClockTicks = 100

var pageSize = uint64(os.Getpagesize())

// Sample is the resource use of a set of processes at one instant.
type Sample struct {
	// Ticks is total CPU time in clock ticks, including children that already
	// exited and were reaped inside the set (so short-lived compiler or test
	// processes are counted).
	Ticks uint64
	RSS   uint64 // resident memory in bytes (shared pages counted per process)
	Procs int
}

type procStat struct {
	ppid  int
	ticks uint64 // utime + stime + cutime + cstime
	rss   uint64
}

// readStat parses /proc/<pid>/stat.
func readStat(pid int) (procStat, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	i := bytes.LastIndexByte(raw, ')') // the command name may contain spaces and ')'
	if i < 0 {
		return procStat{}, false
	}
	f := strings.Fields(string(raw[i+1:]))
	// f[0] is field 3 (state); field n is f[n-3].
	if len(f) < 22 {
		return procStat{}, false
	}
	var st procStat
	st.ppid, _ = strconv.Atoi(f[1])
	for _, k := range []int{11, 12, 13, 14} { // utime, stime, cutime, cstime
		v, _ := strconv.ParseUint(f[k], 10, 64)
		st.ticks += v
	}
	pages, _ := strconv.ParseUint(f[21], 10, 64) // rss in pages
	st.rss = pages * pageSize
	return st, true
}

// SampleProcs measures the given processes themselves (not their children).
func SampleProcs(pids ...int) Sample {
	var s Sample
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if st, ok := readStat(pid); ok {
			s.Ticks += st.ticks
			s.RSS += st.rss
			s.Procs++
		}
	}
	return s
}

// SampleTree measures root and all its live descendants. Processes a job
// starts through Docker run under the Docker daemon, not under the job, and
// are not included.
func SampleTree(root int) Sample {
	if root <= 0 {
		return Sample{}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return Sample{}
	}
	stats := make(map[int]procStat, len(entries))
	children := make(map[int][]int, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		st, ok := readStat(pid)
		if !ok {
			continue
		}
		stats[pid] = st
		children[st.ppid] = append(children[st.ppid], pid)
	}
	var s Sample
	stack := []int{root}
	seen := map[int]bool{}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		st, ok := stats[pid]
		if !ok || seen[pid] {
			continue
		}
		seen[pid] = true
		s.Ticks += st.ticks
		s.RSS += st.rss
		s.Procs++
		stack = append(stack, children[pid]...)
	}
	return s
}
