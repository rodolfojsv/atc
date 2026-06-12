// Package spend keeps a cumulative ledger of usage across runs
// (~/.atc/spend.jsonl, one JSON line per usage event) so the board can
// answer "how much have I burned today / this month" — per-session
// numbers evaporate when sessions are forgotten.
package spend

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session"`
	Backend string    `json:"backend"`
	Model   string    `json:"model,omitempty"`
	In      int64     `json:"in"`
	Out     int64     `json:"out"`
	NanoAiu float64   `json:"nanoAiu,omitempty"`
	CostUSD float64   `json:"costUsd,omitempty"`
}

type Totals struct {
	In, Out int64
	NanoAiu float64
	CostUSD float64
}

func (t *Totals) add(r Record) {
	t.In += r.In
	t.Out += r.Out
	t.NanoAiu += r.NanoAiu
	t.CostUSD += r.CostUSD
}

type Ledger struct {
	mu       sync.Mutex
	path     string
	day      Totals
	month    Totals
	dayKey   string
	monthKey string
}

func dayKey(t time.Time) string   { return t.Format("2006-01-02") }
func monthKey(t time.Time) string { return t.Format("2006-01") }

// Open loads the ledger at path ("" disables it) and folds existing
// records into today's and this month's totals.
func Open(path string) *Ledger {
	now := time.Now()
	l := &Ledger{path: path, dayKey: dayKey(now), monthKey: monthKey(now)}
	if path == "" {
		return l
	}
	f, err := os.Open(path)
	if err != nil {
		return l
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if monthKey(r.Time) == l.monthKey {
			l.month.add(r)
		}
		if dayKey(r.Time) == l.dayKey {
			l.day.add(r)
		}
	}
	return l
}

// Add appends the record and updates the running totals. Best-effort:
// ledger failures never affect sessions.
func (l *Ledger) Add(r Record) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Windows always track the wall clock; the record only counts if
	// its own timestamp falls inside them.
	l.rotate(time.Now())
	if monthKey(r.Time) == l.monthKey {
		l.month.add(r)
	}
	if dayKey(r.Time) == l.dayKey {
		l.day.add(r)
	}
	if l.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Today and Month return the running totals, rolling the windows over
// when the date has changed since the last event.
func (l *Ledger) Today() Totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotate(time.Now())
	return l.day
}

func (l *Ledger) Month() Totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotate(time.Now())
	return l.month
}

func (l *Ledger) rotate(now time.Time) {
	if k := dayKey(now); k != l.dayKey {
		l.dayKey, l.day = k, Totals{}
	}
	if k := monthKey(now); k != l.monthKey {
		l.monthKey, l.month = k, Totals{}
	}
}
