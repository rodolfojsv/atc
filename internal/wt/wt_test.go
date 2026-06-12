package wt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestDiffAndMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	writeFile(t, repo, "a.txt", "original\n")
	commit(t, repo, "add a.txt")

	m := Manager{Root: t.TempDir()}
	baseBranch, baseCommit, err := m.Base(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir, branch, err := m.Create(repo, "feat")
	if err != nil {
		t.Fatal(err)
	}

	// The agent edits a tracked file and adds a new one, no commit.
	writeFile(t, dir, "a.txt", "changed\n")
	writeFile(t, dir, "new.txt", "fresh\n")

	diff, err := m.Diff(dir, baseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "changed") || !strings.Contains(diff, "new.txt") {
		t.Fatalf("diff missing changes:\n%s", diff)
	}

	if err := m.Merge(repo, dir, branch, baseBranch, "atc: test merge"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil || string(data) != "changed\n" {
		t.Fatalf("merge did not land: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatal("untracked file did not merge:", err)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}
