package server

import (
	"fmt"
	"html/template"
	"slices"
	"strings"
	"time"

	"github.com/aymenkrifa/cothon/internal/engine"
	"github.com/aymenkrifa/cothon/internal/fleet"
)

// viewModel is what the templates render.
type viewModel struct {
	S       *engine.State
	Status  string
	Running []engine.JobView // zero or one
	Recent  []engine.JobView
	Groups  []engine.RepoGroup
	Repos   []string
	OrgURL  string
	// View is "grouped" (repo → run → job) or "list" (flat, newest first).
	View string
	// F is the filter from the page URL (pages only; fragments filter client-side).
	F urlFilter
	// U is the live CPU/memory sample (the page then polls /api/usage).
	U *engine.Usage
	// LastJob is the most recent finished job, shown while nothing runs.
	LastJob []engine.JobView // zero or one
	Today   today
}

// today summarizes the jobs started since local midnight.
type today struct {
	Jobs, OK, Failed int
	Busy             time.Duration
}

func view(s *engine.State, mode string) viewModel {
	if mode != "list" {
		mode = "grouped"
	}
	v := viewModel{S: s, Status: s.RunnerStatus(), View: mode}
	if r := s.RunningJob(); r != nil {
		v.Running = []engine.JobView{*r}
	}
	seen := map[string]bool{}
	for _, j := range s.Jobs {
		if j.Repository != "" && !seen[j.Repository] {
			seen[j.Repository] = true
			v.Repos = append(v.Repos, j.Repository)
		}
	}
	v.Recent = s.Jobs
	if len(v.Running) == 0 {
		for _, j := range s.Jobs {
			if j.Status != fleet.StatusRunning {
				v.LastJob = []engine.JobView{j}
				break
			}
		}
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	for _, j := range s.Jobs {
		if j.Started.Before(midnight) {
			continue
		}
		v.Today.Jobs++
		v.Today.Busy += j.Duration(now)
		switch engine.Outcome(j) {
		case "succeeded":
			v.Today.OK++
		case "failed", "abandoned":
			v.Today.Failed++
		}
	}
	v.Groups = engine.GroupJobs(s.Jobs)
	if s.Runner != nil {
		v.OrgURL = s.Runner.GitHubURL
	}
	return v
}

// icon renders a symbol from icons.html. Names are fixed strings from the
// templates, never user data.
func icon(name string) template.HTML {
	return template.HTML(`<svg class="i" aria-hidden="true"><use href="#i-` + template.HTMLEscapeString(name) + `"/></svg>`)
}

// statusIcon maps an outcome to its icon name: shape and color both carry it.
func statusIcon(outcome string) string {
	switch outcome {
	case "succeeded", "success":
		return "pass"
	case "failed", "failure":
		return "fail"
	case "canceled", "cancelled", "skipped":
		return "cancel"
	case "abandoned":
		return "abandoned"
	case "running", "in_progress":
		return "running"
	case "queued", "waiting", "pending":
		return "queued"
	}
	return "unknown"
}

var funcs = template.FuncMap{
	"icon": icon,
	"status": func(outcome string) template.HTML {
		// Fixed class/label from the icon name: log text never reaches the class.
		name := statusIcon(outcome)
		label := map[string]string{"pass": "passed", "fail": "failed", "cancel": "canceled", "abandoned": "abandoned", "running": "running", "queued": "queued"}[name]
		if label == "" {
			label = "unknown"
		}
		return template.HTML(`<span class="st s-`+name+`" role="img" aria-label="`+label+`" title="`+label+`">`) +
			icon(name) + template.HTML(`</span>`)
	},
	"runKey": engine.RunKey,
	"human":  func(d time.Duration) string { return fmtDur(d) },
	"stateLabel": func(status string) string {
		return map[string]string{"idle": "Listening", "busy": "Busy", "stopped": "Stopped", "stopping": "Stopping", "starting": "Starting"}[status]
	},
	"trimScheme": func(u string) string { return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://") },
	"cpu":        func(p float64) string { return fmt.Sprintf("%.0f%%", p) },
	"bytes":      fmtBytes,
	"runPeak": func(jobs []engine.JobView) uint64 {
		var peak uint64
		for _, j := range jobs {
			if j.Usage != nil {
				peak = max(peak, j.Usage.PeakRSS)
			}
		}
		return peak
	},
	"since": func(t time.Time) string { return fmtDur(time.Since(t)) },
	"sub":   func(a, b int) int { return a - b },
	"iso": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	},
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("Jan 02 15:04")
	},
	"dur":     func(j engine.JobView) string { return fmtDur(j.Duration(time.Now())) },
	"outcome": engine.Outcome,
	"span":    func(g engine.RunGroup) string { return fmtDur(g.Span()) },
	"lower":   strings.ToLower,
	"has":     slices.Contains[[]string],
	"outcomeLabel": func(j engine.JobView) string {
		switch {
		case j.Status == fleet.StatusRunning:
			return "Running"
		case j.Status == fleet.StatusAbandoned:
			return "Abandoned"
		case j.Result == "":
			return "Unknown"
		default:
			return j.Result
		}
	},
	"branch": func(ref string) string {
		for _, p := range []string{"refs/heads/", "refs/tags/"} {
			if strings.HasPrefix(ref, p) {
				return strings.TrimPrefix(ref, p)
			}
		}
		return ref
	},
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
	"repoName": func(r string) string {
		if _, n, ok := strings.Cut(r, "/"); ok {
			return n
		}
		return r
	},
	"attempt": func(a string) bool { return a != "" && a != "1" },
	"failed":  func(j engine.JobView) bool { return j.Result == "Failed" || j.Result == "Canceled" },
	"search": func(j engine.JobView) string {
		return strings.ToLower(strings.Join([]string{j.Repository, j.Workflow, j.Name, j.Ref, j.Actor, j.Result, string(j.Status)}, " "))
	},
	"jobStatus": func(j engine.OrgJob) string {
		if j.Status == "completed" {
			return j.Conclusion
		}
		return j.Status
	},
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// fmtBytes renders memory like "850 MB" / "1.2 GB".
func fmtBytes(b uint64) string {
	switch {
	case b == 0:
		return ""
	case b < 1<<20:
		return fmt.Sprintf("%d KB", b>>10)
	case b < 1<<30:
		return fmt.Sprintf("%d MB", b>>20)
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
}
