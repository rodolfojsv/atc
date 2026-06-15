package web

// Completion data for the web/phone prompt box: the repo's
// .claude/commands (for "/" pass-through commands), the working-dir
// file list (for "@" mentions), and a skills/commands inventory. The
// browser can't walk the filesystem itself, so the server supplies the
// raw lists once per session and the client fuzzy-filters them locally —
// no per-keystroke round trips. Mirrors the TUI's internal/tui/complete.go.

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxFileWalk  = 4000
	fileCacheTTL = 30 * time.Second
)

type cmdInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
}

type completeJSON struct {
	Commands []cmdInfo `json:"commands"`
	Files    []string  `json:"files"`
	Skills   []string  `json:"skills"`
}

// handleComplete serves the prompt-box completion lists for a session.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	v := sess.View()
	out := completeJSON{
		Files:  s.cachedFiles(v.Dir),
		Skills: skillsInventory(v.Dir),
	}
	// Only Claude sessions expand .claude/commands as pass-through "/"
	// commands; Copilot doesn't, so don't offer them there.
	if v.Backend == "claude" {
		out.Commands = repoCommands(v.Dir)
	}
	if out.Commands == nil {
		out.Commands = []cmdInfo{}
	}
	if out.Files == nil {
		out.Files = []string{}
	}
	writeJSON(w, out)
}

// cachedFiles returns the working dir's file list, walking at most once
// per fileCacheTTL per directory (a deep repo walk isn't free, and the
// same session is polled repeatedly).
func (s *Server) cachedFiles(dir string) []string {
	s.fileCacheMu.Lock()
	defer s.fileCacheMu.Unlock()
	if s.fileCache == nil {
		s.fileCache = map[string]fileCacheEntry{}
	}
	if e, ok := s.fileCache[dir]; ok && time.Since(e.at) < fileCacheTTL {
		return e.files
	}
	files := walkFiles(dir)
	s.fileCache[dir] = fileCacheEntry{files: files, at: time.Now()}
	return files
}

type fileCacheEntry struct {
	files []string
	at    time.Time
}

// walkFiles returns the dir's files as forward-slash relative paths,
// skipping heavy/uninteresting directories and capping the total.
func walkFiles(dir string) []string {
	skip := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "target": true, ".atc-worktrees": true, "__pycache__": true,
	}
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= maxFileWalk {
			return filepath.SkipAll
		}
		return nil
	})
	return files
}

// repoCommands lists the session repo's .claude/commands/*.md as
// invocable names (subdirectories become namespaces, "/ns:cmd"), with
// each command's frontmatter description when present.
func repoCommands(dir string) []cmdInfo {
	base := filepath.Join(dir, ".claude", "commands")
	var out []cmdInfo
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return nil
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		out = append(out, cmdInfo{
			Name: "/" + strings.ReplaceAll(name, "/", ":"),
			Desc: frontmatterDesc(path),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// frontmatterDesc pulls the "description:" value from a command/skill
// markdown file's YAML frontmatter, or "" if there's none.
func frontmatterDesc(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 2048)
	n, _ := f.Read(buf)
	text := string(buf[:n])
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	for _, ln := range strings.Split(text, "\n")[1:] {
		t := strings.TrimSpace(ln)
		if t == "---" {
			break
		}
		if rest, ok := cutPrefixFold(t, "description:"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// skillsInventory describes the repo's agent assets: Copilot's .github
// layout (skills/, agents/, instructions/, copilot-instructions.md) and
// Claude's .claude layout (skills/, commands/), plus shared instruction
// files. Everything listed is loaded by the agent itself; atc only
// surfaces it. Mirrors the TUI's skillsInventory.
func skillsInventory(dir string) []string {
	var out []string

	for _, s := range globAll(
		filepath.Join(dir, ".github", "skills", "*", "SKILL.md"),
		filepath.Join(dir, ".github", "skills", "*.md"),
	) {
		name := filepath.Base(filepath.Dir(s))
		if filepath.Base(s) != "SKILL.md" {
			name = strings.TrimSuffix(filepath.Base(s), ".md")
		}
		out = append(out, "skill: "+name+" (.github/skills — copilot, model-invoked when relevant)")
	}
	for _, a := range globAll(filepath.Join(dir, ".github", "agents", "*.md")) {
		out = append(out, "agent: "+strings.TrimSuffix(filepath.Base(a), ".md")+" (.github/agents — copilot custom agent)")
	}
	for _, i := range globAll(
		filepath.Join(dir, ".github", "instructions", "*.instructions.md"),
		filepath.Join(dir, ".github", "instructions", "*.md"),
	) {
		out = append(out, "instructions: "+filepath.Base(i)+" (.github/instructions — copilot)")
	}

	for _, s := range globAll(filepath.Join(dir, ".claude", "skills", "*", "SKILL.md")) {
		out = append(out, "skill: "+filepath.Base(filepath.Dir(s))+" (.claude/skills — claude, model-invoked when relevant)")
	}
	for _, c := range repoCommands(dir) {
		out = append(out, "command: "+c.Name+" (.claude/commands — type it in a claude session)")
	}

	for _, probe := range []struct{ path, label string }{
		{filepath.Join(dir, ".github", "copilot-instructions.md"), "copilot instructions: .github/copilot-instructions.md (loaded automatically)"},
		{filepath.Join(dir, "AGENTS.md"), "agent instructions: AGENTS.md (loaded automatically)"},
		{filepath.Join(dir, "CLAUDE.md"), "claude instructions: CLAUDE.md (loaded automatically)"},
	} {
		if _, err := os.Stat(probe.path); err == nil {
			out = append(out, probe.label)
		}
	}
	return out
}

// globAll concatenates glob results, deduplicating across patterns.
func globAll(patterns ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range patterns {
		matches, _ := filepath.Glob(p)
		for _, x := range matches {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out
}
