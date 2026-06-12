// Package export writes a session transcript as a markdown note. Point
// the export directory at a folder inside your Obsidian vault and the
// note is in your vault — no plugin or API needed.
package export

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rodolfojsv/atc/internal/supervisor"
	"github.com/rodolfojsv/atc/internal/wt"
)

// DefaultDir is used when config has no exportDir.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "atc-exports"
	}
	return filepath.Join(home, ".atc", "exports")
}

// Write renders the session as markdown with YAML frontmatter and
// writes it to dir, returning the file path.
func Write(dir string, v supervisor.SessionView, entries []supervisor.Entry) (string, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	now := time.Now()
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.md", wt.Slug(v.Name), now.Format("2006-01-02-1504")))

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: atc session %s\n", v.Name)
	fmt.Fprintf(&b, "repo: %s\n", v.Repo)
	if v.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", v.Branch)
	}
	fmt.Fprintf(&b, "backend: %s\n", v.Backend)
	if v.Usage.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", v.Usage.Model)
	}
	fmt.Fprintf(&b, "status: %s\n", v.Status)
	fmt.Fprintf(&b, "tokens: %d in / %d out\n", v.Usage.InputTokens, v.Usage.OutputTokens)
	if v.Usage.NanoAiu > 0 {
		fmt.Fprintf(&b, "aic: %.3f\n", v.Usage.NanoAiu/1e9)
	}
	if v.Usage.CostUSD > 0 {
		fmt.Fprintf(&b, "costUsd: %.4f\n", v.Usage.CostUSD)
	}
	fmt.Fprintf(&b, "created: %s\n", v.Created.Format(time.RFC3339))
	fmt.Fprintf(&b, "exported: %s\n", now.Format(time.RFC3339))
	b.WriteString("tags: [atc, agent-session]\n")
	b.WriteString("---\n")

	for _, e := range entries {
		if e.Partial {
			continue
		}
		switch e.Kind {
		case supervisor.EntryUser:
			b.WriteString("\n## ❯ " + firstLine(e.Text) + "\n")
			if rest := restLines(e.Text); rest != "" {
				b.WriteString(rest + "\n")
			}
		case supervisor.EntryAssistant:
			b.WriteString("\n" + e.Text + "\n")
		case supervisor.EntryTool:
			b.WriteString("- ⚙ `" + e.Text + "`\n")
		case supervisor.EntryError:
			b.WriteString("- ✗ " + e.Text + "\n")
		case supervisor.EntrySystem:
			// atc housekeeping; not part of the conversation.
		}
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func restLines(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return ""
}
