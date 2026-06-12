package tui

import (
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/muesli/reflow/wordwrap"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

func approveOnce() rpc.PermissionDecision {
	return &rpc.PermissionDecisionApproveOnce{}
}

func reject(feedback string) rpc.PermissionDecision {
	return &rpc.PermissionDecisionReject{Feedback: &feedback}
}

// focusChromeLines is the vertical space around the viewport: title,
// blank, permission banner slot, input, keybar.
const focusChromeLines = 5

func (m *Model) layoutFocus() {
	if m.width == 0 {
		return
	}
	h := m.height - focusChromeLines
	if h < 3 {
		h = 3
	}
	if m.vp.Width == 0 {
		m.vp = viewport.New(m.width, h)
	} else {
		m.vp.Width = m.width
		m.vp.Height = h
	}
	m.input.Width = m.width - 4
}

func (m *Model) refreshViewport() {
	if m.target == nil {
		return
	}
	lines := m.target.Transcript()
	wrapped := make([]string, 0, len(lines))
	for _, l := range lines {
		wrapped = append(wrapped, wordwrap.String(l, m.vp.Width-1))
	}
	m.vp.SetContent(strings.Join(wrapped, "\n"))
	if m.vpFollow {
		m.vp.GotoBottom()
	}
}

func (m *Model) updateFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sess := m.target
	if sess == nil {
		m.mode = modeBoard
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.mode = modeBoard
		m.input.Blur()
		return m, nil
	case "ctrl+c":
		if m.sup.ActiveCount() > 0 {
			m.mode = modeQuit
			return m, nil
		}
		return m, tea.Quit
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.SetValue("")
		m.vpFollow = true
		sup := m.sup
		return m, func() tea.Msg {
			if err := sup.Prompt(sess, text); err != nil {
				return flashMsg{text: err.Error()}
			}
			return RefreshMsg{}
		}
	case "ctrl+x":
		sup := m.sup
		return m, func() tea.Msg { sup.Abort(sess); return RefreshMsg{} }
	case "ctrl+y":
		sess.Respond(approveOnce())
		return m, nil
	case "ctrl+n":
		sess.Respond(reject("denied by user in atc"))
		return m, nil
	case "pgup", "pgdown", "home", "end", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.vpFollow = m.vp.AtBottom()
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) viewFocus() string {
	v := m.target.View()
	var b strings.Builder

	dir := v.Dir
	if v.Worktree != "" {
		dir = filepath.Base(v.Repo) + " @ " + v.Branch
	}
	title := " " + v.Name + "  ·  " + dir + "  ·  "
	b.WriteString(styleTitle.Render("atc") + title + statusLabel(v.Status))
	if v.Usage.TokenLimit > 0 {
		b.WriteString(styleDim.Render("  ·  ctx " + humanTokens(v.Usage.CurrentTokens) + "/" + humanTokens(v.Usage.TokenLimit)))
	}
	b.WriteString("\n")
	b.WriteString(m.vp.View() + "\n")

	if v.Pending != nil {
		b.WriteString(styleBanner.Render("⚠ "+truncate(v.Pending.Summary, m.width-30)) +
			" " + keybar("ctrl+y", "approve", "ctrl+n", "deny") + "\n")
	} else if v.Status == supervisor.StatusError {
		b.WriteString(styleErrSt.Render("✗ "+truncate(v.Err, m.width-4)) + "\n")
	} else {
		b.WriteString("\n")
	}

	b.WriteString("> " + m.input.View() + "\n")
	b.WriteString(keybar("esc", "board", "enter", "send", "ctrl+x", "abort", "pgup/pgdn", "scroll"))
	if m.flash != "" {
		b.WriteString("  " + styleFlash.Render(m.flash))
	}
	return b.String()
}
