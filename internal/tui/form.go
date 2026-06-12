package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

// sessionForm collects options for a new session. Rows: name, backend,
// repo picker (only when repos are configured), repo path, prompt,
// preset, worktree toggle. Enter submits from anywhere; tab/↑↓
// navigate; ←→/space cycle pickers.
type sessionForm struct {
	inputs   []textinput.Model // name, repo, prompt
	backends []string
	backend  int
	repos    []string // configured repo picker choices
	repoPick int
	presets  []string
	preset   int
	readOnly bool
	worktree bool
	focus    int
}

const (
	rowName = iota
	rowBackend
	rowRepoPick
	rowRepo
	rowPrompt
	rowPreset
	rowMode
	rowWorktree
	rowCount
)

// inputForRow maps a form row to its textinput index, or -1.
func inputForRow(row int) int {
	switch row {
	case rowName:
		return 0
	case rowRepo:
		return 1
	case rowPrompt:
		return 2
	}
	return -1
}

func newSessionForm(cfg *config.Config, backends []string) sessionForm {
	name := textinput.New()
	name.Placeholder = "auto"
	name.CharLimit = 48

	repo := textinput.New()
	repo.Placeholder = "/path/to/repo"

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
		backends: backends,
		repos:    cfg.Repos,
		presets:  presets,
		worktree: true,
	}
	switch {
	case cfg.DefaultRepo != "":
		f.inputs[1].SetValue(cfg.DefaultRepo)
	case len(cfg.Repos) > 0:
		f.inputs[1].SetValue(cfg.Repos[0])
	}
	f.inputs[0].Focus()
	return f
}

func (f *sessionForm) setFocus(row int) tea.Cmd {
	row = (row + rowCount) % rowCount
	if row == rowRepoPick && len(f.repos) == 0 {
		// Skip the picker row when no repos are configured.
		if row > f.focus || (f.focus == rowCount-1 && row == rowRepoPick) {
			row = rowRepo
		} else {
			row = rowBackend
		}
	}
	f.focus = row
	for j := range f.inputs {
		if j == inputForRow(row) {
			f.inputs[j].Focus()
		} else {
			f.inputs[j].Blur()
		}
	}
	if inputForRow(row) >= 0 {
		return textinput.Blink
	}
	return nil
}

// cycle moves a picker selection by delta within n choices.
func cycle(current, delta, n int) int {
	return ((current+delta)%n + n) % n
}

// cycleAt handles ←→/space on whichever picker row has focus; reports
// whether the key was consumed.
func (f *sessionForm) cycleAt(delta int) bool {
	switch f.focus {
	case rowBackend:
		f.backend = cycle(f.backend, delta, len(f.backends))
	case rowRepoPick:
		f.repoPick = cycle(f.repoPick, delta, len(f.repos))
		f.inputs[1].SetValue(f.repos[f.repoPick])
	case rowPreset:
		f.preset = cycle(f.preset, delta, len(f.presets))
	case rowMode:
		f.readOnly = !f.readOnly
	case rowWorktree:
		f.worktree = !f.worktree
	default:
		return false
	}
	return true
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
	case "left":
		if f.cycleAt(-1) {
			return m, nil
		}
	case "right", " ":
		if f.cycleAt(1) {
			return m, nil
		}
	case "enter", "ctrl+s":
		return m.submitForm()
	}
	if i := inputForRow(f.focus); i >= 0 {
		var cmd tea.Cmd
		f.inputs[i], cmd = f.inputs[i].Update(msg)
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
		Backend:     f.backends[f.backend],
		Preset:      f.presets[f.preset],
		UseWorktree: f.worktree,
		ReadOnly:    f.readOnly,
	}
	if opts.Repo == "" {
		m.flash = "repo/directory is required"
		return m, nil
	}
	// Validate synchronously so a bad path keeps the form open with the
	// error in view instead of silently doing nothing.
	if _, err := m.sup.NewSession(opts); err != nil {
		m.flash = err.Error()
		return m, nil
	}
	m.mode = modeBoard
	return m, nil
}

func (m *Model) viewForm() string {
	f := &m.form
	var b strings.Builder
	b.WriteString(styleTitle.Render("new session") + "\n\n")

	row := func(r int, label, body string) {
		cursor := "  "
		if f.focus == r {
			cursor = styleKey.Render("▸ ")
		}
		b.WriteString(cursor + styleHeader.Render(padRight(label, 9)) + body + "\n")
	}

	row(rowName, "Name", f.inputs[0].View())
	row(rowBackend, "Backend", "◂ "+f.backends[f.backend]+" ▸")
	if len(f.repos) > 0 {
		row(rowRepoPick, "Pick", "◂ "+truncate(f.repos[f.repoPick], 50)+" ▸")
	}
	row(rowRepo, "Repo", f.inputs[1].View())
	row(rowPrompt, "Prompt", f.inputs[2].View())
	row(rowPreset, "Preset", "◂ "+f.presets[f.preset]+" ▸")
	modeLabel := "[ ] normal — agent may act (per approval policy)"
	if f.readOnly {
		modeLabel = "[x] read-only — plan mode, inspect but never modify"
	}
	row(rowMode, "Mode", modeLabel)
	wtLabel := "[ ] run in repo directly"
	if f.worktree {
		wtLabel = "[x] fresh git worktree"
	}
	row(rowWorktree, "Worktree", wtLabel)

	if m.flash != "" {
		b.WriteString("\n" + styleFlash.Render("  "+m.flash) + "\n")
	}
	b.WriteString("\n" + keybar("tab/↑↓", "field", "←→/space", "choose", "enter", "start", "esc", "cancel"))
	return m.center(styleModal.Width(min(m.width-4, 80)).Render(b.String()))
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
