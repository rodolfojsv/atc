package supervisor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodolfojsv/atc/internal/config"
	"time"
)

func TestAutoName(t *testing.T) {
	for _, c := range []struct{ prompt, repo, want string }{
		{"Fix the login flow please", "/home/rodo/atc", "Fix-the-login-flow-please"},
		{"one two three four five six seven eight", "/r", "one-two-three-four-five-six"}, // capped to 6 words
		{"", "/home/rodo/Development/atc", "atc"},                                        // no prompt → repo base
		{"   ", "/x/my-repo", "my-repo"},
	} {
		if got := autoName(c.prompt, c.repo); got != c.want {
			t.Errorf("autoName(%q,%q) = %q, want %q", c.prompt, c.repo, got, c.want)
		}
	}
	if got := autoName("", ""); !strings.HasPrefix(got, "session-") {
		t.Errorf("no prompt or repo: got %q, want session- fallback", got)
	}
}

func TestRename(t *testing.T) {
	s := New(testConfig(t), nil)
	s.store = store{path: filepath.Join(t.TempDir(), "sessions.json")}
	a := &Session{Name: "alpha", id: "a1", status: StatusWorking}
	b := &Session{Name: "beta", id: "b1", status: StatusWorking}
	s.sessions = []*Session{a, b}

	if err := s.Rename(a, "Fancy Name!"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if a.View().Name != "Fancy-Name" {
		t.Errorf("name not slugged: %q", a.View().Name)
	}
	if err := s.Rename(a, "beta"); err == nil {
		t.Error("rename onto an existing name should error")
	}
	if err := s.Rename(a, "   "); err == nil {
		t.Error("empty rename should error")
	}
}

func TestStoreRoundtrip(t *testing.T) {
	st := store{path: filepath.Join(t.TempDir(), "sessions.json")}
	in := []savedSession{
		{ID: "abc-123", Name: "api-refactor", Repo: "/r", Dir: "/d", Worktree: "/w", Branch: "atc/api-refactor", Preset: "default", Created: time.Now().UTC()},
		{ID: "def-456", Name: "tests", Repo: "/r2", Dir: "/r2", Created: time.Now().UTC()},
	}
	if err := st.save(in); err != nil {
		t.Fatal(err)
	}
	out := st.load()
	if len(out) != 2 || out[0].ID != "abc-123" || out[1].Name != "tests" || out[0].Branch != "atc/api-refactor" {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestStoreMissingOrDisabled(t *testing.T) {
	if got := (store{path: filepath.Join(t.TempDir(), "nope.json")}).load(); got != nil {
		t.Errorf("missing file should load nil, got %+v", got)
	}
	disabled := store{}
	if err := disabled.save([]savedSession{{ID: "x"}}); err != nil {
		t.Errorf("disabled store save should be a no-op, got %v", err)
	}
}

func TestPersistMergesForeignEntries(t *testing.T) {
	st := store{path: filepath.Join(t.TempDir(), "sessions.json")}
	// A headless `atc run` (another process) wrote its finished session.
	foreign := savedSession{ID: "foreign-1", Name: "pr-triage", Repo: "/r", Dir: "/r", Status: "done", Created: time.Now().UTC()}
	if err := st.save([]savedSession{foreign}); err != nil {
		t.Fatal(err)
	}

	s := New(testConfig(t), nil)
	s.store = st
	s.sessions = []*Session{{Name: "mine", Repo: "/m", Dir: "/m", id: "mine-1", status: StatusWorking}}

	// Persisting our sessions must keep the foreign entry.
	s.persist()
	got := st.load()
	ids := map[string]bool{}
	for _, sv := range got {
		ids[sv.ID] = true
	}
	if !ids["mine-1"] || !ids["foreign-1"] {
		t.Fatalf("merge lost an entry: %+v", got)
	}

	// Once killed here, the foreign entry must not be resurrected.
	s.killed["foreign-1"] = true
	s.persist()
	for _, sv := range st.load() {
		if sv.ID == "foreign-1" {
			t.Fatal("killed session resurrected from disk")
		}
	}
}

// Pin and category are board-organization metadata that must survive a
// persist→load cycle and drive the supervisor's mutators / Categories().
func TestPinCategoryRoundtripAndMutators(t *testing.T) {
	st := store{path: filepath.Join(t.TempDir(), "sessions.json")}
	s := New(testConfig(t), nil)
	s.store = st
	pinned := &Session{Name: "alpha", Repo: "/a", Dir: "/a", id: "a1", status: StatusWorking, pinned: true, category: "smib"}
	plain := &Session{Name: "beta", Repo: "/b", Dir: "/b", id: "b1", status: StatusWorking, category: "invitably"}
	s.sessions = []*Session{pinned, plain}

	// Categories: distinct, sorted, empty excluded.
	if got := s.Categories(); len(got) != 2 || got[0] != "invitably" || got[1] != "smib" {
		t.Fatalf("Categories() = %v, want [invitably smib]", got)
	}

	// Mutators persist; reload must reflect the change (whitespace trimmed).
	s.SetCategory(plain, "  scratch  ")
	s.SetPinned(plain, true)
	byID := map[string]savedSession{}
	for _, sv := range st.load() {
		byID[sv.ID] = sv
	}
	if sv := byID["a1"]; !sv.Pinned || sv.Category != "smib" {
		t.Fatalf("alpha not persisted: %+v", sv)
	}
	if sv := byID["b1"]; !sv.Pinned || sv.Category != "scratch" {
		t.Fatalf("beta mutation not persisted: %+v", sv)
	}

	// Clearing the category empties it (omitempty → absent on disk, reads "").
	s.SetCategory(plain, "")
	if plain.View().Category != "" {
		t.Fatalf("category not cleared: %q", plain.View().Category)
	}
}

func TestSettled(t *testing.T) {
	for status, want := range map[string]bool{"done": true, "error": true, "": true, "working": false, "starting": false} {
		if got := (savedSession{Status: status}).settled(); got != want {
			t.Errorf("settled(%q) = %v, want %v", status, got, want)
		}
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestUsageSnapshotRoundtrip(t *testing.T) {
	st := store{path: filepath.Join(t.TempDir(), "sessions.json")}
	in := []savedSession{{
		ID: "u1", Name: "x", Repo: "/r", Dir: "/r", Status: "done", Created: time.Now().UTC(),
		InTokens: 12400, OutTokens: 8100, NanoAiu: 4.2e8, CostUSD: 0.05,
		CurrentTokens: 45000, TokenLimit: 128000,
	}}
	if err := st.save(in); err != nil {
		t.Fatal(err)
	}
	got := st.load()
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	sv := got[0]
	if sv.InTokens != 12400 || sv.OutTokens != 8100 || sv.NanoAiu != 4.2e8 ||
		sv.CostUSD != 0.05 || sv.CurrentTokens != 45000 || sv.TokenLimit != 128000 {
		t.Fatalf("usage snapshot lost: %+v", sv)
	}
}

func TestPersistWritesUsageSnapshot(t *testing.T) {
	st := store{path: filepath.Join(t.TempDir(), "sessions.json")}
	s := New(testConfig(t), nil)
	s.store = st
	sess := &Session{Name: "x", Repo: "/r", Dir: "/r", id: "u2", status: StatusDone}
	sess.usage = Usage{InputTokens: 100, OutputTokens: 50, NanoAiu: 1e8, CostUSD: 0.01, CurrentTokens: 9000, TokenLimit: 128000}
	s.sessions = []*Session{sess}
	s.persist()
	got := st.load()
	if len(got) != 1 || got[0].InTokens != 100 || got[0].NanoAiu != 1e8 || got[0].TokenLimit != 128000 {
		t.Fatalf("persist did not snapshot usage: %+v", got)
	}
}

func TestPersistKeepsConfiguredModelWithoutUsage(t *testing.T) {
	st := store{path: filepath.Join(t.TempDir(), "sessions.json")}
	s := New(testConfig(t), nil)
	s.store = st
	// Configured model, but no turn has run yet (usage.Model empty).
	s.sessions = []*Session{{Name: "x", Repo: "/r", Dir: "/r", id: "m1", status: StatusIdle, Model: "gpt-5"}}
	s.persist()
	got := st.load()
	if len(got) != 1 || got[0].Model != "gpt-5" {
		t.Fatalf("configured model not persisted: %+v", got)
	}
}
