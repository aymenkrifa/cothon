package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aymenkrifa/cothon/internal/fleet"
)

// Resource use of the runner and its jobs, sampled from /proc: every 2s while
// a job runs, every 10s otherwise (runner only). Live numbers are served
// separately from State (Backend.Usage) so they don't re-render the job list;
// a finished job's summary is attached to its JobView and kept on disk.

const (
	usageBusyEvery = 2 * time.Second
	usageIdleEvery = 10 * time.Second
	usageHistory   = 120 // points kept for the running job's graph (4 min at 2s)
)

// Usage is the live view.
type Usage struct {
	Runner ProcUsage `json:"runner"`        // listener + run.sh themselves, without jobs
	Job    *JobUsage `json:"job,omitempty"` // the running job's process tree
	Cores  int       `json:"cores"`
	At     time.Time `json:"at"`
}

type ProcUsage struct {
	CPU   float64 `json:"cpu"` // percent of one core, like top
	RSS   uint64  `json:"rss"` // bytes
	Procs int     `json:"procs"`
}

type JobUsage struct {
	JobID   string       `json:"job_id"`
	Current ProcUsage    `json:"current"`
	Summary UsageSummary `json:"summary"`
	History []UsagePoint `json:"history"`
}

type UsagePoint struct {
	T   time.Time `json:"t"`
	CPU float64   `json:"cpu"`
	RSS uint64    `json:"rss"`
}

// UsageSummary is what a job used overall.
type UsageSummary struct {
	PeakRSS    uint64  `json:"peak_rss"`
	PeakCPU    float64 `json:"peak_cpu"`
	AvgCPU     float64 `json:"avg_cpu"`     // percent of one core over the sampled time
	CPUSeconds float64 `json:"cpu_seconds"` // total CPU time
	Samples    int     `json:"samples"`
}

// sampler turns successive tick counts into CPU percentages.
type sampler struct {
	root  int
	ticks uint64
	at    time.Time
}

func (s *sampler) cpu(root int, sample fleet.Sample, now time.Time) (float64, uint64) {
	defer func() { s.root, s.ticks, s.at = root, sample.Ticks, now }()
	if s.root != root || s.at.IsZero() || sample.Ticks < s.ticks {
		return 0, 0 // first sample of this process (or it restarted)
	}
	dt := now.Sub(s.at).Seconds()
	if dt <= 0 {
		return 0, 0
	}
	delta := sample.Ticks - s.ticks
	return float64(delta) / fleet.ClockTicks / dt * 100, delta
}

func (e *Engine) usageLoop(ctx context.Context) {
	var runnerS, jobS sampler
	var cur *JobUsage
	var sampledFor time.Duration // wall time covered by the running job's samples
	for {
		e.mu.Lock()
		procs := e.state.Processes
		var running *JobView
		if j := e.state.RunningJob(); j != nil {
			cp := *j
			running = &cp
		}
		e.mu.Unlock()

		now := time.Now()
		u := &Usage{Cores: runtime.NumCPU(), At: now}
		rs := fleet.SampleProcs(procs.SupervisorPID, procs.ListenerPID)
		cpu, _ := runnerS.cpu(procs.ListenerPID, rs, now)
		u.Runner = ProcUsage{CPU: cpu, RSS: rs.RSS, Procs: rs.Procs}

		// A job is measured while its worker runs.
		if running != nil && procs.WorkerPID != 0 {
			if cur == nil || cur.JobID != running.ID {
				e.finishJobUsage(cur)
				cur, sampledFor = &JobUsage{JobID: running.ID}, 0
			}
			js := fleet.SampleTree(procs.WorkerPID)
			prevAt := jobS.at
			cpu, delta := jobS.cpu(procs.WorkerPID, js, now)
			cur.Current = ProcUsage{CPU: cpu, RSS: js.RSS, Procs: js.Procs}
			sm := &cur.Summary
			sm.Samples++
			sm.PeakRSS = max(sm.PeakRSS, js.RSS)
			sm.PeakCPU = max(sm.PeakCPU, cpu)
			if delta > 0 || !prevAt.IsZero() {
				sm.CPUSeconds += float64(delta) / fleet.ClockTicks
				if !prevAt.IsZero() {
					sampledFor += now.Sub(prevAt)
				}
			}
			if sampledFor > 0 {
				sm.AvgCPU = sm.CPUSeconds / sampledFor.Seconds() * 100
			}
			cur.History = append(cur.History, UsagePoint{T: now, CPU: cpu, RSS: js.RSS})
			if n := len(cur.History); n > usageHistory {
				cur.History = cur.History[n-usageHistory:]
			}
			cp := *cur
			cp.History = append([]UsagePoint(nil), cur.History...)
			u.Job = &cp
		} else if cur != nil {
			e.finishJobUsage(cur)
			cur, jobS = nil, sampler{}
		}

		e.mu.Lock()
		e.usage = u
		e.mu.Unlock()

		wait := usageIdleEvery
		if u.Job != nil {
			wait = usageBusyEvery
		}
		select {
		case <-ctx.Done():
			e.finishJobUsage(cur)
			return
		case <-time.After(wait):
		case <-e.usagePoke:
		}
	}
}

// finishJobUsage stores a finished job's summary and saves the history file.
func (e *Engine) finishJobUsage(j *JobUsage) {
	if j == nil || j.Summary.Samples == 0 {
		return
	}
	e.mu.Lock()
	e.usageHist[j.JobID] = j.Summary
	// Keep summaries only for jobs still listed.
	keep := make(map[string]bool, len(e.state.Jobs))
	for _, jv := range e.state.Jobs {
		keep[jv.ID] = true
	}
	keep[j.JobID] = true
	for id := range e.usageHist {
		if !keep[id] && len(e.usageHist) > 2*e.cfg.MaxJobs {
			delete(e.usageHist, id)
		}
	}
	snapshot := make(map[string]UsageSummary, len(e.usageHist))
	for k, v := range e.usageHist {
		snapshot[k] = v
	}
	e.mu.Unlock()
	if err := saveUsage(e.usageFile, snapshot); err != nil {
		e.log.Warn("saving job usage", "err", err)
	}
	e.kick() // the job row now shows its summary
}

func (e *Engine) pokeUsage() {
	select {
	case e.usagePoke <- struct{}{}:
	default:
	}
}

// Usage implements Backend: the latest live sample.
func (e *Engine) Usage(context.Context) (*Usage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.usage == nil {
		return &Usage{Cores: runtime.NumCPU()}, nil
	}
	return e.usage, nil
}

func loadUsage(path string) map[string]UsageSummary {
	m := map[string]UsageSummary{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func saveUsage(path string, m map[string]UsageSummary) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
