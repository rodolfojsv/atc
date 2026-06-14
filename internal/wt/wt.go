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

var (
	pathBreakers = regexp.MustCompile(`[/\\\x00-\x1f]+`)
	multiSpace   = regexp.MustCompile(`\s+`)
)

// CleanName normalizes a human-facing session name: it keeps spaces and
// letter case (unlike Slug) so the board can show "fix login bug", and
// only strips characters that would break a file path or URL. Worktree
// branches and directories still go through Slug, so the display name
// and the on-disk name are decoupled. Returns "" when nothing usable
// remains.
func CleanName(name string) string {
	s := pathBreakers.ReplaceAllString(name, " ")
	s = multiSpace.ReplaceAllString(strings.TrimSpace(s), " ")
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

// Base reports the repo's current branch and commit — recorded at
// worktree creation so diffs and merges know the comparison point.
func (m Manager) Base(repo string) (branch, commit string, err error) {
	branch, err = git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", err
	}
	commit, err = git(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return branch, commit, nil
}

// Diff shows everything the session changed relative to its base
// commit: tracked changes (committed or not) as a unified diff, plus a
// listing of untracked files.
func (m Manager) Diff(dir, baseCommit string) (string, error) {
	target := "HEAD"
	if baseCommit != "" {
		target = baseCommit
	}
	diff, err := git(dir, "diff", target)
	if err != nil {
		return "", err
	}
	status, _ := git(dir, "status", "--porcelain")
	var untracked []string
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "?? ") {
			untracked = append(untracked, "  + "+strings.TrimPrefix(line, "?? "))
		}
	}
	var b strings.Builder
	if len(untracked) > 0 {
		b.WriteString("New untracked files:\n" + strings.Join(untracked, "\n") + "\n\n")
	}
	if strings.TrimSpace(diff) == "" && len(untracked) == 0 {
		return "(no changes)", nil
	}
	b.WriteString(diff)
	return b.String(), nil
}

// Merge commits everything in the worktree (if dirty) and merges its
// branch into baseBranch in the main repo. The repo must currently
// have baseBranch checked out; conflicts abort the merge cleanly.
func (m Manager) Merge(repo, dir, branch, baseBranch, message string) error {
	status, err := git(dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		if _, err := git(dir, "add", "-A"); err != nil {
			return err
		}
		if _, err := git(dir, "-c", "user.name=atc", "-c", "user.email=atc@localhost", "commit", "-m", message); err != nil {
			return err
		}
	}
	current, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if baseBranch != "" && current != baseBranch {
		return fmt.Errorf("repo has %s checked out, session branched from %s — switch branches first", current, baseBranch)
	}
	if _, err := git(repo, "merge", "--no-edit", branch); err != nil {
		_, _ = git(repo, "merge", "--abort")
		return fmt.Errorf("merge failed (aborted cleanly): %w", err)
	}
	return nil
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

// IgnoreLocally adds pattern to the repo's .git/info/exclude (the local,
// uncommitted ignore list) so atc scratch files like saved attachments
// never show up in status or get swept into a `git add -A`. It's a no-op
// when dir isn't a git repo (e.g. a scratch session) and idempotent: the
// pattern is appended only if not already present.
func (m Manager) IgnoreLocally(dir, pattern string) error {
	excl, err := git(dir, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return nil // not a git repo; nothing to ignore
	}
	if !filepath.IsAbs(excl) {
		excl = filepath.Join(dir, excl)
	}
	if data, err := os.ReadFile(excl); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(pattern + "\n")
	return err
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
