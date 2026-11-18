package fleet

// procStatsOf reads the stats of these processes; one ps call lists them all.
func procStatsOf(pids []int) map[int]procStat {
	for _, pid := range pids {
		if pid > 0 {
			return procTable()
		}
	}
	return nil
}

// procTable reads the stats of every process (nil if ps fails). -S adds the
// CPU time of reaped children to their parent, like cutime/cstime on Linux.
func procTable() map[int]procStat {
	out := runPS("-axS", "-o", "pid=,ppid=,rss=,time=")
	if out == "" {
		return nil
	}
	return parsePSUsage(out)
}
