package spend

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerAccumulatesAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.jsonl")
	l := Open(path)
	l.Add(Record{Session: "a", Backend: "copilot", In: 100, Out: 50, NanoAiu: 2e8})
	l.Add(Record{Session: "b", Backend: "claude", In: 10, Out: 5, CostUSD: 0.03})

	today := l.Today()
	if today.In != 110 || today.Out != 55 || today.NanoAiu != 2e8 || today.CostUSD != 0.03 {
		t.Fatalf("today totals wrong: %+v", today)
	}

	// A fresh ledger must rebuild totals from disk.
	l2 := Open(path)
	if got := l2.Month(); got.In != 110 || got.NanoAiu != 2e8 {
		t.Fatalf("reloaded month totals wrong: %+v", got)
	}

	// Old records outside the current windows are excluded.
	l2.Add(Record{Time: time.Now().AddDate(0, -2, 0), Session: "old", In: 9999})
	if got := l2.Month(); got.In != 110 {
		t.Fatalf("old record leaked into month: %+v", got)
	}
}

func TestDisabledLedger(t *testing.T) {
	l := Open("")
	l.Add(Record{In: 5})
	if l.Today().In != 5 {
		t.Error("in-memory totals should still work without a path")
	}
}
