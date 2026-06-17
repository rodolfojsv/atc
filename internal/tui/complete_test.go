package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

func keyRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestFuzzyFilter(t *testing.T) {
	files := []string{"internal/tui/app.go", "internal/supervisor/session.go", "main.go", "README.md"}
	got := fuzzyFilter(files, "sess", 5)
	if len(got) != 1 || got[0] != "internal/supervisor/session.go" {
		t.Errorf("sess: %v", got)
	}
	if got := fuzzyFilter(files, "main", 5); len(got) == 0 || got[0] != "main.go" {
		t.Errorf("main should rank first: %v", got)
	}
	if got := fuzzyFilter(files, "zzz", 5); len(got) != 0 {
		t.Errorf("no match expected: %v", got)
	}
}

func TestSlashCompletionAndDispatch(t *testing.T) {
	m := testModel(t)
	m.mode = modeFocus
	m.input.SetValue("/mo")
	m.syncCompletion()
	if !m.comp.active || m.comp.kind != '/' {
		t.Fatalf("expected slash completion, got %+v", m.comp)
	}
	if !strings.HasPrefix(m.comp.items[0], "/model") {
		t.Errorf("expected /model first: %v", m.comp.items)
	}
	m.acceptCompletion()
	if got := m.input.Value(); got != "/model " {
		t.Errorf("accept inserted %q", got)
	}
	if m.comp.active {
		t.Error("overlay should close after accept")
	}
}

func TestSlashCompletionMidPrompt(t *testing.T) {
	m := testModel(t)
	m.mode = modeFocus
	m.input.SetValue("please run /mo")
	m.syncCompletion()
	if !m.comp.active || m.comp.kind != '/' || m.comp.token != "/mo" {
		t.Fatalf("expected mid-prompt slash completion on /mo, got %+v", m.comp)
	}
	m.acceptCompletion()
	if got := m.input.Value(); got != "please run /model " {
		t.Errorf("accept inserted %q, want %q", got, "please run /model ")
	}
}

func TestAtMentionCompletion(t *testing.T) {
	m := testModel(t)
	m.fileList = []string{"internal/policy/policy.go", "main.go"}
	m.fileListDir = "X"
	m.mode = modeFocus
	// Pretend the cache matches the (nil-target) session dir.
	m.input.SetValue("review @pol")
	// syncCompletion calls sessionFiles which needs target; stub via cache:
	// target nil → sessionFiles returns nil, so test fuzz path directly.
	items := fuzzyFilter(m.fileList, "pol", 6)
	if len(items) == 0 || items[0] != "internal/policy/policy.go" {
		t.Errorf("mention filter: %v", items)
	}
}

func TestSkillsInventoryCoversBothLayouts(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		".github/skills/deploy-check/SKILL.md",
		".github/agents/reviewer.md",
		".github/instructions/go.instructions.md",
		".github/copilot-instructions.md",
		".claude/skills/audit/SKILL.md",
		".claude/commands/triage.md",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := testModel(t)
	sess, err := m.sup.NewSession(supervisor.NewSessionOptions{Repo: dir, UseWorktree: false})
	if err != nil {
		t.Fatal(err)
	}
	m.target = sess
	inv := strings.Join(m.skillsInventory(), "\n")
	for _, want := range []string{"deploy-check", "reviewer", "go.instructions.md", "copilot-instructions.md", "audit", "/triage"} {
		if !strings.Contains(inv, want) {
			t.Errorf("inventory missing %q:\n%s", want, inv)
		}
	}
}
