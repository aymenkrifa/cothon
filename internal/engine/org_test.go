package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aymenkrifa/cothon/internal/fleet"
	"github.com/aymenkrifa/cothon/internal/gh"
)

func fakeGitHub(t *testing.T, orgStatus int, calls, notModified *atomic.Int32) *httptest.Server {
	pushed := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		etag := `"` + r.URL.String() + `"`
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("X-RateLimit-Remaining", "4999")
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/acme/repos"):
			if orgStatus != 200 {
				w.WriteHeader(orgStatus)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
				return
			}
			fmt.Fprintf(w, `[{"full_name":"acme/api","pushed_at":%q},{"full_name":"acme/old","pushed_at":"2020-01-01T00:00:00Z"}]`, pushed)
		case r.URL.Path == "/repos/acme/api/actions/runs" && r.URL.Query().Get("status") == "queued":
			fmt.Fprint(w, `{"workflow_runs":[{"id":555,"name":"CI","run_number":9,"run_attempt":1,"status":"queued","head_branch":"main","event":"push","html_url":"https://github.com/acme/api/actions/runs/555","created_at":"2026-11-17T10:00:00Z","actor":{"login":"octo"}}]}`)
		case r.URL.Path == "/repos/acme/api/actions/runs":
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case r.URL.Path == "/repos/acme/api/actions/runs/555/jobs":
			fmt.Fprint(w, `{"jobs":[{"id":1,"name":"build","status":"queued","labels":["self-hosted","Linux"]},{"id":2,"name":"mac","status":"queued","labels":["macos-latest"]}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
}

func TestPollOrg(t *testing.T) {
	var calls, nm atomic.Int32
	srv := fakeGitHub(t, 200, &calls, &nm)
	defer srv.Close()
	h := newHarness(t)
	h.e.refresh()
	h.e.api = gh.NewClient(func(context.Context) (string, error) { return "tok", nil })
	h.e.api.BaseURL = srv.URL

	if err := h.e.pollOrg(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := h.e.org
	if o.Name != "acme" || o.Warning != "" || len(o.Runs) != 1 {
		t.Fatalf("org = %+v", o)
	}
	r := o.Runs[0]
	if r.RunID != "555" || len(r.Jobs) != 2 || !r.Jobs[0].OnThisRunner || r.Jobs[1].OnThisRunner {
		t.Fatalf("run = %+v", r)
	}
	if o.RateRemaining != 4999 {
		t.Errorf("rate = %d", o.RateRemaining)
	}
	// acme/old is outside the recent window and must not be polled.
	first := calls.Load()
	if first != 4 { // repos, queued, in_progress, jobs
		t.Errorf("first poll made %d calls, want 4", first)
	}
	// Second poll: everything unchanged -> all 304s, same result.
	if err := h.e.pollOrg(context.Background()); err != nil {
		t.Fatal(err)
	}
	if nm.Load() != 4 || len(h.e.org.Runs) != 1 {
		t.Errorf("second poll: %d not-modified, runs=%d", nm.Load(), len(h.e.org.Runs))
	}
	// Actions on org runs are allowed (they are "known").
	if !h.e.knownRun("acme/api", "555") || !h.e.knownJob("acme/api", "1") {
		t.Error("org run/job should be actionable")
	}
}

func TestPollOrgForbiddenFallsBackWithWarning(t *testing.T) {
	var calls, nm atomic.Int32
	srv := fakeGitHub(t, 403, &calls, &nm)
	defer srv.Close()
	h := newHarness(t)
	h.e.refresh()
	h.e.api = gh.NewClient(func(context.Context) (string, error) { return "tok", nil })
	h.e.api.BaseURL = srv.URL

	if err := h.e.pollOrg(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if w := h.e.org.Warning; !strings.Contains(w, "no access to acme") || !strings.Contains(w, "local runner only") {
		t.Errorf("warning = %q", w)
	}
	h.e.refresh()
	found := false
	for _, w := range h.e.state.Warnings {
		found = found || strings.Contains(w, "no access to acme")
	}
	if !found {
		t.Error("warning not surfaced in state")
	}
}

func TestPollQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/a/actions/runs" && r.URL.Query().Get("status") == "in_progress":
			// A matrix: one leg runs here, two legs wait for this runner, one wants macOS.
			fmt.Fprint(w, `{"workflow_runs":[{"id":7,"name":"CI","run_number":3,"status":"in_progress","head_branch":"main","event":"push","actor":{"login":"octo"}}]}`)
		case r.URL.Path == "/repos/acme/a/actions/runs":
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case r.URL.Path == "/repos/acme/a/actions/runs/7/jobs":
			fmt.Fprint(w, `{"jobs":[
				{"id":42,"name":"build","status":"in_progress","labels":["self-hosted"]},
				{"id":43,"name":"Test (2/2)","status":"queued","labels":["self-hosted","linux"],"created_at":"2026-11-17T10:01:00Z","html_url":"https://github.com/acme/a/actions/runs/7/job/43"},
				{"id":44,"name":"Test (1/2)","status":"queued","labels":["self-hosted"],"created_at":"2026-11-17T10:00:00Z"},
				{"id":45,"name":"mac","status":"queued","labels":["macos-latest"]}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	h := newHarness(t)
	h.procs = fleet.Processes{ListenerPID: 1, WorkerPID: 2}
	h.write("Worker_1.log", workerLog("a", "acme/a", "", time.Now().Add(-time.Minute))) // job 42 runs here
	h.e.refresh()
	h.e.queueAPI = gh.NewClient(func(context.Context) (string, error) { return "tok", nil })
	h.e.queueAPI.BaseURL = srv.URL
	if err := h.e.pollQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.e.refresh()
	q := h.e.state.Queue
	if q == nil || len(q.Jobs) != 2 || q.Jobs[0].Name != "Test (1/2)" || q.Jobs[1].JobID != "43" {
		t.Fatalf("queue = %+v", q)
	}
	if !h.e.knownRun("acme/a", "7") {
		t.Error("a queued run should be cancelable")
	}
	// Job 43 starts here: it leaves the queue before the next poll.
	h.write("Worker_2.log", strings.Replace(workerLog("b", "acme/a", "", time.Now()), `"v": 42.0`, `"v": 43.0`, 1))
	h.e.refresh()
	if q := h.e.state.Queue; len(q.Jobs) != 1 || q.Jobs[0].JobID != "44" {
		t.Errorf("started job still queued: %+v", q.Jobs)
	}
}

// A repo that fails to answer keeps its last known queued jobs, and only
// repeated failures show a warning.
func TestPollQueueTransientFailure(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(502)
			return
		}
		switch {
		case r.URL.Path == "/repos/acme/a/actions/runs" && r.URL.Query().Get("status") == "queued":
			fmt.Fprint(w, `{"workflow_runs":[{"id":7,"name":"CI","run_number":3}]}`)
		case r.URL.Path == "/repos/acme/a/actions/runs":
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case r.URL.Path == "/repos/acme/a/actions/runs/7/jobs":
			fmt.Fprint(w, `{"jobs":[{"id":44,"name":"test","status":"queued","labels":["self-hosted"]}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	h := newHarness(t)
	h.write("Worker_1.log", workerLog("a", "acme/a", "", time.Now().Add(-time.Minute)))
	h.e.refresh()
	h.e.queueAPI = gh.NewClient(func(context.Context) (string, error) { return "tok", nil })
	h.e.queueAPI.BaseURL = srv.URL
	if err := h.e.pollQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	for i := 1; i <= queueWarnAfter; i++ {
		err := h.e.pollQueue(context.Background())
		h.e.mu.Lock()
		q := h.e.queue
		h.e.mu.Unlock()
		if len(q.Jobs) != 1 || q.Jobs[0].JobID != "44" {
			t.Fatalf("poll %d: last known job dropped: %+v", i, q.Jobs)
		}
		if warn := i >= queueWarnAfter; (q.Warning != "") != warn || (err != nil) != warn {
			t.Fatalf("poll %d: warning = %q, err = %v", i, q.Warning, err)
		}
	}
	down.Store(false)
	if err := h.e.pollQueue(context.Background()); err != nil || h.e.queue.Warning != "" {
		t.Fatalf("after recovery: err = %v, warning = %q", err, h.e.queue.Warning)
	}
}
