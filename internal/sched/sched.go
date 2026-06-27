// Package sched is a minimal 5-field cron scheduler — just enough for
// "9am on weekdays" style schedules without pulling in a cron library
// (the trust surface of this tool is deliberately small).
//
// Supported per field: "*", "*/n", "a", "a-b", "a-b/n", and
// comma-separated lists of those. Fields: minute (0-59), hour (0-23),
// day-of-month (1-31), month (1-12), day-of-week (0-7, 0 and 7 = Sunday).
package sched

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type field struct {
	set        map[int]bool
	restricted bool // false when the field was "*"
}

type Entry struct {
	min, hour, dom, mon, dow field
}

func Parse(expr string) (*Entry, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(parts))
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	fields := make([]field, 5)
	for i, p := range parts {
		f, err := parseField(p, bounds[i][0], bounds[i][1])
		if err != nil {
			return nil, fmt.Errorf("cron %q: %w", expr, err)
		}
		fields[i] = f
	}
	// Normalize day-of-week 7 to 0 (both mean Sunday).
	if fields[4].set[7] {
		fields[4].set[0] = true
	}
	return &Entry{min: fields[0], hour: fields[1], dom: fields[2], mon: fields[3], dow: fields[4]}, nil
}

func parseField(s string, lo, hi int) (field, error) {
	f := field{set: map[int]bool{}}
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return f, fmt.Errorf("bad step in %q", part)
			}
			step, part = n, part[:i]
		}
		start, end := lo, hi
		switch {
		case part == "*":
			if step == 1 && s == "*" {
				// Pure wildcard: leave unrestricted (matters for dom/dow OR rule).
				for v := lo; v <= hi; v++ {
					f.set[v] = true
				}
				return field{set: f.set, restricted: false}, nil
			}
		case strings.Contains(part, "-"):
			a, b, ok := strings.Cut(part, "-")
			va, err1 := strconv.Atoi(a)
			vb, err2 := strconv.Atoi(b)
			if !ok || err1 != nil || err2 != nil {
				return f, fmt.Errorf("bad range %q", part)
			}
			start, end = va, vb
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return f, fmt.Errorf("bad value %q", part)
			}
			start, end = v, v
		}
		if start < lo || end > hi || start > end {
			return f, fmt.Errorf("value out of range in %q", part)
		}
		for v := start; v <= end; v += step {
			f.set[v] = true
		}
	}
	f.restricted = true
	return f, nil
}

// Matches reports whether the entry fires at t (minute resolution).
// Like classic cron, when both day-of-month and day-of-week are
// restricted, matching either one suffices.
func (e *Entry) Matches(t time.Time) bool {
	if !e.min.set[t.Minute()] || !e.hour.set[t.Hour()] || !e.mon.set[int(t.Month())] {
		return false
	}
	domOK := e.dom.set[t.Day()]
	dowOK := e.dow.set[int(t.Weekday())]
	if e.dom.restricted && e.dow.restricted {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Next returns the first firing strictly after t (minute resolution), or
// the zero time when nothing matches within a year — which only happens
// for a schedule that can never fire (e.g. Feb 30). Used to show a
// schedule's upcoming run in the UI.
func (e *Entry) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(1, 0, 0)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if e.Matches(t) {
			return t
		}
	}
	return time.Time{}
}

// Job pairs a parsed schedule with what to run.
type Job struct {
	Entry *Entry
	Fire  func()
}

// RunReloadable checks all jobs once per minute, aligned to the
// wall-clock minute, until ctx is done, re-deriving the job set from
// build() each minute so schedules added, edited, or removed in config
// take effect without restarting the process. Each firing runs on its
// own goroutine. build() is expected to be cheap (e.g. gated on the
// config file's mtime) and to return the desired job set, or a non-nil
// error when it cannot — in which case the previous good set keeps
// firing and the error is reported via onErr (which may be nil). An
// empty set is fine: a config may have zero schedules now and gain one
// later.
func RunReloadable(ctx context.Context, build func() ([]Job, error), onErr func(error)) {
	report := func(err error) {
		if err != nil && onErr != nil {
			onErr(err)
		}
	}
	jobs, err := build()
	report(err)
	for {
		now := time.Now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now)):
		}
		if fresh, err := build(); err != nil {
			report(err) // keep the last good set
		} else {
			jobs = fresh
		}
		tick := time.Now()
		for _, j := range jobs {
			if j.Entry.Matches(tick) {
				go j.Fire()
			}
		}
	}
}
