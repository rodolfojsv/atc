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
}
