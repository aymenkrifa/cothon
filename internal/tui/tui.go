// Package tui is the terminal UI. It works on any engine.Backend: a running
// daemon or an in-process engine.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/aymenkrifa/cothon/internal/browser"
	"github.com/aymenkrifa/cothon/internal/engine"
	"github.com/aymenkrifa/cothon/internal/fleet"
)

// Options configure the TUI.
type Options struct {
	// Browser opens links: "" = desktop default, "none" = don't open (show
	// the link instead), else a command such as "firefox".
	Browser string
}

// Run starts the TUI. source is shown in the header ("daemon 127.0.0.1:7777" or "standalone").
func Run(ctx context.Context, b engine.Backend, source string, o Options, opts ...tea.ProgramOption) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := &model{ctx: ctx, b: b, source: source, browser: o.Browser, watch: b.Watch(ctx), collapsed: map[string]bool{}}
	opts = append([]tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}, opts...)
	_, err := tea.NewProgram(m, opts...).Run()
	if err == tea.ErrProgramKilled {
		return nil
	}
	return err
}

// ---------- messages ----------

type (
	usageMsg struct{ u *engine.Usage }
	stateMsg struct {
		st  *engine.State
		err error
	}
	watchMsg     struct{}
	watchDoneMsg struct{}
	tickMsg      time.Time
	clearFlash   struct{ at time.Time }
	actionMsg    struct {
		msg string
		err error
	}
)

// ---------- model ----------

type view int

const (
	viewJobs view = iota
	viewQueue
	viewOrg
)

type confirm struct {
	prompt string
	action engine.Action
}

type model struct {
	ctx    context.Context
	b      engine.Backend
	source string
	watch  <-chan struct{}

	st       *engine.State
	err      error
	lost     bool
	width    int
	height   int
	view     view
	cursor   int
	offset   int
	ticking  bool
	flash    string
	flashErr bool
	flashAt  time.Time
	confirm  *confirm
	noticeAt time.Time // last engine notice shown
	usage    *engine.Usage
	browser  string

	filtering    bool
	filter       string
	statusFilter string // "", failed, succeeded, canceled, running

	list      bool            // flat list instead of repo → run → job
	collapsed map[string]bool // user overrides of the default open state
}

type entryKind int

const (
	eRepo entryKind = iota
	eRun
	eJob
)

// entry is one line of the jobs view.
type entry struct {
	kind entryKind
	repo *engine.RepoGroup
	run  *engine.RunGroup
	job  *engine.JobView
	open bool
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.waitWatch())
}

// fetchUsage asks for live CPU/memory (only while a job runs: the card is
// the only place showing live numbers).
func (m *model) fetchUsage() tea.Cmd {
	if !m.running() {
		return nil
	}
	return func() tea.Msg {
		u, err := m.b.Usage(m.ctx)
		if err != nil {
			return nil
		}
		return usageMsg{u}
	}
}

func (m *model) fetch() tea.Cmd {
	return func() tea.Msg {
		st, err := m.b.State(m.ctx)
		return stateMsg{st, err}
	}
}

func (m *model) waitWatch() tea.Cmd {
	ch := m.watch
	return func() tea.Msg {
		if _, ok := <-ch; !ok {
			return watchDoneMsg{}
		}
		return watchMsg{}
	}
}

// tick redraws the clock-driven parts: every second while a job runs (the
// elapsed timer), otherwise every 30s (the "x min ago" labels).
func (m *model) tick() tea.Cmd {
	d := 30 * time.Second
	if m.running() {
		d = time.Second
	}
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) do(a engine.Action) tea.Cmd {
	m.flash, m.flashErr, m.flashAt = "working…", false, time.Now()
	return func() tea.Msg {
		msg, err := m.b.Do(m.ctx, a)
		return actionMsg{msg, err}
	}
}

const flashFor = 8 * time.Second

func (m *model) setFlash(s string, isErr bool) tea.Cmd {
	at := time.Now()
	m.flash, m.flashErr, m.flashAt = s, isErr, at
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return clearFlash{at} })
}

func (m *model) running() bool {
	return m.st != nil && m.st.RunningJob() != nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case stateMsg:
		if msg.err != nil {
			m.err = msg.err
			if m.st != nil {
				return m, m.setFlash("refresh failed: "+msg.err.Error(), true)
			}
			return m, nil
		}
		// Keep the selection on the same item when new jobs arrive above it.
		key := m.selKey()
		m.st, m.err = msg.st, nil
		if i := m.indexOf(key); i >= 0 {
			m.offset += i - m.cursor
			m.cursor = i
		}
		m.clampCursor()
		var cmds []tea.Cmd
		// Results of deferred operations (stop when idle, restart) arrive as notices.
		if n := m.st.Notice; n != nil && n.At.After(m.noticeAt) {
			if !m.noticeAt.IsZero() || time.Since(n.At) < flashFor {
				cmds = append(cmds, m.setFlash(n.Message, !n.OK))
			}
			m.noticeAt = n.At
		}
		if !m.ticking {
			m.ticking = true
			cmds = append(cmds, m.tick())
		}
		return m, tea.Batch(cmds...)

	case watchMsg:
		return m, tea.Batch(m.fetch(), m.waitWatch())

	case watchDoneMsg:
		m.lost = true
		if m.source == "standalone" {
			m.flash, m.flashErr, m.flashAt = "stopped watching the runner logs — restart cothon tui", true, time.Now().Add(24*time.Hour)
		} else {
			m.flash, m.flashErr, m.flashAt = "disconnected from the daemon — restart cothon tui", true, time.Now().Add(24*time.Hour)
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.tick(), m.fetchUsage())

	case usageMsg:
		m.usage = msg.u
		return m, nil

	case clearFlash:
		if msg.at.Equal(m.flashAt) {
			m.flash = ""
		}
		return m, nil

	case actionMsg:
		var c tea.Cmd
		if msg.err != nil {
			c = m.setFlash(msg.err.Error(), true)
		} else {
			c = m.setFlash(msg.msg, false)
		}
		return m, tea.Batch(c, m.fetch())

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Keys that arrive in one read (fast typing, paste) come as one
	// multi-rune event; outside the filter box, handle them one by one.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !m.filtering && !k.Paste {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			_, cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.filtering {
				break // "/" opened the filter: the rest is text, not commands
			}
		}
		return m, tea.Batch(cmds...) // not Sequence: a flash timer must not delay a quit
	}
	s := k.String()
	var cmd tea.Cmd
	if s == "ctrl+c" {
		return m, tea.Quit
	}
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		if s == "y" || s == "Y" {
			return m, m.do(c.action)
		}
		return m, m.setFlash("canceled", false)
	}
	if m.filtering {
		switch k.Type {
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyEsc:
			m.filtering, m.filter = false, ""
		case tea.KeyBackspace:
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		case tea.KeyCtrlU:
			m.filter = ""
		case tea.KeyCtrlW:
			f := strings.TrimRight(m.filter, " ")
			if i := strings.LastIndex(f, " "); i >= 0 {
				m.filter = f[:i+1]
			} else {
				m.filter = ""
			}
		case tea.KeyRunes, tea.KeySpace:
			if !k.Alt {
				m.filter += string(k.Runes)
			}
		}
		m.cursor, m.offset = 0, 0
		return m, nil
	}

	switch s {
	case "q", "esc":
		if s == "esc" && (m.filter != "" || m.statusFilter != "") {
			m.filter, m.statusFilter = "", ""
			return m, nil
		}
		return m, tea.Quit
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup":
		m.cursor -= m.listHeight()
	case "pgdown", " ":
		m.cursor += m.listHeight()
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = 1 << 30
	case "tab":
		views := m.views()
		if len(views) == 1 {
			cmd = m.setFlash("only the jobs view is on (queue/org polling are off in the config)", false)
			break
		}
		for i, v := range views {
			if v == m.view {
				m.view = views[(i+1)%len(views)]
				break
			}
		}
		m.cursor, m.offset = 0, 0
	case "/":
		m.filtering = true
	case "F":
		order := []string{"", "failed", "succeeded", "canceled", "running"}
		for i, v := range order {
			if v == m.statusFilter {
				m.statusFilter = order[(i+1)%len(order)]
				break
			}
		}
		m.cursor, m.offset = 0, 0
	case "v":
		m.list = !m.list
		m.cursor, m.offset = 0, 0
		if m.list {
			cmd = m.setFlash("list view", false)
		} else {
			cmd = m.setFlash("grouped view (repo → run → job)", false)
		}
	case "left", "h", "right", "l":
		if e := m.selectedEntry(); e != nil && e.kind != eJob {
			m.setOpen(e, s == "right" || s == "l")
		} else if e != nil && (s == "left" || s == "h") {
			// On a job: collapse its run and move the cursor to it.
			for i, x := range m.entries() {
				if x.kind == eRun && x.run.Key == runKey(*e.job) {
					m.setOpen(&x, false)
					m.cursor = i
					break
				}
			}
		}
	case "enter", "o":
		if e := m.selectedEntry(); s == "enter" && e != nil && e.kind != eJob {
			m.setOpen(e, !e.open)
			break
		}
		cmd = m.open(m.selectedURL(false))
	case "O":
		cmd = m.open(m.selectedURL(true))
	case "r":
		if j := m.selectedJob(); j != nil && j.Status != fleet.StatusRunning && j.CheckRunID != "" {
			m.confirm = &confirm{fmt.Sprintf("Rerun job %q in %s?", j.Name, j.Repository),
				engine.Action{Kind: engine.ActRerunJob, Repo: j.Repository, JobID: j.CheckRunID}}
		}
	case "f":
		if r := m.selectedRun(); r != nil && r.Failed+r.Canceled == 0 {
			cmd = m.setFlash("nothing failed in this run", false)
		} else if r != nil {
			m.confirm = &confirm{fmt.Sprintf("Rerun failed jobs of %s %s #%s?", r.Repo, r.Workflow, r.RunNumber),
				engine.Action{Kind: engine.ActRerunFailed, Repo: r.Repo, RunID: r.RunID}}
		} else if j := m.selectedJob(); j != nil && j.Status != fleet.StatusRunning && j.Result != "Succeeded" && j.RunID != "" {
			m.confirm = &confirm{fmt.Sprintf("Rerun failed jobs of %s run #%s?", j.Repository, j.RunNumber),
				engine.Action{Kind: engine.ActRerunFailed, Repo: j.Repository, RunID: j.RunID}}
		} else if r := m.selectedOrgRun(); r != nil {
			m.confirm = &confirm{fmt.Sprintf("Rerun failed jobs of %s #%d?", r.Repo, r.RunNumber),
				engine.Action{Kind: engine.ActRerunFailed, Repo: r.Repo, RunID: r.RunID}}
		}
	case "c":
		if q := m.selectedQueued(); q != nil {
			m.confirm = &confirm{fmt.Sprintf("Cancel run %s #%d (queued job %q)?", q.Repo, q.RunNumber, q.Name),
				engine.Action{Kind: engine.ActCancelRun, Repo: q.Repo, RunID: q.RunID}}
		} else if r := m.selectedRun(); r != nil && r.Running == 0 {
			cmd = m.setFlash("this run is not running", false)
		} else if r != nil {
			m.confirm = &confirm{fmt.Sprintf("Cancel the whole run %s %s #%s? Jobs on other runners stop too.", r.Repo, r.Workflow, r.RunNumber),
				engine.Action{Kind: engine.ActCancelRun, Repo: r.Repo, RunID: r.RunID}}
		} else if j := m.selectedJob(); j != nil && j.Status == fleet.StatusRunning && j.RunID != "" {
			m.confirm = &confirm{fmt.Sprintf("Cancel the whole run %s #%s? Jobs on other runners stop too.", j.Repository, j.RunNumber),
				engine.Action{Kind: engine.ActCancelRun, Repo: j.Repository, RunID: j.RunID}}
		} else if r := m.selectedOrgRun(); r != nil {
			m.confirm = &confirm{fmt.Sprintf("Cancel run %s #%d?", r.Repo, r.RunNumber),
				engine.Action{Kind: engine.ActCancelRun, Repo: r.Repo, RunID: r.RunID}}
		}
	case "s":
		m.confirm = &confirm{"Start the runner?", engine.Action{Kind: engine.ActRunnerStart}}
	case "x":
		m.confirm = &confirm{"Stop the runner (waits for the current job)?", engine.Action{Kind: engine.ActRunnerStop}}
	case "X":
		m.confirm = &confirm{"Stop the runner NOW (cancels a running job)?", engine.Action{Kind: engine.ActRunnerStop, Force: true}}
	case "R":
		m.confirm = &confirm{"Restart the runner (waits for the current job)?", engine.Action{Kind: engine.ActRunnerRestart}}
	case "a":
		return m, m.do(engine.Action{Kind: engine.ActRunnerAbort})
	}
	m.clampCursor()
	return m, cmd
}

// ---------- selection ----------

func (m *model) visibleJobs() []engine.JobView {
	if m.st == nil {
		return nil
	}
	q := strings.ToLower(m.filter)
	var out []engine.JobView
	for _, j := range m.st.Jobs {
		o := outcome(j)
		if m.statusFilter != "" && o != m.statusFilter && !(m.statusFilter == "failed" && o == "abandoned") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(j.Repository+" "+j.Workflow+" "+j.Name+" "+j.Ref+" "+j.Actor), q) {
			continue
		}
		out = append(out, j)
	}
	return out
}

// views lists the tabs available right now.
func (m *model) views() []view {
	v := []view{viewJobs}
	if m.st != nil && m.st.Queue != nil {
		v = append(v, viewQueue)
	}
	if m.st != nil && m.st.Org != nil {
		v = append(v, viewOrg)
	}
	return v
}

func (m *model) queued() []engine.QueuedJob {
	if m.st == nil || m.st.Queue == nil {
		return nil
	}
	return m.st.Queue.Jobs
}

func (m *model) selectedQueued() *engine.QueuedJob {
	q := m.queued()
	if m.view != viewQueue || m.cursor < 0 || m.cursor >= len(q) {
		return nil
	}
	return &q[m.cursor]
}

func (m *model) itemCount() int {
	if m.view == viewQueue {
		return len(m.queued())
	}
	if m.view == viewOrg {
		if m.st == nil || m.st.Org == nil {
			return 0
		}
		return len(m.st.Org.Runs)
	}
	return len(m.entries())
}

func runKey(j engine.JobView) string { return j.Repository + "|" + j.RunID + "|" + j.RunAttempt }

// entries flattens the jobs view into lines: the flat list, or the
// repo → run → job tree honoring collapse state.
func (m *model) entries() []entry {
	jobs := m.visibleJobs()
	var out []entry
	if m.list {
		for i := range jobs {
			out = append(out, entry{kind: eJob, job: &jobs[i]})
		}
		return out
	}
	// Repos start open, runs start collapsed; filters only narrow what is
	// listed and never override the user's folding.
	repos := engine.GroupJobs(jobs)
	for ri := range repos {
		rg := &repos[ri]
		open := m.isOpen("repo:"+rg.Repo, true)
		out = append(out, entry{kind: eRepo, repo: rg, open: open})
		if !open {
			continue
		}
		for ui := range rg.Runs {
			run := &rg.Runs[ui]
			ropen := m.isOpen("run:"+run.Key, false)
			out = append(out, entry{kind: eRun, repo: rg, run: run, open: ropen})
			if !ropen {
				continue
			}
			for ji := range run.Jobs {
				out = append(out, entry{kind: eJob, repo: rg, run: run, job: &run.Jobs[ji]})
			}
		}
	}
	return out
}

func (m *model) isOpen(key string, def bool) bool {
	if v, ok := m.collapsed[key]; ok {
		return !v
	}
	return def
}

func (m *model) setOpen(e *entry, open bool) {
	switch e.kind {
	case eRepo:
		m.collapsed["repo:"+e.repo.Repo] = !open
	case eRun:
		m.collapsed["run:"+e.run.Key] = !open
	}
}

func (m *model) selectedEntry() *entry {
	if m.view != viewJobs {
		return nil
	}
	es := m.entries()
	if m.cursor < 0 || m.cursor >= len(es) {
		return nil
	}
	return &es[m.cursor]
}

func (m *model) selectedRun() *engine.RunGroup {
	if e := m.selectedEntry(); e != nil && e.kind == eRun {
		return e.run
	}
	return nil
}

// selKey identifies the selected item, so the selection can follow it when
// the list changes.
func (m *model) selKey() string { return m.keyAt(m.cursor) }

func (m *model) keyAt(i int) string {
	if m.st == nil || i < 0 {
		return ""
	}
	if m.view == viewQueue {
		if q := m.queued(); i < len(q) {
			return "queue:" + q[i].JobID
		}
		return ""
	}
	if m.view == viewOrg {
		if m.st.Org == nil || i >= len(m.st.Org.Runs) {
			return ""
		}
		return "org:" + m.st.Org.Runs[i].Repo + "|" + m.st.Org.Runs[i].RunID
	}
	es := m.entries()
	if i >= len(es) {
		return ""
	}
	switch e := es[i]; e.kind {
	case eRepo:
		return "repo:" + e.repo.Repo
	case eRun:
		return "run:" + e.run.Key
	default:
		return "job:" + e.job.ID + "|" + e.job.LogFile
	}
}

func (m *model) indexOf(key string) int {
	if key == "" {
		return -1
	}
	for i := 0; i < m.itemCount(); i++ {
		if m.keyAt(i) == key {
			return i
		}
	}
	return -1
}

func (m *model) clampCursor() {
	n := m.itemCount()
	m.cursor = max(0, min(m.cursor, n-1))
	h := max(1, m.listHeight())
	if m.view == viewOrg {
		h = max(1, h/2) // each org run takes two lines
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, min(m.offset, max(0, n-h)))
}

func (m *model) selectedJob() *engine.JobView {
	if e := m.selectedEntry(); e != nil && e.kind == eJob {
		return e.job
	}
	return nil
}

func (m *model) selectedOrgRun() *engine.OrgRun {
	if m.view != viewOrg || m.st == nil || m.st.Org == nil || m.cursor >= len(m.st.Org.Runs) {
		return nil
	}
	return &m.st.Org.Runs[m.cursor]
}

func (m *model) selectedURL(run bool) string {
	if e := m.selectedEntry(); e != nil && e.kind == eRun {
		return e.run.RunURL
	}
	if e := m.selectedEntry(); e != nil && e.kind == eRepo {
		return "https://github.com/" + e.repo.Repo + "/actions"
	}
	if j := m.selectedJob(); j != nil {
		if run {
			return j.RunURL
		}
		return j.HTMLURL
	}
	if r := m.selectedOrgRun(); r != nil {
		return r.HTMLURL
	}
	if q := m.selectedQueued(); q != nil {
		return q.HTMLURL
	}
	return ""
}

// open opens a GitHub link with the configured browser.
func (m *model) open(u string) tea.Cmd {
	switch {
	case u == "":
		return m.setFlash("no link for this item", false)
	case !strings.HasPrefix(u, "https://"):
		return m.setFlash("not opening a non-https link", true)
	case m.browser == "none":
		return m.setFlash(u, false)
	}
	if err := browser.Open(u, m.browser); err != nil {
		return m.setFlash("could not open the browser: "+err.Error(), true)
	}
	return m.setFlash("opened "+u, false)
}

func outcome(j engine.JobView) string { return engine.Outcome(j) }

// ---------- view ----------

var (
	cOK     = lipgloss.AdaptiveColor{Light: "#1f8a4c", Dark: "#3fbf72"}
	cFail   = lipgloss.AdaptiveColor{Light: "#cf3b3b", Dark: "#ff6b6b"}
	cRun    = lipgloss.AdaptiveColor{Light: "#b07508", Dark: "#f0b43c"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#646d78", Dark: "#8b95a3"}
	cAccent = lipgloss.AdaptiveColor{Light: "#3b6fd8", Dark: "#6e9bff"}

	sTitle  = lipgloss.NewStyle().Bold(true)
	sMuted  = lipgloss.NewStyle().Foreground(cMuted)
	sOK     = lipgloss.NewStyle().Foreground(cOK)
	sFail   = lipgloss.NewStyle().Foreground(cFail)
	sRun    = lipgloss.NewStyle().Foreground(cRun)
	sAccent = lipgloss.NewStyle().Foreground(cAccent)
	sSel    = lipgloss.NewStyle().Reverse(true)
	sCard   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cRun).Padding(0, 1)
	sTab    = lipgloss.NewStyle().Padding(0, 1)
	sTabOn  = lipgloss.NewStyle().Padding(0, 1).Bold(true).Underline(true)
)

// showCard: the running-job card is dropped on short terminals.
func (m *model) showCard() bool { return m.running() && m.height >= 15 && m.width >= 30 }

// warnings shown above the list, capped so the list keeps some room.
func (m *model) warnings() []string {
	w := m.st.Warnings
	if limit := max(0, m.height-5); len(w) > limit {
		w = w[:limit]
	}
	return w
}

func (m *model) listHeight() int {
	used := 4 // header, tabs, column header, footer
	if m.showCard() {
		used += 5
	}
	if m.st != nil {
		used += len(m.warnings())
	}
	return max(1, m.height-used)
}

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	if m.st == nil {
		if m.err != nil {
			return sFail.Render("error: "+m.err.Error()) + "\n" + sMuted.Render("q to quit")
		}
		return sMuted.Render("loading…")
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteByte('\n')
	for _, w := range m.warnings() {
		b.WriteString(sRun.Render(trunc("⚠ "+w, m.width)))
		b.WriteByte('\n')
	}
	if j := m.st.RunningJob(); j != nil && m.showCard() {
		b.WriteString(m.runningCard(*j))
		b.WriteByte('\n')
	}
	b.WriteString(m.tabs())
	b.WriteByte('\n')
	switch m.view {
	case viewOrg:
		b.WriteString(m.orgList())
	case viewQueue:
		b.WriteString(m.queueList())
	default:
		b.WriteString(m.jobList())
	}
	b.WriteString(m.footer())
	return b.String()
}

func (m *model) header() string {
	st := m.st
	name := "runner"
	if st.Runner != nil {
		name = st.Runner.Name
	}
	status := st.RunnerStatus()
	dot := map[string]lipgloss.Style{"idle": sOK, "busy": sRun, "stopping": sRun, "starting": sRun, "stopped": sFail}[status].Render("●")
	left := sTitle.Render("cothon") + sMuted.Render(" · ") + sTitle.Render(name) + "  " + dot + " " + status
	if st.Processes.ListenerPID != 0 {
		left += sMuted.Render(fmt.Sprintf("  pid %d", st.Processes.ListenerPID))
	}
	if q := m.queued(); len(q) > 0 {
		left += sAccent.Render(fmt.Sprintf("  %d queued", len(q)))
	}
	if st.Pending != "" {
		left += sRun.Render("  (" + st.Pending + " when idle — a to abort)")
	}
	if st.Processes.Listeners > 1 {
		left += sFail.Render(fmt.Sprintf("  %d listeners!", st.Processes.Listeners))
	}
	right := sMuted.Render(m.source)
	if m.lost {
		right = sFail.Render("disconnected")
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return trunc(left, m.width)
	}
	return trunc(left+strings.Repeat(" ", gap)+right, m.width)
}

func (m *model) runningCard(j engine.JobView) string {
	elapsed := fmtDur(j.Duration(time.Now()))
	line1 := sRun.Render("▶ ") + sTitle.Render(j.Name) + sMuted.Render("  "+elapsed)
	if u := m.usage; u != nil && u.Job != nil && u.Job.JobID == j.ID {
		line1 += sMuted.Render(fmt.Sprintf("   CPU %.0f%% · %s · peak %s · %d procs",
			u.Job.Current.CPU, fmtBytes(u.Job.Current.RSS), fmtBytes(u.Job.Summary.PeakRSS), u.Job.Current.Procs))
	}
	line2 := j.Repository + sMuted.Render(" · "+j.Workflow+" #"+j.RunNumber)
	if j.RunAttempt != "" && j.RunAttempt != "1" {
		line2 += sMuted.Render(" (attempt " + j.RunAttempt + ")")
	}
	var parts []string
	if j.Ref != "" {
		parts = append(parts, "⎇ "+branch(j.Ref))
	}
	if j.SHA != "" {
		parts = append(parts, short(j.SHA))
	}
	if j.Actor != "" {
		parts = append(parts, j.Event+" by "+j.Actor)
	}
	line3 := sMuted.Render(strings.Join(parts, "  "))
	w := max(10, m.width-4)
	return sCard.Width(w).Render(trunc(line1, w-2) + "\n" + trunc(line2, w-2) + "\n" + trunc(line3, w-2))
}

func (m *model) tabs() string {
	mode := "grouped"
	if m.list {
		mode = "list"
	}
	jobsLabel := fmt.Sprintf("Jobs · %s (%d)", mode, len(m.visibleJobs()))
	tabs := []string{}
	if m.view == viewJobs {
		tabs = append(tabs, sTabOn.Render(jobsLabel))
	} else {
		tabs = append(tabs, sTab.Render(jobsLabel))
	}
	if m.st.Queue != nil {
		label := fmt.Sprintf("Queue (%d)", len(m.st.Queue.Jobs))
		if m.view == viewQueue {
			tabs = append(tabs, sTabOn.Render(label))
		} else {
			tabs = append(tabs, sTab.Render(label))
		}
	}
	if m.st.Org != nil {
		label := fmt.Sprintf("Org %s (%d)", m.st.Org.Name, len(m.st.Org.Runs))
		if m.view == viewOrg {
			tabs = append(tabs, sTabOn.Render(label))
		} else {
			tabs = append(tabs, sTab.Render(label))
		}
	}
	s := strings.Join(tabs, " ")
	var f []string
	if m.statusFilter != "" {
		f = append(f, "status:"+m.statusFilter)
	}
	if m.filtering {
		f = append(f, "/"+m.filter+"▏")
	} else if m.filter != "" {
		f = append(f, "/"+m.filter)
	}
	if len(f) > 0 {
		s += "  " + sAccent.Render(strings.Join(f, "  "))
	}
	return trunc(s, m.width)
}

type col struct {
	title string
	width int
}

func (m *model) jobList() string {
	if !m.list {
		return m.groupedList()
	}
	jobs := m.visibleJobs()
	// Column widths adapt to the terminal; optional columns drop on narrow screens.
	cols := m.listCols()
	var b strings.Builder
	b.WriteString(sMuted.Render(trunc("  "+row(cols[1:], func(i int) string { return cols[i+1].title }), m.width)))
	b.WriteByte('\n')
	h := m.listHeight()
	lines := 0
	if len(jobs) == 0 {
		b.WriteString(sMuted.Render("  no jobs") + "\n")
		lines++
	}
	now := time.Now()
	for i := m.offset; i < len(jobs) && lines < h; i++ {
		j := jobs[i]
		started := ""
		if !j.Started.IsZero() {
			started = ago(now.Sub(j.Started))
		}
		cells := []string{j.Name, shortRepo(j.Repository), j.Workflow + " #" + j.RunNumber, branch(j.Ref), started, peakMem(j), fmtDur(j.Duration(now))}
		rest := row(cols[1:], func(c int) string { return cells[c] })
		if i == m.cursor {
			rest = sSel.Render(rest)
		}
		b.WriteString(trunc(statusIcon(outcome(j))+" "+rest, m.width) + "\n")
		lines++
	}
	for ; lines < h; lines++ {
		b.WriteByte('\n')
	}
	return b.String()
}

func (m *model) listCols() []col {
	w := m.width
	cols := []col{{"", 2}, {"JOB", 22}, {"REPO", 22}, {"WORKFLOW", 20}, {"BRANCH", 16}, {"STARTED", 12}, {"PEAK MEM", 8}, {"DURATION", 9}}
	if w < 100 {
		cols[4].width = 0
	}
	if w < 90 {
		cols[6].width = 0
	}
	if w < 80 {
		cols[5].width = 0
		cols[3].width = 14
	}
	if w < 70 {
		cols[2].width = 0
	}
	fixed := 0
	for i, c := range cols {
		if i != 1 && c.width > 0 {
			fixed += c.width + 1
		}
	}
	cols[1].width = max(8, w-fixed-3)
	return cols
}

// groupedList renders repo → run → job lines with ▸/▾ markers.
func (m *model) groupedList() string {
	es := m.entries()
	var b strings.Builder
	b.WriteString(sMuted.Render(trunc("  REPO · RUN · JOB    ←/→ fold · v list view", m.width)))
	b.WriteByte('\n')
	h := m.listHeight()
	lines := 0
	if len(es) == 0 {
		b.WriteString(sMuted.Render("  no jobs") + "\n")
		lines++
	}
	now := time.Now()
	rightW := 22 // "started  duration" column
	if m.width < 50 {
		rightW = 0 // too narrow: names first
	}
	for i := m.offset; i < len(es) && lines < h; i++ {
		e := es[i]
		caret := "▸ "
		if e.open {
			caret = "▾ "
		}
		var icon, left, right string
		switch e.kind {
		case eRepo:
			icon = " " // a repo has no single status: its counts say it all
			left = caret + sTitle.Render(e.repo.Repo) + "  " + counts(e.repo.OK, e.repo.Failed, e.repo.Canceled, e.repo.Running)
			right = ago(now.Sub(e.repo.Latest))
		case eRun:
			r := e.run
			icon = statusIcon(r.Outcome)
			if e.open {
				icon = " " // an expanded run hands its status to its jobs: one spinner per job
			}
			label := r.Workflow + " #" + r.RunNumber
			if r.Attempt != "" && r.Attempt != "1" {
				label += " (attempt " + r.Attempt + ")"
			}
			left = "   " + caret + label + sMuted.Render("  ⎇ "+branch(r.Ref)) + "  " + counts(r.OK, r.Failed, r.Canceled, r.Running)
			right = ago(now.Sub(r.Started)) + "  " + fmtDur(r.Span())
		case eJob:
			j := e.job
			icon = statusIcon(outcome(*j))
			left = "        " + j.Name
			right = fmtDur(j.Duration(now))
			if mem := peakMem(*j); mem != "" {
				right = mem + "  " + right
			}
		}
		lw := max(0, m.width-2-rightW-1)
		line := pad(trunc(left, lw), lw)
		if rightW > 0 {
			line += " " + fmt.Sprintf("%*s", rightW, stripANSI(right))
		}
		if i == m.cursor {
			line = sSel.Render(stripANSI(line))
		}
		b.WriteString(icon + " " + line + "\n")
		lines++
	}
	for ; lines < h; lines++ {
		b.WriteByte('\n')
	}
	return b.String()
}

func counts(ok, failed, canceled, running int) string {
	var p []string
	if running > 0 {
		p = append(p, sRun.Render(fmt.Sprintf("● %d", running)))
	}
	if ok > 0 {
		p = append(p, sOK.Render(fmt.Sprintf("✓ %d", ok)))
	}
	if failed > 0 {
		p = append(p, sFail.Render(fmt.Sprintf("✗ %d", failed)))
	}
	if canceled > 0 {
		p = append(p, sMuted.Render(fmt.Sprintf("⊘ %d", canceled)))
	}
	return strings.Join(p, " ")
}

// queueList shows jobs waiting for this runner, the next one first.
func (m *model) queueList() string {
	q := m.queued()
	var b strings.Builder
	h := m.listHeight()
	b.WriteString(sMuted.Render(trunc("  QUEUED FOR THIS RUNNER (next first)    c cancel run · enter open", m.width)) + "\n")
	lines := 0
	if len(q) == 0 {
		b.WriteString(sMuted.Render("  nothing queued") + "\n")
		lines++
	}
	now := time.Now()
	for i := m.offset; i < len(q) && lines < h; i++ {
		j := q[i]
		left := j.Name + sMuted.Render("  "+shortRepo(j.Repo)+" · "+j.Workflow+fmt.Sprintf(" #%d", j.RunNumber))
		if j.Branch != "" {
			left += sMuted.Render("  ⎇ " + branch(j.Branch))
		}
		right := "waiting " + fmtDur(now.Sub(j.Queued))
		lw := max(0, m.width-2-len(right)-2)
		line := pad(trunc(left, lw), lw) + "  " + right
		if i == m.cursor {
			line = sSel.Render(stripANSI(line))
		}
		b.WriteString(trunc(sAccent.Render("◌")+" "+line, m.width) + "\n")
		lines++
	}
	for ; lines < h; lines++ {
		b.WriteByte('\n')
	}
	return b.String()
}

func (m *model) orgList() string {
	if m.st.Org == nil {
		return ""
	}
	o := m.st.Org
	var b strings.Builder
	h := m.listHeight()
	if len(o.Runs) == 0 {
		b.WriteString(sMuted.Render("  nothing queued or in progress") + "\n")
	}
	now := time.Now()
	lines := 0
	for i := m.offset; i < len(o.Runs) && lines < h; i++ {
		r := o.Runs[i]
		st := sRun
		if r.Status == "queued" {
			st = sAccent
		}
		head := fmt.Sprintf("%-11s %s · %s #%d  ⎇ %s  %s", st.Render(r.Status), shortRepo(r.Repo), r.Workflow, r.RunNumber, r.Branch, sMuted.Render(ago(now.Sub(r.Created))))
		head = trunc(head, m.width)
		if i == m.cursor {
			head = sSel.Render(head)
		}
		b.WriteString(head + "\n")
		lines++
		var chips []string
		for _, j := range r.Jobs {
			label := j.Name
			if j.OnThisRunner {
				label += "★"
			}
			switch {
			case j.Status == "queued":
				chips = append(chips, sAccent.Render(label))
			case j.Status == "in_progress":
				chips = append(chips, sRun.Render(label))
			case j.Conclusion == "success":
				chips = append(chips, sOK.Render(label))
			case j.Conclusion == "failure":
				chips = append(chips, sFail.Render(label))
			default:
				chips = append(chips, sMuted.Render(label))
			}
		}
		if len(chips) > 0 && lines < h {
			b.WriteString("    " + trunc(strings.Join(chips, sMuted.Render(" · ")), m.width-4) + "\n")
			lines++
		}
	}
	for ; lines < h; lines++ {
		b.WriteByte('\n')
	}
	return b.String()
}

func (m *model) footer() string {
	if m.confirm != nil {
		return sRun.Render(trunc(m.confirm.prompt+"  (y/N)", m.width))
	}
	if m.flash != "" && time.Since(m.flashAt) < flashFor {
		st := sOK
		if m.flashErr {
			st = sFail
		}
		return st.Render(trunc(m.flash, m.width))
	}
	help := "↑↓ move  ←→ fold  enter open/fold  v view  r rerun  f rerun failed  c cancel run  / filter  F status  s/x/R runner  tab org  q quit"
	return sMuted.Render(trunc(help, m.width))
}

// ---------- helpers ----------

func row(cols []col, cell func(int) string) string {
	var parts []string
	for i, c := range cols {
		if c.width == 0 {
			continue
		}
		parts = append(parts, pad(trunc(cell(i), c.width), c.width))
	}
	return strings.Join(parts, " ")
}

func statusIcon(o string) string {
	switch o {
	case "succeeded":
		return sOK.Render("✓")
	case "failed":
		return sFail.Render("✗")
	case "abandoned":
		return sFail.Render("▲")
	case "running":
		return sRun.Render("●")
	case "canceled":
		return sMuted.Render("⊘")
	}
	return sMuted.Render("?")
}

// trunc cuts s to n terminal columns, measuring wide characters correctly
// and keeping colors.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func ago(d time.Duration) string {
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Round(time.Minute).Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Round(time.Hour).Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func branch(ref string) string {
	// refs/pull/223/merge → PR #223
	if rest, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		if n, _, ok := strings.Cut(rest, "/"); ok {
			return "PR #" + n
		}
	}
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, p) {
			return strings.TrimPrefix(ref, p)
		}
	}
	return ref
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func shortRepo(r string) string {
	if _, n, ok := strings.Cut(r, "/"); ok {
		return n
	}
	return r
}

// peakMem is a finished job's peak memory, if cothon measured it.
func peakMem(j engine.JobView) string {
	if j.Usage == nil {
		return ""
	}
	return fmtBytes(j.Usage.PeakRSS)
}

func fmtBytes(b uint64) string {
	switch {
	case b == 0:
		return ""
	case b < 1<<20:
		return fmt.Sprintf("%dK", b>>10)
	case b < 1<<30:
		return fmt.Sprintf("%dM", b>>20)
	default:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	}
}
