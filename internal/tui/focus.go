package tui

import (
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
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

// maxInputLines caps how far the prompt box grows before it scrolls
// internally.
const maxInputLines = 6

func (m *Model) layoutFocus() {
	if m.width == 0 {
		return
	}
	if m.vp.Width == 0 {
		m.vp = viewport.New(m.width, 3)
	} else {
		m.vp.Width = m.width
	}
	m.input.SetWidth(m.width - 2)
	m.syncFocusLayout()

	if m.mdWidth != m.width {
		// Markdown rendering is width-dependent; rebuild the renderer
		// and drop the cache on resize.
		m.mdWidth = m.width
		m.mdCache = map[string]string{}
		r, err := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(min(m.width-2, 110)),
			glamour.WithEmoji(),
		)
		if err == nil {
			m.mdr = r
		}
	}
}

// syncFocusLayout grows the prompt box with its content (a long prompt
// wraps into a paragraph) and gives the viewport the remaining height.
// Chrome around them: title, permission banner slot, keybar.
func (m *Model) syncFocusLayout() {
	lines := m.input.LineCount()
	if lines < 1 {
		lines = 1
	}
	if lines > maxInputLines {
		lines = maxInputLines
	}
	m.input.SetHeight(lines)
	h := m.height - 4 - lines
	if h < 3 {
		h = 3
	}
	m.vp.Height = h
}

// renderMarkdown renders assistant text via glamour, caching by content
// (entries are immutable once complete).
func (m *Model) renderMarkdown(text string) string {
	if m.mdr == nil {
		return wordwrap.String(text, m.vp.Width-1)
	}
	if out, ok := m.mdCache[text]; ok {
		return out
	}
	out, err := m.mdr.Render(text)
	if err != nil {
		return wordwrap.String(text, m.vp.Width-1)
	}
	out = strings.Trim(out, "\n")
	if len(m.mdCache) > 500 {
		m.mdCache = map[string]string{}
	}
	m.mdCache[text] = out
	return out
}

func (m *Model) renderEntry(e supervisor.Entry) string {
	w := m.vp.Width - 1
	switch e.Kind {
	case supervisor.EntryUser:
		return styleUser.Render("❯ ") + styleUserText.Render(wordwrap.String(e.Text, w-2)) + "\n"
	case supervisor.EntryAssistant:
		if e.Partial {
			// Streaming text renders plain; it becomes markdown once the
			// message completes.
			return wordwrap.String(e.Text, w) + styleDim.Render(" ▍")
		}
		return m.renderMarkdown(e.Text) + "\n"
	case supervisor.EntryTool:
		return styleDim.Render(wordwrap.String("  ⚙ "+e.Text, w))
	case supervisor.EntrySystem:
		return styleDim.Render(wordwrap.String("  · "+e.Text, w))
	case supervisor.EntryError:
		return styleErrSt.Render(wordwrap.String("  ✗ "+e.Text, w))
	}
	return e.Text
}

func (m *Model) refreshViewport() {
	if m.target == nil {
		return
	}
	entries := m.target.Transcript()
	blocks := make([]string, 0, len(entries))
	for _, e := range entries {
		blocks = append(blocks, m.renderEntry(e))
	}
	m.vp.SetContent(strings.Join(blocks, "\n"))
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
		m.input.Reset()
		m.syncFocusLayout()
		m.histIdx, m.histDraft = -1, ""
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
	case "up", "down":
		if m.historyNav(sess, msg.String()) {
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.syncFocusLayout()
	return m, cmd
}

// historyNav recalls previous prompts shell-style: ↑ on the first line
// of the prompt box walks back through what you've sent (stashing the
// current draft), ↓ on the last line walks forward and finally restores
// the draft. Reports false when the key should move the cursor instead.
func (m *Model) historyNav(sess *supervisor.Session, dir string) bool {
	hist := sess.History()
	if dir == "up" {
		if m.input.Line() != 0 {
			return false
		}
		if len(hist) == 0 {
			return true // first line, nothing to recall: don't move
		}
		if m.histIdx == -1 {
			m.histDraft = m.input.Value()
			m.histIdx = len(hist)
		}
		if m.histIdx > 0 {
			m.histIdx--
			m.input.SetValue(hist[m.histIdx])
			m.syncFocusLayout()
		}
		return true
	}
	if m.histIdx == -1 || m.input.Line() != m.input.LineCount()-1 {
		return false
	}
	m.histIdx++
	if m.histIdx >= len(hist) {
		m.histIdx = -1
		m.input.SetValue(m.histDraft)
		m.histDraft = ""
	} else {
		m.input.SetValue(hist[m.histIdx])
	}
	m.syncFocusLayout()
	return true
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
	if v.Usage.NanoAiu > 0 {
		b.WriteString(styleDim.Render("  ·  " + humanAIC(v.Usage.NanoAiu) + " AIC"))
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

	b.WriteString(m.input.View() + "\n")
	b.WriteString(keybar("esc", "board", "enter", "send", "ctrl+j", "newline", "ctrl+x", "abort", "wheel", "scroll"))
	if m.flash != "" {
		b.WriteString("  " + styleFlash.Render(m.flash))
	}
	return b.String()
}
