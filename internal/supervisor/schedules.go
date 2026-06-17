package supervisor

import (
	"time"

	"github.com/rodolfojsv/atc/internal/sched"
	"github.com/rodolfojsv/atc/internal/schedrun"
)

// maxScheduleRuns caps how many recent fires a ScheduleView carries, so a
// long-lived schedule's timeline stays bounded in the UI.
const maxScheduleRuns = 60

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
	Worktree    bool
	HasPrecheck bool
	NextFire    time.Time // zero when the cron is unparseable or can never fire
	LastUpdate  time.Time // time of the most recent "updated" fire; zero if never
	Runs        []ScheduleRun
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
	out := make([]ScheduleView, 0, len(s.cfg.Schedules))
	for _, sc := range s.cfg.Schedules {
		name := sc.Name
		if name == "" {
			name = "sched"
		}
		v := ScheduleView{
			Name:        name,
			Cron:        sc.Cron,
			Repo:        sc.Repo,
			Preset:      sc.Preset,
			Worktree:    sc.Worktree,
			HasPrecheck: sc.Precheck != "",
		}
		if entry, err := sched.Parse(sc.Cron); err == nil {
			v.NextFire = entry.Next(now)
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
