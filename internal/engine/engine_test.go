package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aymenkrifa/cothon/internal/config"
	"github.com/aymenkrifa/cothon/internal/fleet"
)

func workerLog(guid, repo, result string, start time.Time) string {
	ts := func(d time.Duration) string { return start.Add(d).UTC().Format("2006-01-02 15:04:05") }
	s := fmt.Sprintf(`[%sZ INFO Worker] Job message:
{
  "jobId": %q,
  "jobDisplayName": "build",
  "contextData": {
    "github": {"t": 2, "d": [
      {"k": "repository", "v": %q},
      {"k": "run_id", "v": "7"},
      {"k": "run_number", "v": "3"},
      {"k": "workflow", "v": "CI"}
    ]},
    "job": {"t": 2, "d": [{"k": "check_run_id", "v": 42.0}]}
  }
}
[%sZ INFO JobRunner] Job ID %s
`, ts(0), guid, repo, ts(time.Second), guid)
	if result != "" {
		s += fmt.Sprintf("[%sZ INFO JobRunner] Job result after all job steps finish: %s\n[%sZ INFO Worker] Job completed.\n", ts(time.Minute), result, ts(time.Minute))
	}
	return s
}

type harness struct {
	t     *testing.T
	dir   string
	e     *Engine
	procs fleet.Processes
	mu    sync.Mutex
	notes []string
}

func newHarness(t *testing.T) *harness {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "_diag"), 0o755)
	os.WriteFile(filepath.Join(dir, ".runner"), []byte(`{"agentName":"laptop","gitHubUrl":"https://github.com/acme"}`), 0o644)
	cfg := config.Default()
	cfg.RunnerDir = dir
	h := &harness{t: t, dir: dir}
	h.e = New(cfg, Options{Notify: true})
	h.e.usageFile = filepath.Join(t.TempDir(), "usage.json") // never the real state dir
	h.e.usageHist = map[string]UsageSummary{}
	h.e.scanner.Procs = func() fleet.Processes { return h.procs }
	h.e.ctl.Find = func() fleet.Processes { return h.procs }
	h.e.notify = func(title, body string, critical bool) {
		h.mu.Lock()
		h.notes = append(h.notes, title+" | "+body)
		h.mu.Unlock()
	}
	return h
}

func (h *harness) write(name, content string) {
	if err := os.WriteFile(filepath.Join(h.dir, "_diag", name), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) notifications() []string {
	time.Sleep(20 * time.Millisecond) // notify runs in a goroutine
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.notes...)
}

var t0 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

func TestNotifiesOnlyNewlyFinishedJobs(t *testing.T) {
	h := newHarness(t)
	h.write("Worker_1.log", workerLog("old", "acme/a", "Succeeded", t0))
	h.procs = fleet.Processes{ListenerPID: 1}
	h.e.refresh()
	if n := h.notifications(); len(n) != 0 {
		t.Fatalf("jobs present at startup must not notify: %v", n)
	}

	h.procs.WorkerPID = 2
	h.write("Worker_2.log", workerLog("new", "acme/b", "", t0.Add(time.Hour)))
	h.e.refresh()
	if n := h.notifications(); len(n) != 0 {
		t.Fatalf("running job must not notify: %v", n)
	}

	h.procs.WorkerPID = 0
	h.write("Worker_2.log", workerLog("new", "acme/b", "Failed", t0.Add(time.Hour)))
	h.e.refresh()
	n := h.notifications()
	if len(n) != 1 || !strings.Contains(n[0], "✗ b · build") || !strings.Contains(n[0], "Failed in 1m0s") {
		t.Fatalf("notifications = %v", n)
	}

	h.e.refresh()
	if n := h.notifications(); len(n) != 1 {
		t.Fatalf("must notify once: %v", n)
	}
}

func TestVersionOnlyBumpsOnChange(t *testing.T) {
	h := newHarness(t)
	h.write("Worker_1.log", workerLog("a", "acme/a", "Succeeded", t0))
	h.e.refresh()
	v1 := h.e.state.Version
	h.e.refresh()
	if h.e.state.Version != v1 {
		t.Errorf("no change but version %d -> %d", v1, h.e.state.Version)
	}
	h.procs = fleet.Processes{ListenerPID: 9}
	h.e.refresh()
	if h.e.state.Version != v1+1 {
		t.Errorf("listener appeared but version stayed %d", h.e.state.Version)
	}
}

func TestWatchWakesSubscribers(t *testing.T) {
	h := newHarness(t)
	h.e.refresh()
	ctx, cancel := context.WithCancel(context.Background())
	ch := h.e.Watch(ctx)
	h.procs = fleet.Processes{ListenerPID: 3}
	h.e.refresh()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no wakeup")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			<-ch
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestActionsRejectUnknownTargets(t *testing.T) {
	h := newHarness(t)
	h.e.cli.Path = "/bin/false" // must never be reached
	h.write("Worker_1.log", workerLog("a", "acme/a", "Failed", t0))
	h.e.refresh()
	ctx := context.Background()
	for _, a := range []Action{
		{Kind: ActCancelRun, Repo: "acme/other", RunID: "7"},
		{Kind: ActCancelRun, Repo: "acme/a", RunID: "8"},
		{Kind: ActRerunJob, Repo: "acme/a", JobID: "43"},
		{Kind: ActRerunJob, Repo: "acme/a; rm -rf ~", JobID: "42"},
		{Kind: ActCancelRun, Repo: "../../user", RunID: "7"},
		{Kind: "rm"},
	} {
		if _, err := h.e.Do(ctx, a); err == nil || strings.Contains(err.Error(), "exit status") {
			t.Errorf("%+v: err = %v, want rejection before calling gh", a, err)
		}
	}
}

func TestActionsCallGH(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	stub := filepath.Join(t.TempDir(), "gh")
	os.WriteFile(stub, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755)
	h.e.cli.Path = stub
	h.write("Worker_1.log", workerLog("a", "acme/a", "Failed", t0))
	h.e.refresh()
	ctx := context.Background()
	for _, a := range []Action{
		{Kind: ActRerunJob, Repo: "acme/a", JobID: "42"},
		{Kind: ActRerunFailed, Repo: "acme/a", RunID: "7"},
		{Kind: ActCancelRun, Repo: "acme/a", RunID: "7"},
	} {
		if _, err := h.e.Do(ctx, a); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	got, _ := os.ReadFile(log)
	want := "api -X POST repos/acme/a/actions/jobs/42/rerun\n" +
		"api -X POST repos/acme/a/actions/runs/7/rerun-failed-jobs\n" +
		"api -X POST repos/acme/a/actions/runs/7/cancel\n"
	if string(got) != want {
		t.Errorf("gh calls:\n%s\nwant:\n%s", got, want)
	}
}

func TestStopWhenIdleIsDeferred(t *testing.T) {
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: 999999999, WorkerPID: 2} // nonexistent pid: Interrupt fails harmlessly
	h.e.refresh()
	msg, err := h.e.Do(context.Background(), Action{Kind: ActRunnerStop})
	if err != nil || !strings.Contains(msg, "when the current job finishes") {
		t.Fatalf("msg=%q err=%v", msg, err)
	}
	if h.e.pending != "stop" {
		t.Fatalf("pending = %q", h.e.pending)
	}
	h.e.refresh() // still busy: nothing happens
	if h.e.pending != "stop" || !h.e.signaled.IsZero() {
		t.Fatalf("busy runner must not be signaled (pending=%q signaled=%v)", h.e.pending, h.e.signaled)
	}
	// Job finishes: the engine tries to SIGINT the listener.
	h.procs.WorkerPID = 0
	h.e.refresh()
	if h.e.notice == nil || !strings.Contains(h.e.notice.Message, "stop failed") {
		t.Fatalf("expected an interrupt attempt once idle, notice=%+v", h.e.notice)
	}
}

func TestAbortPending(t *testing.T) {
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: 1, WorkerPID: 2}
	h.e.refresh()
	h.e.Do(context.Background(), Action{Kind: ActRunnerRestart})
	if h.e.pending != "restart" {
		t.Fatalf("pending = %q", h.e.pending)
	}
	if _, err := h.e.Do(context.Background(), Action{Kind: ActRunnerAbort}); err != nil || h.e.pending != "" {
		t.Fatalf("abort: err=%v pending=%q", err, h.e.pending)
	}
}

func TestLabelsMatch(t *testing.T) {
	runner := []string{"self-hosted", "linux", "x64", "laptop"}
	for labels, want := range map[string]bool{
		"self-hosted":        true,
		"self-hosted,Linux":  true,
		"self-hosted,laptop": true,
		"ubuntu-latest":      false,
		"self-hosted,gpu":    false,
		"":                   false,
	} {
		var l []string
		if labels != "" {
			l = strings.Split(labels, ",")
		}
		if got := labelsMatch(l, runner); got != want {
			t.Errorf("labelsMatch(%q) = %v", labels, got)
		}
	}
}

func TestScanErrorKeepsStateAndDoesNotFlood(t *testing.T) {
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: 1}
	h.write("Worker_1.log", workerLog("a", "acme/a", "Succeeded", t0))
	h.write("Worker_2.log", workerLog("b", "acme/b", "Failed", t0.Add(time.Minute)))
	h.e.refresh()
	diag := filepath.Join(h.dir, "_diag")
	os.Rename(diag, diag+".gone") // runner dir temporarily unreadable
	h.e.refresh()
	if len(h.e.state.Jobs) != 2 || !h.e.state.Processes.Running() {
		t.Fatalf("state lost on scan error: %d jobs, procs %+v", len(h.e.state.Jobs), h.e.state.Processes)
	}
	os.Rename(diag+".gone", diag)
	h.e.refresh()
	if n := h.notifications(); len(n) != 0 {
		t.Fatalf("old jobs re-notified after a scan error: %v", n)
	}
}

func TestAbandonedNeedsTwoScans(t *testing.T) {
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: 1, WorkerPID: 2}
	h.write("Worker_1.log", workerLog("a", "acme/a", "", t0))
	h.e.refresh()
	// The worker exits between the log read and the process scan: looks abandoned once…
	h.procs.WorkerPID = 0
	h.e.refresh()
	if n := h.notifications(); len(n) != 0 {
		t.Fatalf("notified on first abandoned sighting: %v", n)
	}
	// …then the log shows the real result.
	h.write("Worker_1.log", workerLog("a", "acme/a", "Succeeded", t0))
	h.e.refresh()
	n := h.notifications()
	if len(n) != 1 || !strings.Contains(n[0], "Succeeded") {
		t.Fatalf("want the real result notified once, got %v", n)
	}
}

func TestStartIsSingleFlight(t *testing.T) {
	h := newHarness(t)
	os.WriteFile(filepath.Join(h.dir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	h.e.ctl.LogFile = filepath.Join(t.TempDir(), "runner.out")
	if _, err := h.e.Do(context.Background(), Action{Kind: ActRunnerStart}); err != nil {
		t.Fatal(err)
	}
	// The listener hasn't appeared yet: a second start must be refused.
	_, err := h.e.Do(context.Background(), Action{Kind: ActRunnerStart})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second start: %v", err)
	}
	// run.sh alive (between listener restarts) also blocks a start.
	h.e.lastStart = time.Time{}
	h.procs = fleet.Processes{SupervisorPID: 42}
	if _, err := h.e.Do(context.Background(), Action{Kind: ActRunnerStart}); err == nil {
		t.Fatal("start allowed while run.sh is alive")
	}
}

func TestSignaledStopTimesOut(t *testing.T) {
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: 1}
	h.e.refresh()
	h.e.mu.Lock()
	h.e.pending, h.e.signaled = "stop", time.Now().Add(-2*signalTimeout)
	h.e.mu.Unlock()
	h.e.refresh()
	if h.e.pending != "" || h.e.notice == nil || h.e.notice.OK {
		t.Fatalf("pending=%q notice=%+v", h.e.pending, h.e.notice)
	}
}

func TestWatchAfterCloseIsClosed(t *testing.T) {
	h := newHarness(t)
	h.e.closeSubs()
	select {
	case _, ok := <-h.e.Watch(context.Background()):
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("Watch after close blocks")
	}
}

func TestRestartRightAfterStart(t *testing.T) {
	h := newHarness(t)
	os.WriteFile(filepath.Join(h.dir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	h.e.ctl.LogFile = filepath.Join(t.TempDir(), "runner.out")
	if _, err := h.e.Do(context.Background(), Action{Kind: ActRunnerStart}); err != nil {
		t.Fatal(err)
	}
	h.procs = fleet.Processes{ListenerPID: 999999999} // the started runner came up
	h.e.refresh()
	// It is restarted (and has exited) within the 30s start guard.
	h.procs = fleet.Processes{}
	h.e.mu.Lock()
	h.e.restarting = true
	h.e.mu.Unlock()
	h.e.restartAfterStop()
	if n := h.e.notice; n == nil || !n.OK || !strings.Contains(n.Message, "restarted") {
		t.Fatalf("restart after a recent start: %+v", n)
	}
}

func TestUsageOfRunningJob(t *testing.T) {
	// A "worker" whose child burns CPU.
	cmd := exec.Command("sh", "-c", "while :; do :; done & sleep 5; kill $!")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: os.Getpid(), WorkerPID: cmd.Process.Pid}
	h.write("Worker_1.log", workerLog("job1", "acme/a", "", time.Now()))
	h.e.refresh()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.e.usageLoop(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		time.Sleep(400 * time.Millisecond)
		h.e.pokeUsage()
	}
	time.Sleep(200 * time.Millisecond)
	u, _ := h.e.Usage(ctx)
	if u.Job == nil || u.Job.JobID != "job1" || u.Job.Current.CPU < 40 || u.Job.Current.Procs < 2 || len(u.Job.History) < 2 {
		t.Fatalf("live usage = %+v", u.Job)
	}
	if u.Runner.Procs != 1 || u.Runner.RSS == 0 {
		t.Errorf("runner usage = %+v", u.Runner)
	}
	// The job finishes: its summary is kept, saved, and shown on its row.
	h.procs.WorkerPID = 0
	h.write("Worker_1.log", workerLog("job1", "acme/a", "Succeeded", time.Now()))
	h.e.refresh()
	h.e.pokeUsage()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	h.e.refresh()
	sum := h.e.state.Jobs[0].Usage
	if sum == nil || sum.PeakRSS == 0 || sum.CPUSeconds <= 0 || sum.AvgCPU < 20 {
		t.Fatalf("summary = %+v", sum)
	}
	if saved := loadUsage(h.e.usageFile); saved["job1"].PeakRSS != sum.PeakRSS {
		t.Errorf("summary not saved: %+v", saved)
	}
}
