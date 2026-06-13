package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/export"
	"github.com/rodolfojsv/atc/internal/spend"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

// openDiff loads the session's diff into the shared viewport (worktree
// vs its base, or the repo's uncommitted changes for direct sessions).
func (m *Model) openDiff(sess *supervisor.Session) (tea.Model, tea.Cmd) {
	diff, err := m.sup.Diff(sess)
	if err != nil {
		m.flash = err.Error()
		return m, nil
	}
	m.target = sess
	m.mode = modeDiff
	m.layoutFocus()
	m.vp.SetContent(colorizeDiff(diff, m.width-1))
	m.vp.GotoTop()
	return m, nil
}

func (m *Model) updateDiff(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "d":
		m.mode = modeBoard
		return m, nil
	case "m":
		if v := m.target.View(); v.Worktree != "" {
			m.mode = modeMerge
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) viewDiff() string {
	v := m.target.View()
	var b strings.Builder
	title := " diff: " + v.Name
	if v.Worktree != "" {
		title += "  ·  " + v.Branch
		if v.BaseBranch != "" {
			title += " vs " + v.BaseBranch
		}
	} else {
		title += "  ·  uncommitted changes in repo"
	}
	b.WriteString(styleTitle.Render("atc") + title + "\n")
	b.WriteString(m.vp.View() + "\n\n")
	if v.Worktree != "" {
		b.WriteString(keybar("m", "merge into "+v.BaseBranch, "↑↓/wheel", "scroll", "esc", "back"))
	} else {
		b.WriteString(keybar("↑↓/wheel", "scroll", "esc", "back"))
	}
	if m.flash != "" {
		b.WriteString("  " + styleFlash.Render(m.flash))
	}
	return b.String()
}

func (m *Model) updateMerge(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		sess, sup := m.target, m.sup
		m.mode = modeBoard
		return m, func() tea.Msg {
			if err := sup.Merge(sess); err != nil {
				return flashMsg{text: "merge: " + err.Error()}
			}
			return flashMsg{text: sess.Name + " merged — K then w to clean up the worktree"}
		}
	case "esc", "n":
		m.mode = modeDiff
	}
	return m, nil
}

// colorizeDiff applies minimal +/- coloring to unified diff text.
func colorizeDiff(diff string, width int) string {
	lines := strings.Split(diff, "\n")
	for i, l := range lines {
		l = truncate(l, width)
		switch {
		case strings.HasPrefix(l, "+"):
			l = styleDone.Render(l)
		case strings.HasPrefix(l, "-"):
			l = styleErrSt.Render(l)
		case strings.HasPrefix(l, "@@"), strings.HasPrefix(l, "diff "):
			l = styleHeader.Render(l)
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// exportSession writes the selected session's transcript as markdown
// into the configured export directory (ideally inside your vault).
func (m *Model) exportSession(sess *supervisor.Session) (tea.Model, tea.Cmd) {
	path, err := export.Write(m.cfg.ExportDir, sess.View(), sess.Transcript())
	if err != nil {
		m.flash = "export: " + err.Error()
	} else {
		m.flash = "exported → " + path
	}
	return m, nil
}

// spendLabel renders ledger totals compactly for the board footer.
func spendLabel(t spend.Totals) string {
	parts := []string{}
	if t.NanoAiu > 0 {
		parts = append(parts, humanAIC(t.NanoAiu)+" aic")
	}
	if t.CostUSD > 0 {
		parts = append(parts, humanCost(supervisor.Usage{CostUSD: t.CostUSD}))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " + ")
}
