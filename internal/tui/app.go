// Package tui is atc's terminal interface: a session board with live
// status, a focus view for any session, a new-session form, and a
// permission approval modal.
package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

// RefreshMsg wakes the UI after supervisor state changes.
type RefreshMsg struct{}

type flashMsg struct{ text string }

// NewFlash builds a footer flash message that callers outside the
// package (e.g. main, announcing the web UI URL) can p.Send().
func NewFlash(text string) tea.Msg { return flashMsg{text: text} }

type mode int

const (
	modeBoard mode = iota
	modeFocus
	modeNew
	modePerm
	modeKill
	modeQuit
	modeDiff
	modeMerge
)

type Model struct {
	sup    *supervisor.Supervisor
	cfg    *config.Config
	mode   mode
	cursor int
	width  int
	height int
	flash  string

	target   *supervisor.Session // focused / modal subject
	vp       viewport.Model
	vpFollow bool
	input    textarea.Model

	// Markdown rendering for assistant transcript entries.
	mdr     *glamour.TermRenderer
	mdCache map[string]string
	mdWidth int

	// Prompt history recall (↑/↓ in the focus prompt box).
	histIdx   int // index into the focused session's history; -1 = not browsing
	histDraft string

	// Completion overlay (@ files, / commands) and its file cache.
	comp        completion
	fileList    []string
	fileListDir string
	fileListAt  time.Time

	form sessionForm
}

func New(sup *supervisor.Supervisor, cfg *config.Config) *Model {
	input := textarea.New()
	input.Placeholder = "prompt — enter to send, ctrl+j for a new line"
	input.CharLimit = 0
	input.ShowLineNumbers = false
	input.SetHeight(1)
	// Clean look: the surrounding rounded border (see viewFocus) is the
	// only chrome — no per-line gutter prompt, no cursor-line highlight.
	input.Prompt = ""
	input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	input.BlurredStyle.CursorLine = lipgloss.NewStyle()
	input.FocusedStyle.Placeholder = styleDim
	input.BlurredStyle.Placeholder = styleDim
	// Enter is reserved for sending; ctrl+j inserts a manual newline.
	// Long prompts soft-wrap into a growing paragraph either way.
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"))
	return &Model{sup: sup, cfg: cfg, vpFollow: true, input: input, histIdx: -1}
}

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutFocus()
		return m, nil
	case RefreshMsg:
		m.clampCursor()
		if m.mode == modeFocus && m.target != nil {
			m.refreshViewport()
		}
		// A focused/killed modal target may have errored or closed.
		if m.mode == modePerm && (m.target == nil || m.target.View().Pending == nil) {
			m.mode = modeBoard
		}
		return m, nil
	case flashMsg:
		m.flash = msg.text
		return m, nil
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.KeyMsg:
		m.flash = ""
		switch m.mode {
		case modeBoard:
			return m.updateBoard(msg)
		case modeFocus:
			return m.updateFocus(msg)
		case modeNew:
			return m.updateForm(msg)
		case modePerm:
			return m.updatePerm(msg)
		case modeKill:
			return m.updateKill(msg)
		case modeQuit:
			return m.updateQuit(msg)
		case modeDiff:
			return m.updateDiff(msg)
		case modeMerge:
			return m.updateMerge(msg)
		}
	}
	return m, nil
}

// updateMouse handles wheel scrolling: the transcript in focus view,
// the selection cursor on the board.
func (m *Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	wheelUp := msg.Button == tea.MouseButtonWheelUp
	wheelDown := msg.Button == tea.MouseButtonWheelDown
	if !wheelUp && !wheelDown {
		return m, nil
	}
	switch m.mode {
	case modeFocus, modeDiff:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.vpFollow = m.vp.AtBottom()
		return m, cmd
	case modeBoard:
		if wheelUp && m.cursor > 0 {
			m.cursor--
		}
		if wheelDown && m.cursor < len(m.sup.Sessions())-1 {
			m.cursor++
		}
	}
	return m, nil
}

func (m *Model) selected() *supervisor.Session {
	sessions := m.sup.Sessions()
	if len(sessions) == 0 || m.cursor >= len(sessions) {
		return nil
	}
	return sessions[m.cursor]
}

func (m *Model) clampCursor() {
	if n := len(m.sup.Sessions()); m.cursor >= n && n > 0 {
		m.cursor = n - 1
	} else if n == 0 {
		m.cursor = 0
	}
}

func (m *Model) updateBoard(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.sup.Sessions())-1 {
			m.cursor++
		}
	case "enter":
		if sess := m.selected(); sess != nil {
			m.target = sess
			m.mode = modeFocus
			m.vpFollow = true
			m.histIdx, m.histDraft = -1, ""
			m.layoutFocus()
			m.refreshViewport()
			return m, m.input.Focus()
		}
	case "n":
		m.form = newSessionForm(m.cfg, m.sup.Backends())
		m.mode = modeNew
		return m, textinput.Blink
	case "a":
		if sess := m.selected(); sess != nil && sess.View().Pending != nil {
			m.target = sess
			m.mode = modePerm
		}
	case "A":
		if sess := m.selected(); sess != nil {
			on := !sess.View().AutoApprove
			sess.SetAutoApprove(on)
			if on {
				m.flash = sess.Name + ": auto-approve ON (deny-list still applies)"
			} else {
				m.flash = sess.Name + ": auto-approve off"
			}
		}
	case "x":
		if sess := m.selected(); sess != nil {
			sup, target := m.sup, sess
			return m, func() tea.Msg { sup.Abort(target); return RefreshMsg{} }
		}
	case "K":
		if sess := m.selected(); sess != nil {
			m.target = sess
			m.mode = modeKill
		}
	case "d":
		if sess := m.selected(); sess != nil {
			return m.openDiff(sess)
		}
	case "e":
		if sess := m.selected(); sess != nil {
			return m.exportSession(sess)
		}
	case "q", "ctrl+c":
		if m.sup.ActiveCount() > 0 {
			m.mode = modeQuit
			return m, nil
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m *Model) updatePerm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sess := m.target
	if sess == nil {
		m.mode = modeBoard
		return m, nil
	}
	switch msg.String() {
	// After answering, stay in the modal: if more requests are queued
	// the next one surfaces here; when the queue empties, the refresh
	// guard returns to the board.
	case "y":
		sess.Respond(agent.ApproveOnce, "")
	case "n":
		sess.Respond(agent.Deny, "denied by user in atc")
	case "s":
		sess.Respond(agent.ApproveSession, "")
	case "a":
		sess.SetAutoApprove(true)
		m.flash = sess.Name + ": auto-approve ON (deny-list still applies)"
		m.mode = modeBoard
	case "esc":
		m.mode = modeBoard
	}
	return m, nil
}

func (m *Model) updateKill(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sess := m.target
	if sess == nil {
		m.mode = modeBoard
		return m, nil
	}
	kill := func(removeWorktree bool) tea.Cmd {
		sup := m.sup
		return func() tea.Msg {
			sup.Kill(sess, removeWorktree)
			return RefreshMsg{}
		}
	}
	switch msg.String() {
	case "y":
		m.mode = modeBoard
		return m, kill(false)
	case "w":
		m.mode = modeBoard
		return m, kill(true)
	case "esc", "n":
		m.mode = modeBoard
	}
	return m, nil
}

func (m *Model) updateQuit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		return m, tea.Quit
	case "esc", "n":
		m.mode = modeBoard
	}
	return m, nil
}

func (m *Model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	switch m.mode {
	case modeFocus:
		return m.viewFocus()
	case modeNew:
		return m.viewForm()
	case modePerm:
		return m.viewPerm()
	case modeKill:
		return m.modal(fmt.Sprintf("Kill session %q?", m.target.Name),
			keybar("y", "kill, keep worktree", "w", "kill + remove worktree", "esc", "cancel"))
	case modeQuit:
		return m.modal(fmt.Sprintf("%d active session(s) will be disconnected. Quit?", m.sup.ActiveCount()),
			keybar("y", "quit", "esc", "cancel"))
	case modeDiff:
		return m.viewDiff()
	case modeMerge:
		v := m.target.View()
		return m.modal(fmt.Sprintf("Commit all changes in %q and merge into %s?", m.target.Name, v.BaseBranch),
			keybar("y", "merge", "esc", "cancel"))
	}
	return m.viewBoard()
}

func (m *Model) viewBoard() string {
	var b strings.Builder
	title := styleTitle.Render("atc — agent traffic control")
	b.WriteString(title + "\n\n")

	sessions := m.sup.Sessions()
	if len(sessions) == 0 {
		b.WriteString(styleDim.Render("  no sessions — press ") + styleKey.Render("[n]") + styleDim.Render(" to launch an agent") + "\n")
	} else {
		nameW, dirW, tokW, costW, ctxW := 18, 22, 12, 9, 5
		header := fmt.Sprintf("  %-*s %-*s %-*s %*s %*s %*s  %s",
			nameW, "SESSION", dirW, "DIR", statusWidth, "STATUS", tokW, "TOKENS", costW, "COST", ctxW, "CTX", "DETAIL")
		b.WriteString(styleHeader.Render(header) + "\n")
		for i, sess := range sessions {
			v := sess.View()
			dir := filepath.Base(v.Dir)
			if v.Worktree != "" {
				dir = filepath.Base(v.Repo) + "@" + filepath.Base(v.Worktree)
			}
			tokens := "—"
			if v.Usage.InputTokens+v.Usage.OutputTokens > 0 {
				tokens = humanTokens(v.Usage.InputTokens) + "↑" + humanTokens(v.Usage.OutputTokens) + "↓"
			}
			ctx := "—"
			if v.Usage.TokenLimit > 0 {
				ctx = fmt.Sprintf("%d%%", v.Usage.CurrentTokens*100/v.Usage.TokenLimit)
			}
			detail := v.Intent
			if v.Status == supervisor.StatusWorking && v.SinceEvent > 2*time.Minute {
				detail = "⚠ no events for " + v.SinceEvent.Truncate(time.Minute).String() + " — x to abort, or restart atc to reattach"
			}
			switch {
			case v.Pending != nil:
				detail = styleWaiting.Render(truncate(v.Pending.Summary, m.width-70))
			case v.Status == supervisor.StatusError:
				detail = styleErrSt.Render(truncate(v.Err, m.width-70))
			case detail == "":
				detail = styleDim.Render(truncate(v.LastLine, m.width-70))
			default:
				detail = styleDim.Render(truncate(detail, m.width-70))
			}
			marker := ""
			if v.AutoApprove {
				marker = " ⚡"
			}
			if v.ReadOnly {
				marker += " 🔒"
			}
			name := truncate(v.Name+marker, nameW)
			if v.AutoApprove {
				name = styleAuto.Render(name)
			}
			row := fmt.Sprintf("  %-*s %-*s %s %*s %*s %*s  %s",
				nameW, name, dirW, truncate(dir, dirW),
				padANSI(statusLabel(v.Status), statusWidth),
				tokW, tokens, costW, humanCost(v.Usage), ctxW, ctx, detail)
			if i == m.cursor {
				row = styleSel.Render("▸") + row[1:]
			}
			b.WriteString(row + "\n")
		}
	}

	b.WriteString("\n")
	if m.flash != "" {
		b.WriteString(styleFlash.Render("  "+m.flash) + "\n")
	}
	today, month := m.sup.Spend()
	footer := styleDim.Render("  spend today " + spendLabel(today) + " · month " + spendLabel(month))
	if sess := m.selected(); sess != nil {
		if model := sess.View().Model; model != "" {
			footer = rightAlign(m.width, footer, styleDim.Render("model: "+model))
		}
	}
	b.WriteString(footer + "\n")
	b.WriteString("\n" + keybar(
		"enter", "attach", "n", "new", "a", "approve", "d", "diff", "e", "export", "A", "auto⚡", "x", "abort", "K", "kill", "q", "quit"))
	return b.String()
}

func (m *Model) viewPerm() string {
	v := m.target.View()
	if v.Pending == nil {
		return m.viewBoard()
	}
	banner := "⚠ permission request — " + m.target.Name + " (" + v.Pending.Kind + ")"
	if v.PendingCount > 1 {
		banner += fmt.Sprintf("  ·  %d more queued", v.PendingCount-1)
	}
	var lines []string
	lines = append(lines, styleBanner.Render(banner), "")
	max := m.height - 10
	detail := v.Pending.Detail
	if len(detail) > max && max > 0 {
		detail = append(append([]string{}, detail[:max]...), styleDim.Render(fmt.Sprintf("… (%d more lines)", len(detail)-max)))
	}
	for _, d := range detail {
		lines = append(lines, truncate(d, m.width-8))
	}
	lines = append(lines, "", keybar("y", "approve once", "s", "always (this kind, this session)", "a", "approve + auto⚡", "n", "deny", "esc", "back"))
	return m.center(styleModal.Width(min(m.width-4, 100)).Render(strings.Join(lines, "\n")))
}

func (m *Model) modal(title, footer string) string {
	return m.center(styleModal.Render(title + "\n\n" + footer))
}

func (m *Model) center(s string) string {
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, s)
}

// padANSI pads a styled string to a fixed visual width.
func padANSI(s string, w int) string {
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
