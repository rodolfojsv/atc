package schedrun

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndQuery(t *testing.T) {
	log := New(filepath.Join(t.TempDir(), "runs.jsonl"))

	base := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	runs := []Run{
		{Schedule: "prs", Time: base, Result: Updated, Session: "prs"},
		{Schedule: "prs", Time: base.Add(15 * time.Minute), Result: NoUpdate},
		{Schedule: "jira", Time: base.Add(20 * time.Minute), Result: Errored, Detail: "precheck: boom"},
		{Schedule: "prs", Time: base.Add(30 * time.Minute), Result: NoUpdate},
	}
	for _, r := range runs {
		if err := log.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	all, err := log.All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != len(runs) {
		t.Fatalf("got %d runs, want %d", len(all), len(runs))
	}
	if all[2].Detail != "precheck: boom" {
		t.Errorf("detail roundtrip: got %q", all[2].Detail)
	}

	// Latest for "prs" is the final no-update, not the earlier update.
	latest, ok := log.Latest("prs")
	if !ok || latest.Result != NoUpdate || !latest.Time.Equal(base.Add(30*time.Minute)) {
		t.Errorf("latest prs: got %+v ok=%v", latest, ok)
	}

	// LastUpdate skips the no-updates and returns the update time — the
	// "no updates since X" anchor.
	since, ok := log.LastUpdate("prs")
	if !ok || !since.Equal(base) {
		t.Errorf("last update prs: got %v ok=%v, want %v", since, ok, base)
	}

	// A schedule that never updated has no anchor.
	if _, ok := log.LastUpdate("jira"); ok {
		t.Errorf("jira never updated, want no anchor")
	}
}

func TestEmptyAndMissing(t *testing.T) {
	// Zero-value log is a no-op sink, not a crash.
	var zero Log
	if err := zero.Append(Run{Schedule: "x", Result: Updated}); err != nil {
		t.Errorf("zero append: %v", err)
	}
	if runs, err := zero.All(); err != nil || runs != nil {
		t.Errorf("zero all: %v %v", runs, err)
	}

	// Missing file reads as empty, not an error.
	log := New(filepath.Join(t.TempDir(), "absent.jsonl"))
	if runs, err := log.All(); err != nil || runs != nil {
		t.Errorf("missing all: %v %v", runs, err)
	}
	if _, ok := log.Latest("any"); ok {
		t.Errorf("missing latest: want none")
	}
}
