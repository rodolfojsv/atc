package supervisor

import (
	"path/filepath"
	"testing"
	"time"
)

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
