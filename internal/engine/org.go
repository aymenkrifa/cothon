package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aymenkrifa/cothon/internal/gh"
)

func (e *Engine) pokeOrg() {
	select {
	case e.orgPoke <- struct{}{}:
	default:
	}
}

// orgName resolves the configured org, defaulting to the runner's owner.
func (e *Engine) orgName() string {
	if e.cfg.Org.Name != "" {
		return e.cfg.Org.Name
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Runner != nil {
		u, err := url.Parse(e.state.Runner.GitHubURL)
		if err == nil {
			if owner, _, _ := strings.Cut(strings.Trim(u.Path, "/"), "/"); owner != "" {
				return owner
			}
		}
	}
	return ""
}

// orgLoop polls queued / in-progress runs across recently pushed org repos.
// On permission errors it keeps the local view, shows a warning and backs off.
func (e *Engine) orgLoop(ctx context.Context) {
	interval := e.cfg.Org.PollInterval.Duration
	for {
		wait := interval
		var pausedUntil time.Time
		if err := e.pollOrg(ctx); err != nil {
			e.log.Warn("org poll failed", "err", err)
			var he *gh.HTTPError
			if limited, until := e.api.RateLimited(err); limited {
				pausedUntil = until
				wait = min(max(time.Until(until), interval), time.Hour)
				e.setOrgWarning(fmt.Sprintf("org mode: GitHub rate limit reached; paused until %s", until.Local().Format("15:04")))
			} else if errors.As(err, &he) && (he.Status == 401 || he.Status == 403 || he.Status == 404) {
				wait = 5 * interval
			}
		}
		if !waitOrPoke(ctx, wait, e.orgPoke, pausedUntil) {
			return
		}
	}
}

type apiRepo struct {
	FullName string    `json:"full_name"`
	PushedAt time.Time `json:"pushed_at"`
	Archived bool      `json:"archived"`
}

type apiRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	RunNumber  int       `json:"run_number"`
	RunAttempt int       `json:"run_attempt"`
	Status     string    `json:"status"`
	HeadBranch string    `json:"head_branch"`
	Event      string    `json:"event"`
	HTMLURL    string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
	Actor      struct {
		Login string `json:"login"`
	} `json:"actor"`
}

type apiJob struct {
	CreatedAt  time.Time `json:"created_at"`
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	RunnerName string    `json:"runner_name"`
	Labels     []string  `json:"labels"`
	HTMLURL    string    `json:"html_url"`
	StartedAt  time.Time `json:"started_at"`
}

func (e *Engine) pollOrg(ctx context.Context) error {
	org := e.orgName()
	if org == "" {
		e.setOrg(&OrgState{Warning: "org mode: no org name (set [org] name in the config) — showing local runner only"})
		return errors.New("no org name")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var repos []apiRepo
	reposPath := "/orgs/" + url.PathEscape(org) + "/repos?sort=pushed&direction=desc&per_page=50&type=all"
	err := e.api.Get(ctx, reposPath, &repos)
	var he *gh.HTTPError
	if errors.As(err, &he) && he.Status == 404 {
		// The runner may belong to a user account rather than an org.
		reposPath = "/users/" + url.PathEscape(org) + "/repos?sort=pushed&direction=desc&per_page=50&type=owner"
		err = e.api.Get(ctx, reposPath, &repos)
	}
	if err != nil {
		if limited, _ := e.api.RateLimited(err); !limited {
			e.setOrg(&OrgState{Name: org, Warning: orgWarning(org, err)})
		}
		return err
	}

	cutoff := time.Now().Add(-e.cfg.Org.RecentWindow.Duration)
	var active []string
	for _, r := range repos {
		if r.Archived || r.PushedAt.Before(cutoff) || !gh.ValidRepo(r.FullName) {
			continue
		}
		active = append(active, r.FullName)
		if len(active) >= e.cfg.Org.MaxRepos {
			break
		}
	}

	runnerName := ""
	e.mu.Lock()
	if e.state.Runner != nil {
		runnerName = e.state.Runner.Name
	}
	e.mu.Unlock()

	used := map[string]bool{}
	get := func(path string, v any) error {
		used[path] = true
		return e.api.Get(ctx, path, v)
	}
	used[reposPath] = true

	var runs []OrgRun
	var firstErr error
	seenRun := map[string]bool{} // a run can move from queued to in_progress between the two queries
	for _, repo := range active {
		for _, status := range []string{"queued", "in_progress"} {
			var page struct {
				WorkflowRuns []apiRun `json:"workflow_runs"`
			}
			if err := get("/repos/"+repo+"/actions/runs?status="+status+"&per_page=20", &page); err != nil {
				if limited, _ := e.api.RateLimited(err); limited {
					return err // stop the whole cycle; the loop backs off until the reset
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", repo, err)
				}
				continue
			}
			for _, r := range page.WorkflowRuns {
				if id := strconv.FormatInt(r.ID, 10); seenRun[repo+"|"+id] {
					continue
				} else {
					seenRun[repo+"|"+id] = true
				}
				run := OrgRun{
					Repo: repo, Workflow: r.Name, RunID: strconv.FormatInt(r.ID, 10),
					RunNumber: r.RunNumber, Attempt: r.RunAttempt, Status: r.Status,
					Branch: r.HeadBranch, Event: r.Event, Actor: r.Actor.Login,
					HTMLURL: r.HTMLURL, Created: r.CreatedAt,
				}
				var jobs struct {
					Jobs []apiJob `json:"jobs"`
				}
				if err := get("/repos/"+repo+"/actions/runs/"+run.RunID+"/jobs?per_page=100", &jobs); err == nil {
					for _, j := range jobs.Jobs {
						run.Jobs = append(run.Jobs, OrgJob{
							ID: strconv.FormatInt(j.ID, 10), Name: j.Name, Status: j.Status,
							Conclusion: j.Conclusion, RunnerName: j.RunnerName, Labels: j.Labels,
							HTMLURL: j.HTMLURL, Started: j.StartedAt,
							OnThisRunner: (runnerName != "" && j.RunnerName == runnerName) ||
								(j.Status == "queued" && labelsMatch(j.Labels, e.runnerLabels(runnerName))),
						})
					}
				}
				runs = append(runs, run)
			}
		}
	}
	e.api.Forget(func(p string) bool { return used[p] })
	sort.Slice(runs, func(a, b int) bool { return runs[a].Created.After(runs[b].Created) })

	st := &OrgState{Name: org, Updated: time.Now(), Runs: runs, RateRemaining: e.api.Remaining}
	if runs == nil {
		st.Runs = []OrgRun{}
	}
	if firstErr != nil {
		st.Warning = "org mode: some repos could not be read (" + firstErr.Error() + ")"
	}
	e.setOrg(st)
	return nil
}

// setOrgWarning keeps the last org data but shows a warning.
func (e *Engine) setOrgWarning(w string) {
	e.mu.Lock()
	o := OrgState{Name: e.cfg.Org.Name}
	if e.org != nil {
		o = *e.org
	}
	o.Warning = w
	e.org = &o
	e.mu.Unlock()
	e.kick()
}

func (e *Engine) setOrg(o *OrgState) {
	e.mu.Lock()
	e.org = o
	e.mu.Unlock()
	e.kick()
}

func orgWarning(org string, err error) string {
	var he *gh.HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 401:
			return "org mode: GitHub token rejected (run `gh auth login`) — showing local runner only"
		case 403, 404:
			return fmt.Sprintf("org mode: no access to %s (%s) — showing local runner only", org, he.Error())
		}
	}
	return fmt.Sprintf("org mode: %s unavailable (%v) — showing local runner only", org, err)
}

// runnerLabels are the labels a queued job must fit to be picked up by this
// runner. Reading the real labels needs org-admin, so use the configured
// list or the defaults every self-hosted Linux x64 runner has.
func (e *Engine) runnerLabels(name string) []string {
	labels := e.cfg.Org.RunnerLabels
	if len(labels) == 0 {
		labels = []string{"self-hosted", "linux", "x64"}
	}
	if name != "" {
		labels = append(append([]string{}, labels...), name)
	}
	return labels
}

func labelsMatch(jobLabels, runner []string) bool {
	if len(jobLabels) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, l := range runner {
		have[strings.ToLower(l)] = true
	}
	for _, l := range jobLabels {
		if !have[strings.ToLower(l)] {
			return false
		}
	}
	return true
}
