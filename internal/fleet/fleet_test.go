package fleet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// workerLog builds a synthetic Worker_*.log mirroring the layout of runner
// 2.338.x (anonymized). result == "" means the job is still running.
func workerLog(guid, name, repo, runID, attempt, checkRunID, result string, start time.Time) string {
	ts := func(d time.Duration) string { return start.Add(d).UTC().Format("2006-01-02 15:04:05") }
	var b strings.Builder
	fmt.Fprintf(&b, "[%sZ INFO HostContext] Well known directory 'Bin': '/home/u/actions-runner/bin.2.338.2'\n", ts(0))
	fmt.Fprintf(&b, "[%sZ INFO Worker] Job message:\n", ts(0))
	fmt.Fprintf(&b, `{
  "mask": [
    {
      "type": "regex",
      "value": "***"
    }
  ],
  "steps": [
    {
      "reference": {
        "type": "repository",
        "name": "actions/checkout",
        "ref": "v6"
      }
    }
  ],
  "jobId": %q,
  "jobDisplayName": %q,
  "jobName": "_3",
  "contextData": {
    "github": {
      "t": 2,
      "d": [
        {
          "k": "ref",
          "v": "refs/heads/develop"
        },
        {
          "k": "sha",
          "v": "78092cb9ba0a9d068fb232c4778e7d5aa775cd14"
        },
        {
          "k": "repository",
          "v": %q
        },
        {
          "k": "run_id",
          "v": %q
        },
        {
          "k": "run_number",
          "v": "424"
        },
        {
          "k": "run_attempt",
          "v": %q
        },
        {
          "k": "actor",
          "v": "octocat"
        },
        {
          "k": "workflow",
          "v": "CI"
        },
        {
          "k": "event_name",
          "v": "push"
        },
        {
          "k": "server_url",
          "v": "https://github.com"
        },
        {
          "k": "event",
          "t": 2,
          "d": [
            {
              "k": "repository",
              "v": "nested/should-not-win"
            }
          ]
        }
      ]
    },
    "job": {
      "t": 2,
      "d": [
        {
          "k": "check_run_id",
          "v": %s
        }
      ]
    }
  }
}
`, guid, name, repo, runID, attempt, checkRunID)
	fmt.Fprintf(&b, "[%sZ INFO JobRunner] Job ID %s\n", ts(time.Second), guid)
	fmt.Fprintf(&b, "[%sZ INFO StepsRunner] Processing step: DisplayName='Run tests'\n", ts(5*time.Second))
	if result != "" {
		fmt.Fprintf(&b, "[%sZ INFO JobRunner] Job result after all job steps finish: %s\n", ts(2*time.Minute), result)
		fmt.Fprintf(&b, "[%sZ INFO Worker] Job completed.\n", ts(2*time.Minute+3*time.Second))
	}
	return b.String()
}

var t0 = time.Date(2026, 11, 17, 16, 11, 25, 0, time.UTC)

func TestParseWorkerLog(t *testing.T) {
	tests := []struct {
		name, result string
		attempt      string
		wantStatus   Status
	}{
		{"succeeded", "Succeeded", "1", StatusCompleted},
		{"failed", "Failed", "1", StatusCompleted},
		{"canceled-retry", "Canceled", "2", StatusCompleted},
		{"running", "", "1", StatusRunning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := workerLog("1e1faaf1-guid", "Test (3/4)", "acme/widgets", "44119874235", tc.attempt, "134178177665.0", tc.result, t0)
			j, err := ParseWorkerLog(strings.NewReader(log))
			if err != nil {
				t.Fatal(err)
			}
			check := func(field, got, want string) {
				t.Helper()
				if got != want {
					t.Errorf("%s = %q, want %q", field, got, want)
				}
			}
			check("ID", j.ID, "1e1faaf1-guid")
			check("Name", j.Name, "Test (3/4)")
			check("Repository", j.Repository, "acme/widgets")
			check("RunID", j.RunID, "44119874235")
			check("RunAttempt", j.RunAttempt, tc.attempt)
			check("CheckRunID", j.CheckRunID, "134178177665")
			check("Workflow", j.Workflow, "CI")
			check("Ref", j.Ref, "refs/heads/develop")
			check("Event", j.Event, "push")
			check("Result", j.Result, tc.result)
			if j.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", j.Status, tc.wantStatus)
			}
			if !j.Started.Equal(t0) {
				t.Errorf("Started = %v, want %v", j.Started, t0)
			}
			if tc.result != "" {
				if d := j.Duration(time.Time{}); d != 2*time.Minute+3*time.Second {
					t.Errorf("Duration = %v", d)
				}
				check("HTMLURL", j.HTMLURL(), "https://github.com/acme/widgets/actions/runs/44119874235/job/134178177665")
			}
		})
	}
}

func TestParseWorkerLogMalformedJSONFallsBack(t *testing.T) {
	log := workerLog("g", "Build", "acme/widgets", "7", "1", "99.0", "Succeeded", t0)
	// Truncate the JSON so it no longer parses: drop the closing braces of the message.
	log = strings.Replace(log, "  }\n}\n", "", 1)
	j, err := ParseWorkerLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if j.Repository != "acme/widgets" || j.RunID != "7" || j.CheckRunID != "99" || j.Name != "Build" || j.ID != "g" {
		t.Errorf("fallback parse got %+v", j)
	}
}

func TestParseWorkerLogRejectsGarbage(t *testing.T) {
	if _, err := ParseWorkerLog(strings.NewReader("hello\nworld\n")); err == nil {
		t.Error("expected error for non-diag input")
	}
}

func TestIntegerID(t *testing.T) {
	for in, want := range map[string]string{
		"134178177665.0": "134178177665",
		"42":             "42",
		"1.5":            "",
		"1e10":           "",
		"":               "",
		"-3":             "",
	} {
		if got := integerID(in); got != want {
			t.Errorf("integerID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRunnerLog(t *testing.T) {
	log := `[2026-11-17 16:11:20Z INFO Terminal] WRITE LINE: 2026-11-17 16:11:20Z: Listening for Jobs
[2026-11-17 16:11:24Z INFO JobDispatcher] Job request 0 for plan p job g received.
[2026-11-17 16:11:24Z INFO Terminal] WRITE LINE: 2026-11-17 16:11:24Z: Running job: Test (3/4)
`
	st, err := ParseRunnerLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if st.Event != ListenerBusy || st.Job != "Test (3/4)" {
		t.Errorf("got %+v, want busy on Test (3/4)", st)
	}

	log += `[2026-11-17 16:13:28Z INFO Terminal] WRITE LINE: 2026-11-17 16:13:28Z: Job Test (3/4) completed with result: Succeeded
[2026-11-17 16:13:41Z INFO Terminal] WRITE LINE: 2026-11-17 16:13:41Z: Listening for Jobs
`
	st, _ = ParseRunnerLog(strings.NewReader(log))
	if st.Event != ListenerListening {
		t.Errorf("got %+v, want listening", st)
	}

	log += `[2026-11-17 18:00:00Z INFO Runner] Deleting Runner Session...
[2026-11-17 18:00:00Z INFO Listener] Runner execution been cancelled.
`
	st, _ = ParseRunnerLog(strings.NewReader(log))
	if st.Event != ListenerExited {
		t.Errorf("got %+v, want exited", st)
	}
}

func TestScannerMarksAbandonedAndCaches(t *testing.T) {
	dir := t.TempDir()
	diag := filepath.Join(dir, "_diag")
	if err := os.Mkdir(diag, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(diag, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, ".runner"), []byte("\xef\xbb\xbf{\"agentName\":\"laptop\",\"poolName\":\"Default\",\"gitHubUrl\":\"https://github.com/acme\"}"), 0o644)
	write("Worker_20261117-100000-utc.log", workerLog("old", "Lint", "acme/a", "1", "1", "10.0", "", t0.Add(-time.Hour))) // never finished
	write("Worker_20261117-110000-utc.log", workerLog("done", "Test", "acme/b", "2", "1", "20.0", "Failed", t0.Add(-30*time.Minute)))
	write("Worker_20261117-161125-utc.log", workerLog("live", "Build", "acme/c", "3", "1", "30.0", "", t0))
	write("Runner_20261117-090000-utc.log", "[2026-11-17 16:11:24Z INFO Terminal] WRITE LINE: 2026-11-17 16:11:24Z: Running job: Build\n")

	s := NewScanner(dir)
	snap, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Runner == nil || snap.Runner.Name != "laptop" {
		t.Errorf("runner config = %+v", snap.Runner)
	}
	if len(snap.Jobs) != 3 {
		t.Fatalf("got %d jobs", len(snap.Jobs))
	}
	got := []string{}
	for _, j := range snap.Jobs {
		got = append(got, j.ID+":"+string(j.Status))
	}
	want := "live:running done:completed old:abandoned"
	if strings.Join(got, " ") != want {
		t.Errorf("jobs = %v, want %s", got, want)
	}
	if snap.Listener.Event != ListenerBusy {
		t.Errorf("listener = %+v", snap.Listener)
	}

	// Second scan must reuse the cache and not leak the "abandoned" mutation.
	snap2, _ := s.Scan()
	if snap2.Jobs[2].Status != StatusAbandoned || s.cache[snap.Jobs[2].LogFile].job.Status != StatusRunning {
		t.Error("abandoned status should be applied per-snapshot, not stored in cache")
	}

	// Newest job never finished but the listener is back to listening after it
	// started (worker crashed / runner restarted): it must not stay "running".
	write("Runner_20261117-170000-utc.log", "[2026-11-17 17:00:00Z INFO Terminal] WRITE LINE: 2026-11-17 17:00:00Z: Listening for Jobs\n")
	snap3, _ := s.Scan()
	if snap3.Jobs[0].ID != "live" || snap3.Jobs[0].Status != StatusAbandoned {
		t.Errorf("newest job = %s:%s, want live:abandoned", snap3.Jobs[0].ID, snap3.Jobs[0].Status)
	}

	// Listening *before* the job started must not abandon it (a newer log
	// whose last event predates the job).
	write("Runner_20261117-180000-utc.log", "[2026-11-17 16:00:00Z INFO Terminal] WRITE LINE: 2026-11-17 16:00:00Z: Listening for Jobs\n")
	snap4, _ := s.Scan()
	if snap4.Jobs[0].Status != StatusRunning {
		t.Errorf("newest job = %s, want running", snap4.Jobs[0].Status)
	}
}

// TestRealRunnerLogs parses a real runner directory when COTHON_REAL_RUNNER_DIR
// is set (e.g. ~/actions-runner) and fails if any job lacks its key fields.
func TestRealRunnerLogs(t *testing.T) {
	dir := os.Getenv("COTHON_REAL_RUNNER_DIR")
	if dir == "" {
		t.Skip("COTHON_REAL_RUNNER_DIR not set")
	}
	snap, err := NewScanner(dir).Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snap.Errors {
		t.Errorf("scan error: %s", e)
	}
	for _, j := range snap.Jobs {
		if j.ID == "" || j.Name == "" || j.Repository == "" || j.RunID == "" || j.CheckRunID == "" {
			t.Errorf("%s: incomplete job %+v", j.LogFile, j)
		}
		if j.Status == StatusCompleted && j.Result == "" {
			t.Errorf("%s: completed without result", j.LogFile)
		}
	}
	t.Logf("%d real jobs parsed", len(snap.Jobs))
}

func TestScannerUsesProcessLiveness(t *testing.T) {
	dir := t.TempDir()
	diag := filepath.Join(dir, "_diag")
	os.Mkdir(diag, 0o755)
	os.WriteFile(filepath.Join(diag, "Worker_20261117-161125-utc.log"), []byte(workerLog("live", "Build", "acme/c", "3", "1", "30.0", "", t0)), 0o644)
	procs := Processes{ListenerPID: 10, WorkerPID: 11}
	s := NewScanner(dir)
	s.Procs = func() Processes { return procs }
	snap, _ := s.Scan()
	if snap.Jobs[0].Status != StatusRunning {
		t.Fatalf("with a worker process: %s", snap.Jobs[0].Status)
	}
	procs.WorkerPID = 0
	snap, _ = s.Scan()
	if snap.Jobs[0].Status != StatusAbandoned {
		t.Fatalf("without a worker process: %s", snap.Jobs[0].Status)
	}
}

func TestParseWorkerLogFileHeadTail(t *testing.T) {
	log := workerLog("g", "Big", "acme/big", "5", "1", "50.0", "", t0)
	var b strings.Builder
	b.WriteString(log)
	// Pad with ~1 MB of step output so the head+tail path is used.
	for i := 0; b.Len() < 1<<20; i++ {
		fmt.Fprintf(&b, "[2026-11-17 16:12:%02dZ INFO StepsRunner] some step output line %d with padding padding padding\n", i%60, i)
	}
	fmt.Fprintf(&b, "[2026-11-17 16:20:00Z INFO JobRunner] Job result after all job steps finish: Failed\n")
	fmt.Fprintf(&b, "[2026-11-17 16:20:01Z INFO Worker] Job completed.\n")
	path := filepath.Join(t.TempDir(), "Worker_x.log")
	os.WriteFile(path, []byte(b.String()), 0o644)

	j, err := ParseWorkerLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Repository != "acme/big" || j.CheckRunID != "50" || j.Result != "Failed" || j.Status != StatusCompleted {
		t.Errorf("got %+v", j)
	}
	if want := time.Date(2026, 11, 17, 16, 20, 1, 0, time.UTC); !j.Finished.Equal(want) {
		t.Errorf("Finished = %v", j.Finished)
	}
}

func BenchmarkParseLogLine(b *testing.B) {
	line := "[2026-11-17 16:11:25Z INFO JobRunner] Job result after all job steps finish: Succeeded"
	for b.Loop() {
		parseLogLine(line)
	}
}

// Sweep header sizes and result positions around the head/tail boundary:
// the result line must never be lost (regression: scanner read-ahead).
func TestHeadTailNeverLosesResult(t *testing.T) {
	dir := t.TempDir()
	for _, headerPad := range []int{0, 40 << 10, 70 << 10, 200 << 10, 330 << 10} {
		for _, tailPad := range []int{0, 1 << 10, 18 << 10, 63 << 10, 65 << 10, 100 << 10} {
			var b strings.Builder
			log := workerLog("g", "Big", "acme/big", "5", "1", "50.0", "", t0)
			// Grow the job message (header) by padding the mask list.
			log = strings.Replace(log, `"value": "***"`, `"value": "`+strings.Repeat("x", headerPad)+`"`, 1)
			b.WriteString(log)
			for i := 0; b.Len() < len(log)+300<<10; i++ {
				fmt.Fprintf(&b, "[2026-11-17 16:12:%02dZ INFO StepsRunner] step output %d\n", i%60, i)
			}
			fmt.Fprintf(&b, "[2026-11-17 16:20:00Z INFO JobRunner] Job result after all job steps finish: Failed\n")
			for b2 := 0; b2 < tailPad; {
				line := "[2026-11-17 16:20:00Z INFO JobRunner] cleanup line padding padding padding\n"
				b.WriteString(line)
				b2 += len(line)
			}
			fmt.Fprintf(&b, "[2026-11-17 16:20:01Z INFO Worker] Job completed.\n")
			path := filepath.Join(dir, fmt.Sprintf("Worker_%d_%d.log", headerPad, tailPad))
			os.WriteFile(path, []byte(b.String()), 0o644)
			j, err := ParseWorkerLogFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if j.Status != StatusCompleted || j.Repository != "acme/big" {
				t.Errorf("header %d tail %d: status=%s repo=%q", headerPad, tailPad, j.Status, j.Repository)
			}
			if tailPad < 60<<10 && j.Result != "Failed" {
				t.Errorf("header %d tail %d: result %q", headerPad, tailPad, j.Result)
			}
		}
	}
}

func TestPartialLastLineIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Worker_p.log")
	log := workerLog("g", "Build", "acme/a", "1", "1", "1.0", "", t0) +
		"[2026-11-17 16:20:00Z INFO JobRunner] Job result after all job steps finish: Succ"
	os.WriteFile(path, []byte(log), 0o644)
	j, err := ParseWorkerLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Result != "" || j.Status != StatusRunning {
		t.Errorf("half-written line must be ignored: %+v", j)
	}
}

func TestSessionRestartIsNotExit(t *testing.T) {
	log := `[2026-11-17 16:13:00Z INFO Terminal] WRITE LINE: 2026-11-17 16:13:00Z: Listening for Jobs
[2026-11-17 16:13:38Z INFO Runner] Deleting Runner Session...
[2026-11-17 16:13:38Z INFO Runner] Restarting runner session after config update...
`
	st, _ := ParseRunnerLog(strings.NewReader(log))
	if st.Event == ListenerExited {
		t.Error("session restart reported as exit")
	}
	st, _ = ParseRunnerLog(strings.NewReader(log + "[2026-11-17 18:00:00Z INFO Listener] Runner execution has finished with return code 0\n"))
	if st.Event != ListenerExited {
		t.Errorf("got %s, want exited", st.Event)
	}
}

func TestInvisibleRunnerDoesNotAbandon(t *testing.T) {
	dir := t.TempDir()
	diag := filepath.Join(dir, "_diag")
	os.Mkdir(diag, 0o755)
	os.WriteFile(filepath.Join(diag, "Worker_1.log"), []byte(workerLog("live", "Build", "acme/c", "3", "1", "30.0", "", t0)), 0o644)
	s := NewScanner(dir)
	s.Procs = func() Processes { return Processes{} } // e.g. runner owned by another user / hidepid
	snap, _ := s.Scan()
	if snap.Jobs[0].Status != StatusRunning {
		t.Errorf("status = %s, want running", snap.Jobs[0].Status)
	}
}

func TestHeaderlessPageMerged(t *testing.T) {
	dir := t.TempDir()
	diag := filepath.Join(dir, "_diag")
	os.Mkdir(diag, 0o755)
	os.WriteFile(filepath.Join(diag, "Worker_1.log"), []byte(workerLog("big", "Build", "acme/c", "3", "1", "30.0", "", t0)), 0o644)
	page2 := "[2026-11-17 16:30:00Z INFO StepsRunner] more output\n" +
		"[2026-11-17 16:40:00Z INFO JobRunner] Job result after all job steps finish: Succeeded\n" +
		"[2026-11-17 16:40:01Z INFO Worker] Job completed.\n"
	os.WriteFile(filepath.Join(diag, "Worker_2.log"), []byte(page2), 0o644)
	os.WriteFile(filepath.Join(diag, "Worker_3.log"), nil, 0o644) // just created, empty
	snap, _ := NewScanner(dir).Scan()
	if len(snap.Jobs) != 1 {
		t.Fatalf("got %d jobs, want the page merged into 1", len(snap.Jobs))
	}
	j := snap.Jobs[0]
	if j.ID != "big" || j.Result != "Succeeded" || j.Status != StatusCompleted || j.Duration(time.Time{}) != 28*time.Minute+36*time.Second {
		t.Errorf("merged job = %+v (%v)", j, j.Duration(time.Time{}))
	}
	for _, e := range snap.Errors {
		if strings.Contains(e, "Worker_") {
			t.Errorf("log error: %v", e)
		}
	}
}

func TestSampleTreeCountsChildren(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 3 & sleep 3 & wait")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(200 * time.Millisecond)
	s := SampleTree(cmd.Process.Pid)
	if s.Procs < 3 || s.RSS == 0 {
		t.Errorf("tree sample = %+v, want sh + 2 children with memory", s)
	}
	if own := SampleProcs(cmd.Process.Pid); own.Procs != 1 {
		t.Errorf("own sample = %+v", own)
	}
	if SampleTree(0).Procs != 0 || SampleTree(-5).Procs != 0 {
		t.Error("invalid roots must sample nothing")
	}
}
