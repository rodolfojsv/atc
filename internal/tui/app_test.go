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
