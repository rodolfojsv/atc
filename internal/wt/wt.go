// Package wt creates and removes per-session git worktrees so parallel
// agents never collide in the same checkout.
package wt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Manager creates worktrees under Root; empty Root means
// ~/.atc/worktrees/<repo-base>/<session-name>.
type Manager struct {
	Root string
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Slug makes a session name safe for branch and directory names.
func Slug(name string) string {
	s := unsafeChars.ReplaceAllString(strings.TrimSpace(name), "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "session"
	}
	return s
}

// Create adds a worktree for repo on a fresh atc/<name> branch and
// returns its path and branch name. Leftover branches/directories from
// earlier sessions of the same name are skipped with a numeric suffix
// rather than failing.
func (m Manager) Create(repo, name string) (dir, branch string, err error) {
	repo, err = filepath.Abs(repo)
	if err != nil {
		return "", "", err
	}
	if _, err := git(repo, "rev-parse", "--git-dir"); err != nil {
		return "", "", fmt.Errorf("%s is not a git repository: %w", repo, err)
	}

	base := m.Root
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		base = filepath.Join(home, ".atc", "worktrees", filepath.Base(repo))
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", "", err
	}

	slug := Slug(name)
	var lastErr error
	for i := 1; i <= 20; i++ {
		candidate := slug
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", slug, i)
		}
		branch = "atc/" + candidate
		dir = filepath.Join(base, candidate)
		if branchExists(repo, branch) || pathExists(dir) {
			continue
		}
		if _, lastErr = git(repo, "worktree", "add", "-b", branch, dir); lastErr == nil {
			return dir, branch, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no free worktree name for %q after 20 tries", slug)
	}
	return "", "", lastErr
}

func branchExists(repo, branch string) bool {
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Remove deletes the worktree and its branch. The branch delete is
// best-effort: it fails legitimately when the branch has unmerged work,
// and that work should survive the session.
func (m Manager) Remove(repo, dir, branch string) error {
	if _, err := git(repo, "worktree", "remove", "--force", dir); err != nil {
		return err
	}
	if branch != "" {
		_, _ = git(repo, "branch", "-d", branch)
	}
	return nil
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
