package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

// sessionForm collects options for a new session: three text fields,
// a preset selector, and a worktree toggle.
type sessionForm struct {
	inputs   []textinput.Model // name, repo, prompt
	presets  []string
	preset   int
	worktree bool
	focus    int // 0..2 inputs, 3 preset, 4 worktree
}

const (
	formFieldPreset   = 3
	formFieldWorktree = 4
	formFieldCount    = 5
)

func newSessionForm(cfg *config.Config) sessionForm {
	name := textinput.New()
	name.Placeholder = "auto"
	name.CharLimit = 48

	repo := textinput.New()
	repo.Placeholder = "/path/to/repo"
	repo.SetValue(cfg.DefaultRepo)

	prompt := textinput.New()
	prompt.Placeholder = "optional first prompt"
	prompt.CharLimit = 0

	presets := []string{"default"}
	for n := range cfg.Presets {
		if n != "default" {
			presets = append(presets, n)
		}
	}

	f := sessionForm{
		inputs:   []textinput.Model{name, repo, prompt},
		presets:  presets,
		worktree: true,
	}
	f.inputs[0].Focus()
	return f
}

func (f *sessionForm) setFocus(i int) tea.Cmd {
	f.focus = (i + formFieldCount) % formFieldCount
	for j := range f.inputs {
		if j == f.focus {
			f.inputs[j].Focus()
		} else {
			f.inputs[j].Blur()
		}
	}
	if f.focus < len(f.inputs) {
		return textinput.Blink
	}
	return nil
}

func (m *Model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.form
	switch msg.String() {
	case "esc":
		m.mode = modeBoard
		return m, nil
	case "tab", "down":
		return m, f.setFocus(f.focus + 1)
	case "shift+tab", "up":
		return m, f.setFocus(f.focus - 1)
	case "left", "right":
		if f.focus == formFieldPreset {
			d := 1
			if msg.String() == "left" {
				d = len(f.presets) - 1
			}
			f.preset = (f.preset + d) % len(f.presets)
			return m, nil
		}
		if f.focus == formFieldWorktree {
			f.worktree = !f.worktree
			return m, nil
		}
	case " ":
		if f.focus == formFieldWorktree {
			f.worktree = !f.worktree
			return m, nil
		}
		if f.focus == formFieldPreset {
			f.preset = (f.preset + 1) % len(f.presets)
			return m, nil
		}
	case "enter":
		if f.focus < formFieldWorktree {
			return m, f.setFocus(f.focus + 1)
		}
		return m.submitForm()
	case "ctrl+s":
		return m.submitForm()
	}
	if f.focus < len(f.inputs) {
		var cmd tea.Cmd
		f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) submitForm() (tea.Model, tea.Cmd) {
	f := &m.form
	opts := supervisor.NewSessionOptions{
		Name:        strings.TrimSpace(f.inputs[0].Value()),
		Repo:        strings.TrimSpace(f.inputs[1].Value()),
		Prompt:      strings.TrimSpace(f.inputs[2].Value()),
		Preset:      f.presets[f.preset],
		UseWorktree: f.worktree,
	}
	if opts.Repo == "" {
		m.flash = "repo/directory is required"
		return m, nil
	}
	m.mode = modeBoard
	sup := m.sup
	return m, func() tea.Msg {
		// Worktree creation runs git; keep it off the UI goroutine.
		if _, err := sup.NewSession(opts); err != nil {
			return flashMsg{text: err.Error()}
		}
		return RefreshMsg{}
	}
}

func (m *Model) viewForm() string {
	f := &m.form
	labels := []string{"Name", "Repo", "Prompt"}
	var b strings.Builder
	b.WriteString(styleTitle.Render("new session") + "\n\n")
	for i, in := range f.inputs {
		cursor := "  "
		if f.focus == i {
			cursor = styleKey.Render("▸ ")
		}
		b.WriteString(cursor + styleHeader.Render(padRight(labels[i], 9)) + in.View() + "\n")
	}

	cursor := "  "
	if f.focus == formFieldPreset {
		cursor = styleKey.Render("▸ ")
	}
	b.WriteString(cursor + styleHeader.Render(padRight("Preset", 9)) + "◂ " + f.presets[f.preset] + " ▸\n")

	cursor = "  "
	if f.focus == formFieldWorktree {
		cursor = styleKey.Render("▸ ")
	}
	wtLabel := "[ ] run in repo directly"
	if f.worktree {
		wtLabel = "[x] fresh git worktree"
	}
	b.WriteString(cursor + styleHeader.Render(padRight("Worktree", 9)) + wtLabel + "\n")

	if m.flash != "" {
		b.WriteString("\n" + styleFlash.Render("  "+m.flash) + "\n")
	}
	b.WriteString("\n" + keybar("tab", "next", "space/←→", "toggle", "enter", "start", "esc", "cancel"))
	return m.center(styleModal.Width(min(m.width-4, 80)).Render(b.String()))
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
