package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aymenkrifa/cothon/internal/engine"
	"github.com/aymenkrifa/cothon/internal/fleet"
)

type fakeBackend struct {
	st    *engine.State
	did   []engine.Action
	usage *engine.Usage
}

func (f *fakeBackend) State(context.Context) (*engine.State, error) { return f.st, nil }
func (f *fakeBackend) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	go func() { <-ctx.Done(); close(ch) }()
	return ch
}
func (f *fakeBackend) Usage(context.Context) (*engine.Usage, error) {
	if f.usage != nil {
		return f.usage, nil
	}
	return &engine.Usage{Cores: 8}, nil
}
func (f *fakeBackend) Do(_ context.Context, a engine.Action) (string, error) {
	f.did = append(f.did, a)
	return "ok", nil
}

func newTestServer(t *testing.T) (*fakeBackend, http.Handler) {
	t.Helper()
	job := &fleet.Job{ID: "g", CheckRunID: "99", Name: "Test (1/2)", Repository: "acme/widgets", Workflow: "CI",
		RunID: "7", RunNumber: "12", RunAttempt: "2", Ref: "refs/heads/main", Status: fleet.StatusCompleted,
		Result: "Failed", Started: time.Now().Add(-time.Hour), Finished: time.Now().Add(-59 * time.Minute)}
	run := &fleet.Job{ID: "h", CheckRunID: "100", Name: "<script>alert(1)</script>", Repository: "acme/widgets",
		Workflow: "CI", RunID: "8", RunNumber: "13", Status: fleet.StatusRunning, Started: time.Now().Add(-time.Minute)}
	st := &engine.State{
		Version:   3,
		Runner:    &fleet.RunnerConfig{Name: "laptop", GitHubURL: "https://github.com/acme"},
		Processes: fleet.Processes{ListenerPID: 1, WorkerPID: 2},
		Jobs: []engine.JobView{
			{Job: run, HTMLURL: run.HTMLURL(), RunURL: run.RunURL()},
			{Job: job, HTMLURL: job.HTMLURL(), RunURL: job.RunURL()},
		},
		Org: &engine.OrgState{Name: "acme", Runs: []engine.OrgRun{{Repo: "acme/api", Workflow: "CI", RunID: "5", Status: "queued",
			Jobs: []engine.OrgJob{{ID: "1", Name: "build", Status: "queued", OnThisRunner: true}}}}},
	}
	fb := &fakeBackend{st: st}
	s, err := New(fb, "secret-token", "127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	return fb, s.Handler()
}

func do(h http.Handler, method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7777"
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessCookie is what a signed-in browser sends: the derived browser secret,
// never the bearer token itself.
var sessCookie = "cothon_session_7777=" + (&Server{Token: "secret-token"}).browserSecret()

var auth = map[string]string{"Cookie": sessCookie}

func TestPageNeedsLogin(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(h, "GET", "/grouped?status=failed", nil, "")
	if rec.Code != 401 || rec.Header().Get("Set-Cookie") != "" || !strings.Contains(rec.Body.String(), "cothon web") {
		t.Fatalf("unauthenticated page: %d cookie=%q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	if strings.Contains(rec.Body.String(), "widgets") {
		t.Error("login page leaks data")
	}
}

func TestLoginFlow(t *testing.T) {
	s, err := New(&fakeBackend{st: &engine.State{}}, "secret-token", "127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	code := s.NewLoginCode()
	rec := do(h, "GET", "/login?code="+code+"&next=%2Flist%2Facme%2Fwidgets%3Fstatus%3Dfailed", nil, "")
	c := rec.Header().Get("Set-Cookie")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/list/acme/widgets?status=failed" {
		t.Fatalf("login = %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(c, sessCookie) || strings.Contains(c, "secret-token") || !strings.Contains(c, "HttpOnly") ||
		!strings.Contains(c, "SameSite=Strict") || !strings.Contains(c, "Max-Age=") {
		t.Errorf("cookie = %q", c)
	}
	// One-time.
	if rec := do(h, "GET", "/login?code="+code, nil, ""); rec.Code != 401 || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("code reuse: %d", rec.Code)
	}
	// Open redirects are refused.
	for _, next := range []string{"//evil.example", "https://evil.example", "/\\evil.example", "javascript:alert(1)"} {
		rec := do(h, "GET", "/login?code="+s.NewLoginCode()+"&next="+url.QueryEscape(next), nil, "")
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Errorf("next=%q redirected to %q", next, loc)
		}
	}
	// Codes come only from the CLI's bearer secret, never from the cookie.
	if rec := do(h, "POST", "/api/login-code", map[string]string{"Cookie": sessCookie, "X-Cothon": "1"}, ""); rec.Code != 401 {
		t.Errorf("cookie minted a code: %d", rec.Code)
	}
	rec = do(h, "POST", "/api/login-code", map[string]string{"Authorization": "Bearer secret-token", "X-Cothon": "1"}, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"code"`) {
		t.Errorf("bearer login-code: %d %s", rec.Code, rec.Body)
	}
}

func TestPageRenders(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(h, "GET", "/", auth, "")
	if rec.Code != 200 {
		t.Fatalf("GET / = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"laptop", "Test (1/2)", "widgets", "Rerun failed jobs", `data-version="3"`, "build ★", "Cancel run"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("job name not escaped")
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("pages must not hand out the session cookie")
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Error("missing CSP")
	}
}

func TestHostCheckBlocksRebinding(t *testing.T) {
	_, h := newTestServer(t)
	for _, host := range []string{"evil.example:7777", "127.0.0.1:8080", "attacker.com"} {
		if rec := do(h, "GET", "/", map[string]string{"Host": host}, ""); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q: got %d", host, rec.Code)
		}
	}
	if rec := do(h, "GET", "/", map[string]string{"Host": "localhost:7777", "Cookie": sessCookie}, ""); rec.Code != 200 {
		t.Errorf("localhost: got %d", rec.Code)
	}
}

func TestAPIRequiresToken(t *testing.T) {
	_, h := newTestServer(t)
	for _, p := range []string{"/api/state", "/fragment"} {
		if rec := do(h, "GET", p, nil, ""); rec.Code != 401 {
			t.Errorf("%s without token: %d", p, rec.Code)
		}
		if rec := do(h, "GET", p, map[string]string{"Authorization": "Bearer wrong"}, ""); rec.Code != 401 {
			t.Errorf("%s with wrong token: %d", p, rec.Code)
		}
		if rec := do(h, "GET", p, map[string]string{"Authorization": "Bearer secret-token"}, ""); rec.Code != 200 {
			t.Errorf("%s with bearer: %d", p, rec.Code)
		}
		if rec := do(h, "GET", p, map[string]string{"Cookie": sessCookie}, ""); rec.Code != 200 {
			t.Errorf("%s with cookie: %d", p, rec.Code)
		}
	}
}

func TestActionCSRFProtection(t *testing.T) {
	fb, h := newTestServer(t)
	body := `{"kind":"cancel-run","repo":"acme/widgets","run_id":"7"}`
	bearer := "Bearer secret-token"
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no token", map[string]string{"X-Cothon": "1"}, 401},
		{"no custom header (simple form post)", map[string]string{"Cookie": sessCookie, "Content-Type": "text/plain"}, 403},
		{"cross-site fetch", map[string]string{"Cookie": sessCookie, "X-Cothon": "1", "Sec-Fetch-Site": "cross-site"}, 403},
		{"foreign origin", map[string]string{"Authorization": bearer, "X-Cothon": "1", "Origin": "http://evil.example"}, 403},
		{"same-origin browser", map[string]string{"Cookie": sessCookie, "X-Cothon": "1", "Origin": "http://127.0.0.1:7777", "Sec-Fetch-Site": "same-origin"}, 200},
		{"tui client", map[string]string{"Authorization": bearer, "X-Cothon": "1"}, 200},
	}
	for _, c := range cases {
		before := len(fb.did)
		rec := do(h, "POST", "/api/action", c.hdr, body)
		if rec.Code != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
		if executed := len(fb.did) > before; executed != (c.want == 200) {
			t.Errorf("%s: executed=%v", c.name, executed)
		}
	}
}

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:7777": true, "localhost:1": true, "[::1]:7777": true, "0.0.0.0:7777": false, ":7777": false, "192.168.1.5:7777": false} {
		if err := CheckLoopback(addr); (err == nil) != ok {
			t.Errorf("CheckLoopback(%q) = %v", addr, err)
		}
	}
}

func TestIdle(t *testing.T) {
	s := &Server{lastLeft: time.Now().Add(-3 * time.Minute)}
	if !s.Idle(time.Minute, 2*time.Minute) {
		t.Error("never-connected server past grace should be idle")
	}
	s.clientJoined()
	if s.Idle(0, 0) {
		t.Error("connected client: not idle")
	}
	s.clientLeft()
	if s.Idle(time.Minute, 0) {
		t.Error("just left: not idle yet")
	}
}

func TestGrouping(t *testing.T) {
	mk := func(name, repo, run, attempt, result string, start int) engine.JobView {
		j := &fleet.Job{Name: name, Repository: repo, RunID: run, RunAttempt: attempt, Result: result, Status: fleet.StatusCompleted,
			Started: time.Date(2026, 11, 17, 10, start, 0, 0, time.UTC), Finished: time.Date(2026, 11, 17, 10, start+1, 0, 0, time.UTC)}
		if result == "" {
			j.Status = fleet.StatusRunning
		}
		return engine.JobView{Job: j}
	}
	// newest first, as the engine provides them; runs of two repos interleave.
	jobs := []engine.JobView{
		mk("backend", "acme/app", "63", "1", "", 50),
		mk("Test (2/2)", "acme/tt", "424", "2", "Failed", 40),
		mk("frontend", "acme/app", "62", "1", "Succeeded", 30),
		mk("Test (1/2)", "acme/tt", "424", "2", "Succeeded", 20),
		mk("deploy", "acme/tt", "161", "1", "Succeeded", 10),
	}
	repos := engine.GroupJobs(jobs)
	if len(repos) != 2 || repos[0].Repo != "acme/app" || repos[1].Repo != "acme/tt" {
		t.Fatalf("repos = %+v", repos)
	}
	app, tt := repos[0], repos[1]
	if app.Outcome != "running" || app.Running != 1 || app.OK != 1 || len(app.Runs) != 2 {
		t.Errorf("app = %+v", app)
	}
	if tt.Outcome != "failed" || len(tt.Runs) != 2 || tt.Runs[0].RunID != "424" { // newest run failed
		t.Fatalf("tt = %+v", tt)
	}
	r := tt.Runs[0]
	if r.Jobs[0].Name != "Test (1/2)" || r.Jobs[1].Name != "Test (2/2)" {
		t.Errorf("jobs not in execution order: %s, %s", r.Jobs[0].Name, r.Jobs[1].Name)
	}
	if r.Span() != 21*time.Minute {
		t.Errorf("span = %v", r.Span())
	}
}

func TestViews(t *testing.T) {
	_, h := newTestServer(t)
	grouped := do(h, "GET", "/fragment", auth, "").Body.String()
	if !strings.Contains(grouped, `class="jobs grouped"`) || !strings.Contains(grouped, `class="repo-row toggle"`) {
		t.Error("default view should be grouped")
	}
	list := do(h, "GET", "/fragment?view=list", auth, "").Body.String()
	if !strings.Contains(list, `class="jobs list"`) || strings.Contains(list, "repo-row") {
		t.Error("?view=list should render the flat list")
	}
	withCookie := do(h, "GET", "/", map[string]string{"Cookie": viewCookie + "=list; " + sessCookie}, "").Body.String()
	if !strings.Contains(withCookie, `class="jobs list"`) {
		t.Error("view cookie should pick the initial view")
	}
}

func TestURLRoutes(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(h, "GET", "/list/acme/widgets?status=failed&q=test", auth, "")
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="jobs list"`,
		`<option value="acme/widgets" selected>`,
		`data-v="failed" aria-pressed="true" class="on"`,
		`value="test"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Grouped view by path; unknown repo stays selectable; bad status ignored.
	body = do(h, "GET", "/grouped/acme/other?status=bogus", auth, "").Body.String()
	if !strings.Contains(body, `class="jobs grouped"`) || !strings.Contains(body, `<option value="acme/other" selected>`) || !strings.Contains(body, `data-v="" aria-pressed="true" class="on"`) {
		t.Error("grouped route / unknown repo / bad status")
	}
	// Path wins over the view cookie.
	body = do(h, "GET", "/grouped", map[string]string{"Cookie": viewCookie + "=list; " + sessCookie}, "").Body.String()
	if !strings.Contains(body, `class="jobs grouped"`) {
		t.Error("path should override cookie")
	}
	for _, p := range []string{"/grouped/a/b/c", "/tree", "/list/..%2F..%2Fx/y"} {
		if c := do(h, "GET", p, auth, "").Code; c == 200 && strings.Contains(p, "%2F") {
			if strings.Contains(do(h, "GET", p, auth, "").Body.String(), "selected>..") {
				t.Errorf("%s: traversal accepted as repo", p)
			}
		} else if c != 404 && !strings.Contains(p, "%2F") {
			t.Errorf("%s = %d, want 404", p, c)
		}
	}
}

func TestRendersEmptyAndHostileData(t *testing.T) {
	evil := `"><img src=x onerror=alert(1)>`
	j := &fleet.Job{ID: "x", Name: evil, Repository: "acme/w", Workflow: evil, Ref: evil, Actor: evil, RunID: "1",
		Result: evil, Status: fleet.StatusCompleted, ServerURL: "javascript:alert(1)//"}
	empty := &fleet.Job{ID: "y", Status: fleet.StatusCompleted, Result: "Succeeded"}
	for name, st := range map[string]*engine.State{
		"empty":   {},
		"orgwarn": {Org: &engine.OrgState{Name: "acme", Warning: "no access"}},
		"hostile": {Runner: &fleet.RunnerConfig{Name: evil, GitHubURL: "javascript:alert(1)"},
			Jobs: []engine.JobView{{Job: j, HTMLURL: j.HTMLURL(), RunURL: j.RunURL()}, {Job: empty}}},
	} {
		s, _ := New(&fakeBackend{st: st}, "secret-token", "127.0.0.1:7777")
		h := s.Handler()
		for _, p := range []string{"/grouped", "/list"} {
			rec := do(h, "GET", p, auth, "")
			body := rec.Body.String()
			if rec.Code != 200 {
				t.Fatalf("%s %s: %d", name, p, rec.Code)
			}
			if strings.Contains(body, "<img src=x") || strings.Contains(body, `href="javascript:`) {
				t.Errorf("%s %s: unescaped hostile content", name, p)
			}
			if strings.Contains(body, `href=""`) || strings.Contains(body, `data-job=""`) || strings.Contains(body, `data-run=""`) {
				t.Errorf("%s %s: broken control for empty fields", name, p)
			}
		}
	}
}

func TestStaticNoListingAndGzip(t *testing.T) {
	_, h := newTestServer(t)
	if c := do(h, "GET", "/static/", nil, "").Code; c != 404 {
		t.Errorf("/static/ = %d", c)
	}
	rec := do(h, "GET", "/fragment", map[string]string{"Cookie": sessCookie, "Accept-Encoding": "gzip"}, "")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Error("fragment not gzipped")
	}
}

func TestCookieIsNotABearer(t *testing.T) {
	_, h := newTestServer(t)
	secret := strings.TrimPrefix(sessCookie, "cothon_session_7777=")
	if rec := do(h, "POST", "/api/login-code", map[string]string{"Authorization": "Bearer " + secret, "X-Cothon": "1"}, ""); rec.Code != 401 {
		t.Errorf("browser secret minted a login code: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/state", map[string]string{"Cookie": "cothon_session_7777=secret-token"}, ""); rec.Code != 401 {
		t.Errorf("raw token accepted as cookie: %d", rec.Code)
	}
}

func TestGzipOnlyForSuccessfulGets(t *testing.T) {
	_, h := newTestServer(t)
	gz := map[string]string{"Accept-Encoding": "gzip"}
	if rec := do(h, "GET", "/static/nope.js", gz, ""); rec.Code != 404 || rec.Header().Get("Content-Encoding") != "" || !strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("404: enc=%q body=%q", rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
	rec := do(h, "GET", "/static/app.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-10"}, "")
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Range") != "" {
		t.Errorf("range: %d enc=%q range=%q", rec.Code, rec.Header().Get("Content-Encoding"), rec.Header().Get("Content-Range"))
	}
	if rec := do(h, "HEAD", "/static/app.js", gz, ""); rec.Header().Get("Content-Encoding") != "" {
		t.Error("HEAD should not be compressed")
	}
	if rec := do(h, "GET", "/static/app.js", map[string]string{"Accept-Encoding": "gzip;q=0"}, ""); rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 still compressed")
	}
}

func TestQueuePanelAndInlineIcons(t *testing.T) {
	fb, h := newTestServer(t)
	fb.st.Queue = &engine.QueueState{Jobs: []engine.QueuedJob{{Repo: "acme/api", Workflow: "CI", RunID: "9", RunNumber: 4,
		JobID: "1", Name: "Test (2/4)", Branch: "main", Queued: time.Now().Add(-90 * time.Second), HTMLURL: "https://github.com/acme/api/actions/runs/9/job/1"}}}
	body := do(h, "GET", "/grouped", auth, "").Body.String()
	for _, want := range []string{"Queued for this runner", "Test (2/4)", `aria-label="queued"`, `<kbd aria-hidden="true">/</kbd>`, "Stop and Restart wait for the current job to finish", "Avg CPU", `class="stat-label">Peak memory`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Repo rows carry no status icon; run and job rows carry theirs inline.
	repoRow := body[strings.Index(body, `class="repo-row`):]
	repoRow = repoRow[:strings.Index(repoRow, "</tr>")]
	if strings.Contains(repoRow, `class="st `) {
		t.Error("repo row should not have a status icon")
	}
	if !strings.Contains(body, `<td class="title job"><span class="st`) {
		t.Error("job status icon should be inline in the title cell")
	}
}

// Every row of both tables must have as many cells (counting colspan) as the header.
func TestTableColumnsMatch(t *testing.T) {
	_, h := newTestServer(t)
	cellRe := regexp.MustCompile(`<t[dh]\b[^>]*>`)
	spanRe := regexp.MustCompile(`colspan="(\d+)"`)
	for _, view := range []string{"/grouped", "/list"} {
		body := do(h, "GET", view, auth, "").Body.String()
		table := body[strings.Index(body, `<table class="jobs`):]
		table = table[:strings.Index(table, "</table>")]
		var want int
		for i, row := range strings.Split(table, "<tr")[1:] {
			row = row[:strings.Index(row, "</tr>")]
			n := 0
			for _, tag := range cellRe.FindAllString(row, -1) {
				if m := spanRe.FindStringSubmatch(tag); m != nil {
					c, _ := strconv.Atoi(m[1])
					n += c
				} else {
					n++
				}
			}
			if i == 0 {
				want = n
				continue
			}
			if n != want {
				t.Errorf("%s row %d has %d cells, header has %d: %.120s", view, i, n, want, row)
			}
		}
	}
}
