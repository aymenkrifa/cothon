package fleet

import (
	"testing"
)

func TestParseClock(t *testing.T) {
	for in, want := range map[string]uint64{
		"0:00.03":     3,
		"12:34.56":    (12*60+34)*100 + 56,
		"1234:05.5":   (1234*60+5)*100 + 50,
		"01:02:03":    3723 * 100,
		"2-01:02:03":  (2*86400 + 3723) * 100,
		"00:07":       700,
		"3:00.123456": 18012,
	} {
		if got, ok := parseClock(in); !ok || got != want {
			t.Errorf("parseClock(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "x", "1:2:3:4", "1:xx", "1.", "a-1:00", "-"} {
		if _, ok := parseClock(bad); ok {
			t.Errorf("parseClock(%q) accepted", bad)
		}
	}
}

func TestParsePSUsage(t *testing.T) {
	out := `    1     0  12345   1:02.50
  501     1   2048   0:00.03
junk line here
  502   501    abc   0:01.00
  503   501    100
`
	m := parsePSUsage(out)
	if len(m) != 2 {
		t.Fatalf("got %d rows: %+v", len(m), m)
	}
	if st := m[1]; st.ppid != 0 || st.rss != 12345*1024 || st.ticks != 6250 {
		t.Errorf("pid 1 = %+v", st)
	}
	if st := m[501]; st.ppid != 1 || st.rss != 2048*1024 || st.ticks != 3 {
		t.Errorf("pid 501 = %+v", st)
	}
}

const runner = "/Users/me/actions runner" // a space, on purpose

func TestClassifyPS(t *testing.T) {
	out := `  100     1   01:00:00 /bin/bash /Users/me/actions runner/run.sh
  101   100   01:00:00 /bin/bash /Users/me/actions runner/run-helper.sh
  102   101      59:00 /Users/me/actions runner/bin/Runner.Listener run
  103   101      00:10 /Users/me/actions runner/bin/Runner.Listener run --startuptype service
  104   102      05:00 /Users/me/actions runner/bin/Runner.Worker spawnclient 107 110
  105     1      00:01 grep /Users/me/actions runner/bin/Runner.Listener
  106     1      00:01 /Users/me/actions runner/bin/Runner.ListenerX
  107     1      00:01 /Users/me/other/bin/Runner.Worker spawnclient
  108     1      00:01 (Runner.Listener)
  109     1      00:01 vim /Users/me/actions runner/run.sh
garbage
`
	rows := parsePSRows(out)
	if len(rows) != 10 {
		t.Fatalf("parsed %d rows, want 10", len(rows))
	}
	if rows[0].command != "/bin/bash /Users/me/actions runner/run.sh" || rows[0].elapsed != 3600 {
		t.Errorf("row 0 = %+v", rows[0])
	}
	ps, listeners, worker := classifyPS(rows, []string{runner}, 10_000)
	if ps.SupervisorPID != 100 {
		t.Errorf("supervisor = %d, want run.sh (100)", ps.SupervisorPID)
	}
	if len(listeners) != 2 || worker.pid != 104 || worker.ppid != 102 {
		t.Fatalf("listeners %+v worker %+v", listeners, worker)
	}
	// The worker's parent wins, even though it is the older one here too.
	if got := pickListener(listeners, worker).pid; got != 102 {
		t.Errorf("listener = %d, want 102", got)
	}
	// Without a worker the oldest wins.
	if got := pickListener(listeners, procInfo{}).pid; got != 102 {
		t.Errorf("listener without worker = %d, want 102", got)
	}
	if listeners[0].start != 10_000-59*60 {
		t.Errorf("start = %d", listeners[0].start)
	}
}

func TestClassifyPSSupervisors(t *testing.T) {
	// Only run-helper.sh visible (run.sh started as "./run.sh").
	ps, _, _ := classifyPS(parsePSRows(`
  10 1 00:05 /bin/bash ./run.sh
  11 10 00:05 /bin/bash /Users/me/actions runner/run-helper.sh
`), []string{runner}, 100)
	if ps.SupervisorPID != 11 {
		t.Errorf("supervisor = %d, want run-helper.sh (11)", ps.SupervisorPID)
	}
	// svc.sh install: node RunnerService.js.
	ps, _, _ = classifyPS(parsePSRows(`
  20 1 00:05 /Users/me/actions runner/externals/node20/bin/node /Users/me/actions runner/bin/RunnerService.js
`), []string{runner}, 100)
	if ps.SupervisorPID != 20 {
		t.Errorf("supervisor = %d, want node (20)", ps.SupervisorPID)
	}
	// "bash -x" counts; a script argument after a non-flag doesn't.
	ps, _, _ = classifyPS(parsePSRows(`
  30 1 00:05 bash -x /Users/me/actions runner/run.sh
  31 1 00:05 bash other.sh /Users/me/actions runner/run.sh
`), []string{runner}, 100)
	if ps.SupervisorPID != 30 {
		t.Errorf("supervisor = %d, want 30", ps.SupervisorPID)
	}
}
