package engine

import (
	"strings"
	"time"

	"github.com/aymenkrifa/cothon/internal/fleet"
)

// RepoGroup is a repository with its runs, newest first.
type RepoGroup struct {
	Repo                          string
	Runs                          []RunGroup
	Latest                        time.Time
	Outcome                       string
	OK, Failed, Canceled, Running int
}

// RunGroup is one workflow run attempt and the jobs this runner executed for it.
type RunGroup struct {
	Key                                       string
	Repo, Workflow, RunID, RunNumber, Attempt string
	Ref, Event, Actor, RunURL                 string
	Jobs                                      []JobView // execution order
	Started, Finished                         time.Time
	Outcome                                   string // worst outcome: running > failed > abandoned > canceled > succeeded
	OK, Failed, Canceled, Running             int
}

// GroupRuns groups jobs by (repo, run, attempt), newest run first.
func GroupRuns(jobs []JobView) []RunGroup {
	var groups []RunGroup
	index := map[string]int{}
	for _, j := range jobs {
		key := RunKey(j)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, RunGroup{
				Key: key, Repo: j.Repository, Workflow: j.Workflow, RunID: j.RunID, RunNumber: j.RunNumber,
				Attempt: j.RunAttempt, Ref: j.Ref, Event: j.Event, Actor: j.Actor, RunURL: j.RunURL,
			})
		}
		g := &groups[i]
		g.Jobs = append(g.Jobs, j)
		if g.Started.IsZero() || j.Started.Before(g.Started) {
			g.Started = j.Started
		}
		if j.Finished.After(g.Finished) {
			g.Finished = j.Finished
		}
		switch Outcome(j) {
		case "running":
			g.Running++
		case "succeeded":
			g.OK++
		case "canceled":
			g.Canceled++
		default:
			g.Failed++
		}
	}
	rank := outcomeRank
	for i := range groups {
		g := &groups[i]
		// Jobs arrive newest first; show them in execution order.
		for a, b := 0, len(g.Jobs)-1; a < b; a, b = a+1, b-1 {
			g.Jobs[a], g.Jobs[b] = g.Jobs[b], g.Jobs[a]
		}
		for _, j := range g.Jobs {
			if o := Outcome(j); rank[o] > rank[g.Outcome] {
				g.Outcome = o
			}
		}
	}
	return groups
}

var outcomeRank = map[string]int{"running": 5, "failed": 4, "abandoned": 3, "unknown": 2, "canceled": 2, "succeeded": 1}

// GroupRepos nests runs under their repo; repos ordered by latest activity.
func GroupRepos(runs []RunGroup) []RepoGroup {
	var repos []RepoGroup
	index := map[string]int{}
	for i, r := range runs {
		k, ok := index[r.Repo]
		if !ok {
			k = len(repos)
			index[r.Repo] = k
			repos = append(repos, RepoGroup{Repo: r.Repo})
		}
		g := &repos[k]
		g.Runs = append(g.Runs, runs[i])
		if r.Started.After(g.Latest) {
			g.Latest = r.Started
		}
		g.OK += r.OK
		g.Failed += r.Failed
		g.Canceled += r.Canceled
		g.Running += r.Running
		// A repo's badge reflects where it stands now: running if anything
		// runs, otherwise the result of its newest run (runs arrive newest first).
		if len(g.Runs) == 1 || r.Running > 0 {
			g.Outcome = r.Outcome
		}
	}
	return repos
}

// Outcome is a job's display outcome: running, abandoned, unknown or the
// lower-cased runner result (succeeded, failed, canceled).
func Outcome(j JobView) string {
	switch {
	case j.Status == fleet.StatusRunning:
		return "running"
	case j.Status == fleet.StatusAbandoned:
		return "abandoned"
	case j.Result == "":
		return "unknown"
	default:
		return strings.ToLower(j.Result)
	}
}

// Span is the wall time from the first job start to the last job end (or now).
func (g RunGroup) Span() time.Duration {
	end := g.Finished
	if g.Running > 0 || end.IsZero() {
		end = time.Now()
	}
	return end.Sub(g.Started)
}

// GroupJobs groups jobs (newest first) into repos → runs → jobs.
func GroupJobs(jobs []JobView) []RepoGroup { return GroupRepos(GroupRuns(jobs)) }

// RunKey identifies a job's run attempt. Jobs whose run is unknown (damaged
// logs) each get their own group instead of being lumped together.
func RunKey(j JobView) string {
	if j.RunID == "" {
		return "job:" + j.ID + "|" + j.LogFile
	}
	return j.Repository + "|" + j.RunID + "|" + j.RunAttempt
}
