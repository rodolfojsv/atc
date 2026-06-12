package tui

import (
	"io/fs"
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
	// Slash commands: only as the very first token.
	if strings.HasPrefix(val, "/") && !strings.ContainsAny(val, " \n") {
		var items []string
		for _, c := range slashCommands {
			if strings.HasPrefix(c.name, val) {
				items = append(items, c.name+"  —  "+c.desc)
			}
		}
		if len(items) > 0 && !(len(items) == 1 && strings.HasPrefix(items[0], val+" ")) {
			m.comp = completion{active: true, kind: '/', token: val, items: items}
		}
		return
	}
	// File mention: last whitespace-separated token starting with @.
	fields := strings.FieldsFunc(val, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' })
	if len(fields) == 0 {
		return
	}
	last := fields[len(fields)-1]
	if !strings.HasPrefix(last, "@") || !strings.HasSuffix(val, last) {
		return
	}
	query := strings.TrimPrefix(last, "@")
	items := fuzzyFilter(m.sessionFiles(), query, maxCompletionItems)
	if len(items) > 0 {
		m.comp = completion{active: true, kind: '@', token: last, items: items}
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

// acceptCompletion inserts the selected item into the prompt.
func (m *Model) acceptCompletion() {
	if !m.comp.active || len(m.comp.items) == 0 {
		return
	}
	choice := m.comp.items[m.comp.sel]
	if m.comp.kind == '/' {
		choice = strings.SplitN(choice, "  —  ", 2)[0]
	}
	val := m.input.Value()
	val = strings.TrimSuffix(val, m.comp.token)
	if m.comp.kind == '@' {
		m.input.SetValue(val + choice + " ")
	} else {
		m.input.SetValue(choice + " ")
	}
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
