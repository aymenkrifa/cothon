package fleet

// ClockTicks is the unit of Sample.Ticks: USER_HZ on Linux (the unit of CPU
// times in /proc/<pid>/stat, 100 on every architecture cothon targets), and
// hundredths of a second on macOS, where ps reports CPU time as mm:ss.cc.
const ClockTicks = 100

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
	ticks uint64 // own CPU time plus that of reaped children
	rss   uint64
}

// SampleProcs measures the given processes themselves (not their children).
func SampleProcs(pids ...int) Sample {
	var s Sample
	stats := procStatsOf(pids)
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if st, ok := stats[pid]; ok {
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
	stats := procTable()
	if stats == nil {
		return Sample{}
	}
	children := make(map[int][]int, len(stats))
	for pid, st := range stats {
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
