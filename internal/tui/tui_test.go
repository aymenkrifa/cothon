package tui

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/aymenkrifa/cothon/internal/engine"
	"github.com/aymenkrifa/cothon/internal/fleet"
)

func job(name, repo, run, result string, min int) engine.JobView {
	j := &fleet.Job{Name: name, Repository: repo, RunID: run, RunNumber: run, RunAttempt: "1", Workflow: "CI",
		Ref: "refs/heads/main", Result: result, Status: fleet.StatusCompleted, CheckRunID: "1" + run,
		Started: time.Now().Add(-time.Duration(min) * time.Minute), Finished: time.Now().Add(-time.Duration(min-1) * time.Minute)}
	if result == "" {
		j.Status, j.Finished = fleet.StatusRunning, time.Time{}
	}
	return engine.JobView{Job: j, HTMLURL: j.HTMLURL(), RunURL: j.RunURL()}
}

func render(t *testing.T, st *engine.State, keys ...string) (*model, string) {
	t.Helper()
	m := &model{ctx: context.Background(), source: "test", collapsed: map[string]bool{}}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.Update(stateMsg{st: st})
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m.Update(msg)
	}
	return m, stripANSI(m.View())
}

func testState() *engine.State {
	return &engine.State{
		Runner:    &fleet.RunnerConfig{Name: "laptop"},
		Processes: fleet.Processes{ListenerPID: 1, WorkerPID: 2},
		Jobs: []engine.JobView{
			job("backend", "acme/app", "63", "", 1),
			job("Test (2/2)", "acme/tt", "424", "Failed", 10),
			job("Test (1/2)", "acme/tt", "424", "Succeeded", 12),
			job("frontend", "acme/app", "62", "Succeeded", 20),
		},
	}
}

func TestGroupedViewDefault(t *testing.T) {
	_, out := render(t, testState())
	for _, want := range []string{"▾ acme/app", "▸ CI #63", "▾ acme/tt", "▸ CI #424", "✗ 1", "▸ CI #62"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Test (1/2)") || strings.Contains(out, "frontend") {
		t.Error("runs should start collapsed")
	}
	// Expanding a run lists its jobs in execution order: down x3 → CI #424 row (app, #63, #62, tt, #424).
	_, out = render(t, testState(), "down", "down", "down", "down", "right")
	if !strings.Contains(out, "▾ CI #424") || strings.Index(out, "Test (1/2)") > strings.Index(out, "Test (2/2)") || !strings.Contains(out, "Test (1/2)") {
		t.Errorf("expanded run should list jobs in execution order:\n%s", out)
	}
}

func TestFilterKeepsFolding(t *testing.T) {
	// A status filter narrows the tree but does not force runs open, and
	// folding still works while it is active.
	m, out := render(t, testState(), "F") // status: failed
	if !strings.Contains(out, "▸ CI #424") || strings.Contains(out, "CI #62") {
		t.Fatalf("failed filter should show only the failing run, collapsed:\n%s", out)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	out = stripANSI(m.View())
	if !strings.Contains(out, "▾ CI #424") || !strings.Contains(out, "Test (2/2)") {
		t.Fatalf("expand under filter:\n%s", out)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	out = stripANSI(m.View())
	if !strings.Contains(out, "▸ CI #424") {
		t.Fatalf("collapse under filter:\n%s", out)
	}
}

func TestCollapseAndListToggle(t *testing.T) {
	// Cursor starts on the first repo; ← collapses it.
	_, out := render(t, testState(), "left")
	if !strings.Contains(out, "▸ acme/app") || strings.Contains(out, "CI #63  ⎇") {
		t.Errorf("repo should collapse:\n%s", out)
	}
	m, out := render(t, testState(), "v")
	if !m.list || !strings.Contains(out, "JOB") || !strings.Contains(out, "frontend") {
		t.Errorf("v should switch to the flat list:\n%s", out)
	}
}

func TestRunActionsFromGroupHeader(t *testing.T) {
	// down → first run row (CI #63, running); c asks to cancel that run.
	m, _ := render(t, testState(), "down", "c")
	if m.confirm == nil || m.confirm.action.Kind != engine.ActCancelRun || m.confirm.action.RunID != "63" {
		t.Fatalf("confirm = %+v", m.confirm)
	}
}

// TestRenderReal prints the TUI for the real runner (COTHON_REAL_RUNNER_DIR, -v).
func TestRenderReal(t *testing.T) {
	dir := os.Getenv("COTHON_REAL_RUNNER_DIR")
	if dir == "" {
		t.Skip("COTHON_REAL_RUNNER_DIR not set")
	}
	sc := fleet.NewScanner(dir)
	sc.Procs = func() fleet.Processes { return fleet.FindProcesses(dir) }
	snap, err := sc.Scan()
	if err != nil {
		t.Fatal(err)
	}
	st := &engine.State{Runner: snap.Runner, Listener: snap.Listener, Processes: *snap.Processes}
	for _, j := range snap.Jobs[:min(60, len(snap.Jobs))] {
		st.Jobs = append(st.Jobs, engine.JobView{Job: j, HTMLURL: j.HTMLURL(), RunURL: j.RunURL()})
	}
	_, out := render(t, st)
	t.Log("\n" + out)
}

type fakeBackend struct{ st *engine.State }

func (f fakeBackend) State(context.Context) (*engine.State, error) { return f.st, nil }
func (f fakeBackend) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	go func() { <-ctx.Done(); close(ch) }() // blocks like a live daemon stream
	return ch
}
func (f fakeBackend) Do(context.Context, engine.Action) (string, error) { return "ok", nil }
func (f fakeBackend) Usage(context.Context) (*engine.Usage, error) {
	return &engine.Usage{Cores: 8}, nil
}

// TestProgramQuits runs the real Bubble Tea program loop with scripted keys
// and checks that "q" exits promptly even though Watch never fires.
func TestProgramQuits(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), fakeBackend{testState()}, "test", Options{},
			tea.WithInput(strings.NewReader("jjvFq")), tea.WithOutput(io.Discard))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TUI did not quit on q")
	}
}

func TestSelectionFollowsItemWhenJobsArrive(t *testing.T) {
	st := testState()
	m, _ := render(t, st, "down", "down", "down", "down") // CI #424 run row
	if k := m.selKey(); !strings.HasPrefix(k, "run:acme/tt|424") {
		t.Fatalf("selected %q", k)
	}
	// A new job (new repo) arrives at the top.
	st2 := *st
	st2.Jobs = append([]engine.JobView{job("lint", "acme/zz", "900", "", 0)}, st.Jobs...)
	m.Update(stateMsg{st: &st2})
	if k := m.selKey(); !strings.HasPrefix(k, "run:acme/tt|424") {
		t.Errorf("selection moved to %q", k)
	}
}

func TestNeverOverflowsTerminal(t *testing.T) {
	st := testState()
	st.Jobs[1].Job.Name = "テスト 🚀 very long job name with wide characters and more text"
	for _, size := range [][2]int{{120, 30}, {80, 24}, {60, 12}, {40, 10}, {30, 8}, {20, 6}} {
		for _, list := range []bool{false, true} {
			m := &model{ctx: context.Background(), source: "test", collapsed: map[string]bool{}, list: list}
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.Update(stateMsg{st: st})
			m.Update(tea.KeyMsg{Type: tea.KeyDown})
			m.Update(tea.KeyMsg{Type: tea.KeyDown})
			m.Update(tea.KeyMsg{Type: tea.KeyDown})
			m.Update(tea.KeyMsg{Type: tea.KeyDown})
			m.Update(tea.KeyMsg{Type: tea.KeyRight})
			lines := strings.Split(m.View(), "\n")
			if len(lines) > size[1] {
				t.Errorf("%dx%d list=%v: %d lines", size[0], size[1], list, len(lines))
			}
			for _, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Errorf("%dx%d list=%v: line %d wide: %q", size[0], size[1], list, w, stripANSI(l))
					break
				}
			}
		}
	}
}

func TestBranchNames(t *testing.T) {
	for in, want := range map[string]string{"refs/heads/main": "main", "refs/pull/223/merge": "PR #223", "refs/tags/v1": "v1", "": ""} {
		if got := branch(in); got != want {
			t.Errorf("branch(%q) = %q", in, got)
		}
	}
}

func TestQueueTab(t *testing.T) {
	st := testState()
	st.Queue = &engine.QueueState{Jobs: []engine.QueuedJob{
		{Repo: "acme/tt", Workflow: "CI", RunID: "424", RunNumber: 424, JobID: "9", Name: "Test (2/4)", Branch: "main", Queued: time.Now().Add(-2 * time.Minute)},
	}}
	m, out := render(t, st)
	if !strings.Contains(out, "1 queued") || !strings.Contains(out, "Queue (1)") {
		t.Fatalf("header/tab should show the queue:\n%s", out)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	out = stripANSI(m.View())
	if !strings.Contains(out, "Test (2/4)") || !strings.Contains(out, "waiting 2m") {
		t.Fatalf("queue view:\n%s", out)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if m.confirm == nil || m.confirm.action.Kind != engine.ActCancelRun || m.confirm.action.RunID != "424" {
		t.Fatalf("c on a queued job: %+v", m.confirm)
	}
}
