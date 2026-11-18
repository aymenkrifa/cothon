package fleet

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

var pageSize = uint64(os.Getpagesize())

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

// procStatsOf reads the stats of just these processes.
func procStatsOf(pids []int) map[int]procStat {
	m := make(map[int]procStat, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if st, ok := readStat(pid); ok {
			m[pid] = st
		}
	}
	return m
}

// procTable reads the stats of every process (nil if /proc can't be read).
func procTable() map[int]procStat {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	stats := make(map[int]procStat, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, ok := readStat(pid); ok {
			stats[pid] = st
		}
	}
	return stats
}
