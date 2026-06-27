package supervisor

import (
	"time"

	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/sched"
	"github.com/rodolfojsv/atc/internal/schedrun"
)

// maxScheduleRuns caps how many recent fires a ScheduleView carries, so a
// long-lived schedule's timeline stays bounded in the UI: only the latest
// few fires per task are shown.
const maxScheduleRuns = 5

// ScheduleRun is one entry in a scheduled task's timeline: the outcome of
// a single fire. Result is "updated", "no-update", or "error".
type ScheduleRun struct {
	Time    time.Time
	Result  string
	Session string // the session launched on an "updated" fire, for deep-linking
	Detail  string // error message on an "error" fire
}

// ScheduleView is a scheduled task as the UIs render it: its definition,
// when it next fires, and its recent run history (most recent first). It
// is assembled from config plus the schedule run log — the task itself is
// never a live session.
type ScheduleView struct {
	Name        string
	Cron        string
	Repo        string
	Preset      string
	Model       string // model override; empty falls back to preset/config default
	Agent       string
	Prompt      string
	Precheck    string // raw precheck command, for the editor (HasPrecheck is the display flag)
	Worktree    bool
	Write       bool // false = read-only (plan mode), the default for schedules
	HasPrecheck bool
	Disabled    bool      // turned off in config; kept on the board but never fires
	NextFire    time.Time // zero when the cron is unparseable, disabled, or can never fire
	LastUpdate  time.Time // time of the most recent "updated" fire; zero if never
	Runs        []ScheduleRun
}

// ScheduleConfigs returns a copy of the current schedule definitions, safe to
// mutate by the caller (the web UI's schedule editor). Reads are guarded so a
// concurrent SetSchedules can't tear the slice header.
func (s *Supervisor) ScheduleConfigs() []config.Schedule {
	s.schedMu.RLock()
	defer s.schedMu.RUnlock()
	out := make([]config.Schedule, len(s.cfg.Schedules))
	copy(out, s.cfg.Schedules)
	return out
}

// SetSchedules replaces the in-memory schedule set under lock, so concurrent
// Schedules()/ScheduleConfigs() reads stay consistent. Persisting the change to
// config.json is the caller's job (config.Save); the in-process scheduler
// re-reads config.json from disk every minute, so a saved change fires without
// a restart.
func (s *Supervisor) SetSchedules(list []config.Schedule) {
	s.schedMu.Lock()
	s.cfg.Schedules = list
	s.schedMu.Unlock()
}

// Schedules returns a view of every configured schedule joined with its
// run-log history, for the "Scheduled" section of the board. Quiet fires
// (precheck reported no change) show as "no-update" runs without ever
// having spent a token.
func (s *Supervisor) Schedules() []ScheduleView {
	all, _ := schedrun.Default().All()
	byName := map[string][]schedrun.Run{}
	for _, r := range all {
		byName[r.Schedule] = append(byName[r.Schedule], r)
	}

	now := time.Now()
	scheds := s.ScheduleConfigs()
	out := make([]ScheduleView, 0, len(scheds))
	for _, sc := range scheds {
		name := sc.Name
		if name == "" {
			name = "sched"
		}
		v := ScheduleView{
			Name:        name,
			Cron:        sc.Cron,
			Repo:        sc.Repo,
			Preset:      sc.Preset,
			Model:       sc.Model,
			Agent:       sc.Agent,
			Prompt:      sc.Prompt,
			Precheck:    sc.Precheck,
			Worktree:    sc.Worktree,
			Write:       sc.Write,
			HasPrecheck: sc.Precheck != "",
			Disabled:    sc.Disabled,
		}
		// A disabled schedule never fires, so it has no next run to show.
		if !sc.Disabled {
			if entry, err := sched.Parse(sc.Cron); err == nil {
				v.NextFire = entry.Next(now)
			}
		}

		runs := byName[name]
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].Result == schedrun.Updated {
				v.LastUpdate = runs[i].Time
				break
			}
		}
		for i := len(runs) - 1; i >= 0 && len(v.Runs) < maxScheduleRuns; i-- {
			r := runs[i]
			v.Runs = append(v.Runs, ScheduleRun{
				Time:    r.Time,
				Result:  string(r.Result),
				Session: r.Session,
				Detail:  r.Detail,
			})
		}
		out = append(out, v)
	}
	return out
}
