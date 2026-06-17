package tui

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Completion overlay for the focus prompt box: "@" fuzzy-picks a file
// from the session's working directory (inserting its relative path),
// "/" at the start of the prompt picks an atc command.

const (
	maxCompletionItems = 6
	maxFileWalk        = 4000
	fileCacheTTL       = 30 * time.Second
)

var slashCommands = []struct{ name, desc string }{
	{"/model", "show or switch the model (e.g. /model gpt-5)"},
	{"/diff", "view the worktree diff"},
	{"/export", "export the transcript to markdown"},
	{"/abort", "abort the current turn"},
	{"/auto", "toggle auto-approve ⚡"},
	{"/skills", "list this repo's skills and commands"},
	{"/help", "list commands"},
}

type completion struct {
	active bool
	kind   byte   // '@' or '/'
	token  string // the text being completed, including the trigger char
	items  []string
	sel    int
}

// syncCompletion recomputes the overlay from the prompt's current text.
// v1 assumption: completion applies to the token at the end of the
// text (where the cursor is while typing).
func (m *Model) syncCompletion() {
	m.comp = completion{}
	val := m.input.Value()
	if val == "" {
		return
	}
	// Completion targets the token at the cursor (end of the input): a
	// "/command" or "@file", each preceded by start or whitespace — so a
	// slash works mid-prompt or on a later line, not just as the first
	// character. atc's own commands come first, then the session's loaded
	// commands and skills (Claude and Copilot both invoke these).
	fields := strings.FieldsFunc(val, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' })
	if len(fields) == 0 {
		return
	}
	last := fields[len(fields)-1]
	if !strings.HasSuffix(val, last) { // cursor sits past the token (trailing space)
		return
	}
	if strings.HasPrefix(last, "/") {
		var items []string
		for _, c := range slashCommands {
			if strings.HasPrefix(c.name, last) {
				items = append(items, c.name+"  —  "+c.desc)
			}
		}
		for _, c := range m.backendCommands() {
			if strings.HasPrefix(c.name, last) {
				desc := c.desc
				if desc == "" {
					desc = "repo command"
				}
				items = append(items, c.name+"  —  "+desc)
			}
		}
		if len(items) > 0 && !(len(items) == 1 && strings.HasPrefix(items[0], last+" ")) {
			m.comp = completion{active: true, kind: '/', token: last, items: items}
		}
		return
	}
	if strings.HasPrefix(last, "@") {
		query := strings.TrimPrefix(last, "@")
		items := fuzzyFilter(m.sessionFiles(), query, maxCompletionItems)
		if len(items) > 0 {
			m.comp = completion{active: true, kind: '@', token: last, items: items}
		}
	}
}

// sessionFiles walks the focused session's working directory (cached
// briefly; heavy dirs skipped) and returns relative paths.
func (m *Model) sessionFiles() []string {
	if m.target == nil {
		return nil
	}
	dir := m.target.View().Dir
	if dir == m.fileListDir && time.Since(m.fileListAt) < fileCacheTTL {
		return m.fileList
	}
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
	m.fileList, m.fileListDir, m.fileListAt = files, dir, time.Now()
	return files
}

// fuzzyFilter keeps candidates whose runes contain the query as a
// case-insensitive subsequence, ranked: basename substring matches
// first, then earlier/shorter matches.
func fuzzyFilter(items []string, query string, max int) []string {
	q := strings.ToLower(query)
	type scored struct {
		s     string
		score int
	}
	var out []scored
	for _, it := range items {
		low := strings.ToLower(it)
		pos := subsequenceAt(low, q)
		if pos < 0 {
			continue
		}
		score := -pos - len(it)
		base := strings.ToLower(filepath.Base(it))
		if q == "" || strings.Contains(base, q) {
			score += 10_000
		}
		out = append(out, scored{it, score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > max {
		out = out[:max]
	}
	res := make([]string, len(out))
	for i, s := range out {
		res[i] = s.s
	}
	return res
}

// subsequenceAt reports the index of the first matched rune when q is
// a subsequence of s, or -1.
func subsequenceAt(s, q string) int {
	if q == "" {
		return 0
	}
	first := -1
	i := 0
	qr := []rune(q)
	for pos, r := range s {
		if i < len(qr) && r == qr[i] {
			if first == -1 {
				first = pos
			}
			i++
			if i == len(qr) {
				return first
			}
		}
	}
	return -1
}

// slashItem is one completion entry: an invocable "/name" and an
// optional description.
type slashItem struct{ name, desc string }

// backendCommands merges the focused session's invocable commands and
// skills for "/" completion: the backend's authoritative loaded list
// (Claude's init event / Copilot's RPC — built-in, plugin, user, repo)
// plus a filesystem scan of the Claude .claude layout (repo + user) so
// repo entries appear immediately, with descriptions.
func (m *Model) backendCommands() []slashItem {
	if m.target == nil {
		return nil
	}
	dir := m.target.View().Dir
	seen := map[string]bool{}
	var out []slashItem
	add := func(name, desc string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, slashItem{name, desc})
	}
	// The .claude filesystem scan describes the Claude layout only;
	// Copilot's .github assets come from the authoritative RPC list.
	if m.target.View().Backend == "claude" {
		for _, c := range m.repoCommands() {
			add(c, "")
		}
		for _, s := range claudeSkills(dir) {
			add(s.name, s.desc)
		}
		if home, err := os.UserHomeDir(); err == nil {
			for _, s := range claudeSkills(home) {
				add(s.name, s.desc)
			}
		}
	}
	for _, c := range m.target.SlashCommands(context.Background()) {
		add("/"+c.Name, c.Description)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// claudeSkills lists dir's .claude/skills/*/SKILL.md as invocable
// "/skill" names with their frontmatter descriptions.
func claudeSkills(dir string) []slashItem {
	var out []slashItem
	for _, p := range globAll(filepath.Join(dir, ".claude", "skills", "*", "SKILL.md")) {
		out = append(out, slashItem{name: "/" + filepath.Base(filepath.Dir(p)), desc: frontmatterDesc(p)})
	}
	return out
}

// frontmatterDesc pulls the "description:" value from a markdown file's
// YAML frontmatter, or "" if there's none.
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
		if rest, ok := strings.CutPrefix(t, "description:"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// repoCommands lists the session repo's .claude/commands/*.md as
// invocable names (subdirectories become namespaces, "/ns:cmd").
func (m *Model) repoCommands() []string {
	if m.target == nil {
		return nil
	}
	dir := filepath.Join(m.target.View().Dir, ".claude", "commands")
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		out = append(out, "/"+strings.ReplaceAll(name, "/", ":"))
		return nil
	})
	sort.Strings(out)
	return out
}

// skillsInventory describes the repo's agent assets for /skills:
// Copilot's .github layout (agents/, skills/, instructions/,
// copilot-instructions.md) and Claude's .claude layout (skills/,
// commands/), plus shared instruction files. Everything listed is
// loaded by the respective agent itself; atc only surfaces it.
func (m *Model) skillsInventory() []string {
	if m.target == nil {
		return nil
	}
	dir := m.target.View().Dir
	var out []string

	// Copilot: .github/skills (SKILL.md folders or flat .md files)
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
	// Copilot: custom agents
	for _, a := range globAll(filepath.Join(dir, ".github", "agents", "*.md")) {
		out = append(out, "agent: "+strings.TrimSuffix(filepath.Base(a), ".md")+" (.github/agents — copilot custom agent)")
	}
	// Copilot: scoped instruction files
	for _, i := range globAll(
		filepath.Join(dir, ".github", "instructions", "*.instructions.md"),
		filepath.Join(dir, ".github", "instructions", "*.md"),
	) {
		out = append(out, "instructions: "+filepath.Base(i)+" (.github/instructions — copilot)")
	}

	// Claude: skills and commands
	for _, s := range globAll(filepath.Join(dir, ".claude", "skills", "*", "SKILL.md")) {
		out = append(out, "skill: "+filepath.Base(filepath.Dir(s))+" (.claude/skills — claude, model-invoked when relevant)")
	}
	for _, c := range m.repoCommands() {
		out = append(out, "command: "+c+" (.claude/commands — type it in a claude session)")
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
	if len(out) == 0 {
		out = []string{"no skills, agents, commands, or instruction files found in " + dir}
	}
	return out
}

// globAll concatenates glob results, deduplicating across patterns
// (a flat .md pattern can re-match files a folder pattern found).
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

// acceptCompletion inserts the selected item into the prompt.
func (m *Model) acceptCompletion() {
	if !m.comp.active || len(m.comp.items) == 0 {
		return
	}
	choice := m.comp.items[m.comp.sel]
	if m.comp.kind == '/' {
		choice = strings.SplitN(choice, "  —  ", 2)[0]
	}
	// Replace just the completed token (it may sit after other text), so
	// "/" works mid-prompt the same way "@" does.
	val := strings.TrimSuffix(m.input.Value(), m.comp.token)
	m.input.SetValue(val + choice + " ")
	m.comp = completion{}
	m.syncFocusLayout()
}

func (m *Model) renderCompletion() string {
	var b strings.Builder
	for i, it := range m.comp.items {
		line := "  " + truncate(it, m.width-6)
		if i == m.comp.sel {
			line = styleKey.Render("▸ ") + truncate(it, m.width-6)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
