package supervisor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rodolfojsv/atc/internal/bus"
)

// newTestSup builds a supervisor with a live bus and a store redirected to a
// temp file, so Kill/persist don't touch the real ~/.atc.
func newTestSup(t *testing.T) *Supervisor {
	t.Helper()
	s := New(testConfig(t), bus.New())
	s.store = store{path: filepath.Join(t.TempDir(), "sessions.json")}
	return s
}

func TestPruneScheduledInMemory(t *testing.T) {
	s := newTestSup(t)
	old := time.Now().Add(-72 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	keepRunning := &Session{Name: "sched-running", ScheduleName: "nightly", status: StatusWorking, Created: old}
	keepRecent := &Session{Name: "sched-recent", ScheduleName: "nightly", status: StatusDone, Created: recent}
	keepManual := &Session{Name: "manual-old", status: StatusDone, Created: old}
	dropStale := &Session{Name: "sched-stale", ScheduleName: "nightly", status: StatusDone, Created: old}
	s.sessions = []*Session{keepRunning, keepRecent, keepManual, dropStale}

	if n := s.PruneScheduled(48 * time.Hour); n != 1 {
		t.Fatalf("PruneScheduled removed %d, want 1", n)
	}
	got := map[string]bool{}
	for _, sess := range s.Sessions() {
		got[sess.Name] = true
	}
	if got["sched-stale"] {
		t.Error("stale finished scheduled session should have been pruned")
	}
	for _, name := range []string{"sched-running", "sched-recent", "manual-old"} {
		if !got[name] {
			t.Errorf("%s should have been kept", name)
		}
	}
}

func TestPruneScheduledStoreOnly(t *testing.T) {
	s := newTestSup(t)
	old := time.Now().Add(-72 * time.Hour).UTC()
	recent := time.Now().Add(-1 * time.Hour).UTC()
	if err := s.store.save([]savedSession{
		{ID: "a", Name: "sched-stale", ScheduleName: "nightly", Status: "done", Created: old},
		{ID: "b", Name: "sched-recent", ScheduleName: "nightly", Status: "done", Created: recent},
		{ID: "c", Name: "manual-stale", Status: "done", Created: old},
		{ID: "d", Name: "sched-running", ScheduleName: "nightly", Status: "working", Created: old},
	}); err != nil {
		t.Fatal(err)
	}

	if n := s.PruneScheduled(48 * time.Hour); n != 1 {
		t.Fatalf("PruneScheduled removed %d, want 1", n)
	}
	kept := map[string]bool{}
	for _, sv := range s.store.load() {
		kept[sv.ID] = true
	}
	if kept["a"] {
		t.Error("store entry a (stale scheduled) should have been pruned")
	}
	for _, id := range []string{"b", "c", "d"} {
		if !kept[id] {
			t.Errorf("store entry %s should have been kept", id)
		}
	}
}

func TestPruneScheduledDisabled(t *testing.T) {
	s := newTestSup(t)
	s.sessions = []*Session{{Name: "x", ScheduleName: "n", status: StatusDone, Created: time.Now().Add(-999 * time.Hour)}}
	if n := s.PruneScheduled(0); n != 0 {
		t.Fatalf("retention 0 should prune nothing, removed %d", n)
	}
}

func TestUniqueNameTimestampOnCollision(t *testing.T) {
	s := newTestSup(t)
	s.sessions = []*Session{{Name: "nightly"}}

	if got := s.uniqueName("fresh"); got != "fresh" {
		t.Errorf("no collision should keep the name, got %q", got)
	}
	got := s.uniqueName("nightly")
	if !strings.HasPrefix(got, "nightly-") {
		t.Fatalf("collision should suffix the name, got %q", got)
	}
	// The suffix is a timestamp (digits), not the old -2 counter.
	suffix := strings.TrimPrefix(got, "nightly-")
	if suffix == "2" {
		t.Errorf("collision suffix should be a timestamp, not the old counter: %q", got)
	}
	if _, err := time.Parse("0102-1504", suffix); err != nil {
		t.Errorf("suffix %q is not an MMDD-HHMM timestamp: %v", suffix, err)
	}
}
