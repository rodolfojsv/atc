package config

import (
	"path/filepath"
	"testing"
)

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
