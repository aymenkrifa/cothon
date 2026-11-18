package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aymenkrifa/cothon/internal/gh"
)

// The queue: jobs waiting for this runner. The runner's logs only show a job
// once it starts, so this asks GitHub — but only about repos this runner has
// worked on recently, with conditional requests (unchanged answers come back
// as 304 and don't count against the rate limit).

const (
	queueRepoWindow = 7 * 24 * time.Hour
	queueMaxRepos   = 10
)

// QueueState lists jobs waiting for a runner with this runner's labels.
type QueueState struct {
	Jobs    []QueuedJob `json:"jobs"` // oldest first: the next one to start is on top
	Updated time.Time   `json:"updated,omitzero"`
	Warning string      `json:"warning,omitempty"`
}

type QueuedJob struct {
	Repo      string    `json:"repo"`
	Workflow  string    `json:"workflow"`
	RunID     string    `json:"run_id"`
	RunNumber int       `json:"run_number"`
	JobID     string    `json:"job_id"`
	Name      string    `json:"name"`
	Branch    string    `json:"branch"`
	Event     string    `json:"event"`
	Actor     string    `json:"actor"`
	HTMLURL   string    `json:"html_url"`
	Queued    time.Time `json:"queued"` // when the job was created
	Labels    []string  `json:"labels"`
}

func (e *Engine) pokeQueue() {
	select {
	case e.queuePoke <- struct{}{}:
	default:
	}
}

// queueRepos are the repos this runner ran jobs for recently.
func (e *Engine) queueRepos() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-queueRepoWindow)
	seen := map[string]bool{}
	var repos []string
	for _, j := range e.state.Jobs { // newest first
		if j.Started.Before(cutoff) || !gh.ValidRepo(j.Repository) || seen[j.Repository] {
			continue
		}
		seen[j.Repository] = true
		repos = append(repos, j.Repository)
		if len(repos) == queueMaxRepos {
			break
		}
	}
	return repos
}

func (e *Engine) queueLoop(ctx context.Context) {
	interval := e.cfg.Queue.PollInterval.Duration
	var pausedUntil time.Time
	for {
		wait := interval
		if err := e.pollQueue(ctx); err != nil {
			e.log.Warn("queue poll failed", "err", err)
			var he *gh.HTTPError
			switch limited, until := e.queueAPI.RateLimited(err); {
			case limited:
				pausedUntil = until
				wait = min(max(time.Until(until), interval), time.Hour)
				e.setQueueWarning(fmt.Sprintf("queue: GitHub rate limit reached; paused until %s", until.Local().Format("15:04")))
			case errors.As(err, &he) && he.Status == 401:
				wait = 10 * interval
			}
		}
		if !waitOrPoke(ctx, wait, e.queuePoke, pausedUntil) {
			return
		}
	}
}

// waitOrPoke sleeps for wait, waking early on a poke unless a rate-limit
// pause is in effect. It reports false when ctx ends.
func waitOrPoke(ctx context.Context, wait time.Duration, poke <-chan struct{}, pausedUntil time.Time) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-poke:
			if time.Now().Before(pausedUntil) {
				continue // actions don't cut a rate-limit pause short
			}
			// Let GitHub register the change before re-reading.
			select {
			case <-ctx.Done():
				return false
			case <-time.After(3 * time.Second):
				return true
			}
		}
	}
}

// queueWarnAfter is how many polls in a row a repo must fail before the queue
// shows a warning; single timeouts are common and harmless.
const queueWarnAfter = 3

func (e *Engine) pollQueue(ctx context.Context) error {
	repos := e.queueRepos()
	runnerName := ""
	e.mu.Lock()
	if e.state.Runner != nil {
		runnerName = e.state.Runner.Name
	}
	e.mu.Unlock()
	labels := e.runnerLabels(runnerName)

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	used := map[string]bool{}
	get := func(path string, v any) error {
		used["q:"+path] = true
		return e.queueAPI.Get(ctx, path, v)
	}

	var prev []QueuedJob
	e.mu.Lock()
	if e.queue != nil {
		prev = e.queue.Jobs
	}
	e.mu.Unlock()
	fails := map[string]int{}

	var jobs []QueuedJob
	var firstErr error
	seenRun := map[string]bool{}
	for _, repo := range repos {
		var repoJobs []QueuedJob
		var repoErr error
		// A matrix whose first leg runs here is "in_progress" while the other
		// legs still queue, so both kinds of runs are inspected.
		for _, status := range []string{"queued", "in_progress"} {
			var page struct {
				WorkflowRuns []apiRun `json:"workflow_runs"`
			}
			if err := get("/repos/"+repo+"/actions/runs?status="+status+"&per_page=20", &page); err != nil {
				if limited, _ := e.queueAPI.RateLimited(err); limited {
					return err
				}
				if repoErr == nil {
					repoErr = err
				}
				continue
			}
			for _, r := range page.WorkflowRuns {
				runID := strconv.FormatInt(r.ID, 10)
				if seenRun[repo+"|"+runID] {
					continue
				}
				seenRun[repo+"|"+runID] = true
				var js struct {
					Jobs []apiJob `json:"jobs"`
				}
				if err := get("/repos/"+repo+"/actions/runs/"+runID+"/jobs?per_page=100", &js); err != nil {
					if limited, _ := e.queueAPI.RateLimited(err); limited {
						return err
					}
					if repoErr == nil {
						repoErr = err
					}
					continue
				}
				for _, j := range js.Jobs {
					if (j.Status != "queued" && j.Status != "waiting" && j.Status != "pending") || !labelsMatch(j.Labels, labels) {
						continue
					}
					repoJobs = append(repoJobs, QueuedJob{
						Repo: repo, Workflow: r.Name, RunID: runID, RunNumber: r.RunNumber,
						JobID: strconv.FormatInt(j.ID, 10), Name: j.Name, Branch: r.HeadBranch,
						Event: r.Event, Actor: r.Actor.Login, HTMLURL: j.HTMLURL,
						Queued: j.CreatedAt, Labels: j.Labels,
					})
				}
			}
		}
		if repoErr == nil {
			jobs = append(jobs, repoJobs...)
			continue
		}
		// A failed read (often a slow answer) keeps what was last known for
		// the repo; only repeated failures are worth a warning.
		for _, j := range prev {
			if j.Repo == repo {
				jobs = append(jobs, j)
			}
		}
		fails[repo] = e.queueFails[repo] + 1
		if fails[repo] >= queueWarnAfter && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", repo, repoErr)
		}
	}
	e.queueFails = fails
	e.queueAPI.Forget(func(p string) bool { return used["q:"+p] })
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].Queued.Before(jobs[b].Queued) })

	q := &QueueState{Jobs: jobs, Updated: time.Now()}
	if q.Jobs == nil {
		q.Jobs = []QueuedJob{}
	}
	if firstErr != nil {
		q.Warning = "queue: some repos could not be read (" + firstErr.Error() + ")"
	}
	e.mu.Lock()
	e.queue = q
	e.mu.Unlock()
	e.kick()
	return firstErr
}

func (e *Engine) setQueueWarning(w string) {
	e.mu.Lock()
	q := QueueState{Jobs: []QueuedJob{}}
	if e.queue != nil {
		q = *e.queue
	}
	q.Warning = w
	e.queue = &q
	e.mu.Unlock()
	e.kick()
}
