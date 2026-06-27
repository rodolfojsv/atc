package config

import (
	"path/filepath"
	"testing"
)

// Save then Load must round-trip a schedule (including the new Model field) so
// the web UI's schedule editor persists correctly.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	in := &Config{
		Repos: []string{"/home/u/repo-a"},
		Model: "claude-opus-4-8",
		Schedules: []Schedule{{
			Name: "nightly", Cron: "0 3 * * *", Repo: "/home/u/repo-a",
			Prompt: "summarize", Model: "claude-sonnet-4-6", Write: true, Disabled: true,
		}},
	}
	if err := Save(in, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out.Schedules) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(out.Schedules))
	}
	got := out.Schedules[0]
	if got.Name != "nightly" || got.Cron != "0 3 * * *" || got.Model != "claude-sonnet-4-6" ||
		!got.Write || !got.Disabled || got.Repo != "/home/u/repo-a" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestHasRepo(t *testing.T) {
	c := &Config{Repos: []string{"/home/u/repo-a", "/home/u/repo-b/"}}
	cases := []struct {
		path string
		want bool
	}{
		{"/home/u/repo-a", true},
		{"/home/u/repo-a/", true},         // trailing slash normalized
		{"/home/u/repo-b", true},          // configured entry has trailing slash
		{"/home/u/repo-a/../repo-a", true}, // resolves back to a configured repo
		{"/home/u/repo-a/../repo-c", false},
		{"/home/u/repo-c", false},
		{"/etc", false},
		{"", false},
		{filepath.Join("/home/u/repo-a", "sub"), false}, // a subdir is not the repo itself
	}
	for _, tc := range cases {
		if got := c.HasRepo(tc.path); got != tc.want {
			t.Errorf("HasRepo(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	if (&Config{}).HasRepo("/anything") {
		t.Error("empty Repos should allow nothing")
	}
}
