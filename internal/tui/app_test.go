package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

func testModel(t *testing.T) *Model {
	t.Helper()
	cfg, err := config.Load("/nonexistent/atc-config.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New(supervisor.New(cfg, bus.New()), cfg)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func TestEmptyBoardRenders(t *testing.T) {
	m := testModel(t)
	v := m.View()
	if !strings.Contains(v, "agent traffic control") {
		t.Error("missing title")
	}
	if !strings.Contains(v, "no sessions") {
		t.Error("missing empty-state hint")
	}
}

func TestNewSessionFormOpensAndCancels(t *testing.T) {
	m := testModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.mode != modeNew {
		t.Fatalf("expected form mode, got %v", m.mode)
	}
	if v := m.View(); !strings.Contains(v, "new session") || !strings.Contains(v, "Worktree") {
		t.Error("form missing fields")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeBoard {
		t.Error("esc should return to board")
	}
}

func TestQuitWithNoSessions(t *testing.T) {
	m := testModel(t)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("expected tea.QuitMsg with no active sessions")
	}
}

func TestFormSubmitRequiresRepo(t *testing.T) {
	m := testModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	_, _ = m.submitForm()
	if m.mode != modeNew || m.flash == "" {
		t.Error("submit without repo should stay on form with a flash message")
	}
}

func TestTranscriptRendering(t *testing.T) {
	m := testModel(t)
	md := m.renderEntry(supervisor.Entry{Kind: supervisor.EntryAssistant, Text: "# Findings\n\nThe bug is in **main.go**."})
	if !strings.Contains(md, "Findings") || !strings.Contains(md, "main.go") {
		t.Errorf("markdown entry lost content:\n%s", md)
	}
	tool := m.renderEntry(supervisor.Entry{Kind: supervisor.EntryTool, Text: "bash · go test ./..."})
	if !strings.Contains(tool, "go test ./...") {
		t.Errorf("tool entry lost content:\n%s", tool)
	}
	user := m.renderEntry(supervisor.Entry{Kind: supervisor.EntryUser, Text: "fix the bug"})
	if !strings.Contains(user, "fix the bug") || !strings.Contains(user, "❯") {
		t.Errorf("user entry malformed:\n%s", user)
	}
}

func TestBoardMotion(t *testing.T) {
	const n = 10 // a 10-row board, cursor 0..9
	// feed runs a key sequence from cursor 0 and returns the final cursor.
	feed := func(keys ...string) int {
		cursor, count, gPending := 0, 0, false
		for _, k := range keys {
			cursor, count, gPending, _ = boardMotion(cursor, n, count, gPending, k)
		}
		return cursor
	}

	cases := []struct {
		name string
		keys []string
		want int
	}{
		{"single j", []string{"j"}, 1},
		{"single k clamps at top", []string{"k"}, 0},
		{"count then j", []string{"1", "0", "j"}, 9}, // 10j past the end clamps
		{"smaller count j", []string{"3", "j"}, 3},
		{"G to bottom", []string{"G"}, 9},
		{"count G to row", []string{"5", "G"}, 4}, // 1-based row 5
		{"gg to top", []string{"G", "g", "g"}, 0}, // jump down then gg back up
		{"count gg to row", []string{"3", "g", "g"}, 2},
		{"down then count up", []string{"G", "2", "k"}, 7},
		{"lone zero is not a count", []string{"0", "j"}, 1}, // "0" ignored, j moves 1
		{"arrows honor count", []string{"4", "down"}, 4},
	}
	for _, c := range cases {
		if got := feed(c.keys...); got != c.want {
			t.Errorf("%s: cursor = %d, want %d", c.name, got, c.want)
		}
	}

	// A non-motion key clears a pending count and reports not-handled.
	if _, count, _, handled := boardMotion(0, n, 5, false, "n"); handled || count != 0 {
		t.Errorf("non-motion key: handled=%v count=%d, want false/0", handled, count)
	}
	// gg requires two presses: a lone g is handled but doesn't move.
	if cur, _, gp, handled := boardMotion(4, n, 0, false, "g"); cur != 4 || !gp || !handled {
		t.Errorf("lone g: cursor=%d gPending=%v handled=%v, want 4/true/true", cur, gp, handled)
	}
}

func TestHumanAIC(t *testing.T) {
	for in, want := range map[float64]string{
		0:       "—",
		5e6:     "<0.01", // 0.005 AIC
		4.2e8:   "0.42",
		1.25e10: "12.5",
	} {
		if got := humanAIC(in); got != want {
			t.Errorf("humanAIC(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestInputDisplayLinesGrowsWithWrapAndNewlines(t *testing.T) {
	m := testModel(t)
	m.layoutFocus()
	w := m.input.Width()
	if w <= 0 {
		t.Fatal("input width not set")
	}

	m.input.SetValue("short")
	if got := m.inputDisplayLines(); got != 1 {
		t.Errorf("short prompt: want 1 line, got %d", got)
	}

	// One logical line wider than the box must count as 2+ display rows.
	m.input.SetValue(strings.Repeat("x", w+5))
	if got := m.inputDisplayLines(); got < 2 {
		t.Errorf("wrapped prompt: want >=2 lines, got %d", got)
	}

	// Wrapped line plus an explicit newline (the ctrl+j case).
	m.input.SetValue(strings.Repeat("x", w+5) + "\nsecond")
	if got := m.inputDisplayLines(); got < 3 {
		t.Errorf("wrapped + newline: want >=3 lines, got %d", got)
	}
}
