package wt

import (
	"os/exec"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"My Task!":  "My-Task",
		"  ":        "session",
		"a/b\\c":    "a-b-c",
		"normal-1":  "normal-1",
		".hidden..": "hidden",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateSkipsLeftoverBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	m := Manager{Root: t.TempDir()}

	d1, b1, err := m.Create(repo, "task")
	if err != nil {
		t.Fatal(err)
	}
	if b1 != "atc/task" {
		t.Errorf("first branch = %q", b1)
	}

	// Same name again — the old branch and dir still exist, which used
	// to fail; now it must pick the next free suffix.
	d2, b2, err := m.Create(repo, "task")
	if err != nil {
		t.Fatal(err)
	}
	if b2 != "atc/task-2" || d2 == d1 {
		t.Errorf("collision not skipped: dir=%q branch=%q", d2, b2)
	}

	if err := m.Remove(repo, d2, b2); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	m := Manager{Root: t.TempDir()}
	if _, _, err := m.Create(t.TempDir(), "x"); err == nil {
		t.Error("expected error for non-git directory")
	}
}
