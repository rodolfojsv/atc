package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

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
	m.input.SetWidth(m.width - 6) // border + padding
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

// inputDisplayLines counts the rows the prompt actually occupies on
// screen: logical lines plus soft-wrap overflow. The textarea's own
// LineCount() reports only logical lines, which understates the height
// of wrapped paragraphs and made earlier lines scroll out of view.
func (m *Model) inputDisplayLines() int {
	w := m.input.Width()
	if w <= 0 {
		return 1
	}
	total := 0
	for _, line := range strings.Split(m.input.Value(), "\n") {
		lw := lipgloss.Width(line)
		rows := (lw + w - 1) / w
		// A line ending exactly at the boundary still wraps the cursor
		// onto the next row; +1 keeps that row visible while typing.
		if lw > 0 && lw%w == 0 {
			rows++
		}
		if rows < 1 {
			rows = 1
		}
		total += rows
	}
	if total < 1 {
		total = 1
	}
	return total
}

// syncFocusLayout grows the prompt box with its content (a long prompt
// wraps into a paragraph) and gives the viewport the remaining height.
// Chrome around them: title, permission banner slot, keybar, and the
// input box border (2 lines).
func (m *Model) syncFocusLayout() {
	lines := m.inputDisplayLines()
	if lines > maxInputLines {
		lines = maxInputLines
	}
	m.input.SetHeight(lines)
	h := m.height - 6 - lines
	if m.comp.active {
		h -= len(m.comp.items)
	}
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
	if m.comp.active {
		switch msg.String() {
		case "up":
			if m.comp.sel > 0 {
				m.comp.sel--
			}
			return m, nil
		case "down":
			if m.comp.sel < len(m.comp.items)-1 {
				m.comp.sel++
			}
			return m, nil
		case "tab", "enter":
			m.acceptCompletion()
			return m, nil
		case "esc":
			m.comp = completion{}
			m.syncFocusLayout()
			return m, nil
		}
	}
	// Left arrow returns to the board when the prompt is empty (there's
	// nothing to move the cursor over), complementing esc. With text in
	// the box it keeps its normal cursor-movement behavior.
	if msg.String() == "left" && strings.TrimSpace(m.input.Value()) == "" {
		m.mode = modeBoard
		m.input.Blur()
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
		if strings.HasPrefix(text, "/") {
			return m.runSlashCommand(sess, text)
		}
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
		sess.Respond(agent.ApproveOnce, "")
		return m, nil
	case "ctrl+n":
		sess.Respond(agent.Deny, "denied by user in atc")
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
	m.syncCompletion()
	m.syncFocusLayout()
	return m, cmd
}

// runSlashCommand dispatches atc's own prompt-box commands. Backend
// CLI slash commands (/fleet, /compact, …) don't exist over the SDK
// path — these are atc-native.
func (m *Model) runSlashCommand(sess *supervisor.Session, text string) (tea.Model, tea.Cmd) {
	cmd, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	sup := m.sup
	switch cmd {
	case "/model":
		if arg == "" {
			cur := sess.View().Model
			if cur == "" {
				cur = "backend default"
			}
			m.flash = "model: " + cur + " — /model <name> to switch"
			return m, nil
		}
		return m, func() tea.Msg {
			if err := sup.SwitchModel(sess, arg); err != nil {
				return flashMsg{text: err.Error()}
			}
			return RefreshMsg{}
		}
	case "/diff":
		return m.openDiff(sess)
	case "/export":
		return m.exportSession(sess)
	case "/abort":
		return m, func() tea.Msg { sup.Abort(sess); return RefreshMsg{} }
	case "/auto":
		on := !sess.View().AutoApprove
		sess.SetAutoApprove(on)
		if on {
			m.flash = "auto-approve ON (deny-list still applies)"
		} else {
			m.flash = "auto-approve off"
		}
		return m, nil
	case "/skills":
		for _, line := range m.skillsInventory() {
			sup.Note(sess, line)
		}
		return m, nil
	case "/help":
		names := make([]string, len(slashCommands))
		for i, c := range slashCommands {
			names[i] = c.name
		}
		m.flash = strings.Join(names, " · ") + " — @ mentions a file"
		return m, nil
	}
	// Claude sessions expand their repo's .claude/commands themselves —
	// pass unrecognized commands through as the prompt.
	if sess.View().Backend == "claude" {
		return m, func() tea.Msg {
			if err := sup.Prompt(sess, text); err != nil {
				return flashMsg{text: err.Error()}
			}
			return RefreshMsg{}
		}
	}
	m.flash = "unknown command " + cmd + " — /help lists them"
	return m, nil
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
	if cost := humanCost(v.Usage); cost != "—" {
		b.WriteString(styleDim.Render("  ·  " + cost))
	}
	if lm := m.sup.Limits(); !lm.AsOf.IsZero() && len(lm.Windows) > 0 {
		bind := lm.Windows[0]
		for _, w := range lm.Windows[1:] {
			if w.Pct > bind.Pct {
				bind = w
			}
		}
		b.WriteString(styleDim.Render(fmt.Sprintf("  ·  %.0f%% %s", bind.Pct, bind.Label)))
	}
	b.WriteString(styleDim.Render("  ·  " + v.Backend))
	b.WriteString("\n")
	b.WriteString(m.vp.View() + "\n")

	if v.Pending != nil {
		summary := v.Pending.Summary
		if v.PendingCount > 1 {
			summary = fmt.Sprintf("%s (+%d queued)", summary, v.PendingCount-1)
		}
		b.WriteString(styleBanner.Render("⚠ "+truncate(summary, m.width-30)) +
			" " + keybar("ctrl+y", "approve", "ctrl+n", "deny") + "\n")
	} else if v.Status == supervisor.StatusError {
		b.WriteString(styleErrSt.Render("✗ "+truncate(v.Err, m.width-4)) + "\n")
	} else {
		b.WriteString("\n")
	}

	if m.comp.active {
		b.WriteString(m.renderCompletion())
	}
	box := styleInputBox
	if m.input.Focused() {
		box = styleInputBoxFocused
	}
	b.WriteString(box.Width(m.width-2).Render(m.input.View()) + "\n")
	bottom := keybar("esc/←", "board", "enter", "send", "ctrl+j", "newline", "ctrl+x", "abort", "wheel", "scroll")
	if m.flash != "" {
		bottom += "  " + styleFlash.Render(m.flash)
	}
	model := v.Model
	if model == "" {
		model = "model: default"
	} else {
		model = "model: " + model
	}
	b.WriteString(rightAlign(m.width, bottom, styleDim.Render(model)))
	return b.String()
}
