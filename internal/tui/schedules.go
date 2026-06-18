package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

// openSchedules renders the Scheduled section into the shared viewport:
// each configured task with its cron, next fire, and the sessions it has
// produced. Schedule-launched sessions are hidden from the main board once
// they finish and live here instead — selectable with ↑↓ and openable with
// enter, so you "follow the link" into the run's transcript.
func (m *Model) openSchedules() (tea.Model, tea.Cmd) {
	m.mode = modeSchedules
	m.schedCursor = 0
	m.layoutFocus()
	m.renderSchedules()
	m.vp.GotoTop()
	return m, nil
}

func (m *Model) updateSchedules(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "s":
		m.mode = modeBoard
		return m, nil
	case "up", "k":
		if m.schedCursor > 0 {
			m.schedCursor--
			m.renderSchedules()
			m.scrollSchedToCursor()
		}
		return m, nil
	case "down", "j":
		if m.schedCursor < len(m.schedSessions)-1 {
			m.schedCursor++
			m.renderSchedules()
			m.scrollSchedToCursor()
		}
		return m, nil
	case "enter":
		if m.schedCursor >= 0 && m.schedCursor < len(m.schedSessions) {
			m.target = m.schedSessions[m.schedCursor]
			m.mode = modeFocus
			m.vpFollow = true
			m.histIdx, m.histDraft = -1, ""
			m.layoutFocus()
			m.refreshViewport()
			return m, m.input.Focus()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) viewSchedules() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("atc") + " scheduled tasks\n")
	b.WriteString(m.vp.View() + "\n\n")
	if len(m.schedSessions) > 0 {
		b.WriteString(keybar("↑↓", "select", "enter", "open", "esc", "back"))
	} else {
		b.WriteString(keybar("↑↓/wheel", "scroll", "esc", "back"))
	}
	if m.flash != "" {
		b.WriteString("  " + styleFlash.Render(m.flash))
	}
	return b.String()
}

// renderSchedules rebuilds the viewport content and the flat selection list.
// It clamps the cursor to the number of openable sessions first, so the
// highlight in the rendered content lands on the row enter will open.
func (m *Model) renderSchedules() {
	openable := 0
	for _, sess := range m.sup.Sessions() {
		if sess.View().ScheduleName != "" {
			openable++
		}
	}
	if m.schedCursor >= openable {
		m.schedCursor = openable - 1
	}
	if m.schedCursor < 0 {
		m.schedCursor = 0
	}
	content, sessions, _ := m.schedulesContent()
	m.schedSessions = sessions
	m.vp.SetContent(content)
}

// scrollSchedToCursor keeps the highlighted session row inside the viewport
// when the list is longer than the visible window.
func (m *Model) scrollSchedToCursor() {
	_, _, selLine := m.schedulesContent()
	if selLine < 0 {
		return
	}
	top := m.vp.YOffset
	bottom := top + m.vp.Height - 1
	switch {
	case selLine < top:
		m.vp.SetYOffset(selLine)
	case selLine > bottom:
		m.vp.SetYOffset(selLine - m.vp.Height + 1)
	}
}

// schedulesContent renders the Scheduled section and returns, alongside the
// text, the openable sessions in display order and the viewport line of the
// currently selected one (-1 if there is no selection).
func (m *Model) schedulesContent() (string, []*supervisor.Session, int) {
	scheds := m.sup.Schedules()
	if len(scheds) == 0 {
		return styleDim.Render(`  no schedules configured — add them under "schedules" in config.json`), nil, -1
	}

	// Index the schedule-launched sessions atc currently knows (live on the
	// board or adopted from the store) by their schedule name.
	bySched := map[string][]*supervisor.Session{}
	for _, sess := range m.sup.Sessions() {
		if n := sess.View().ScheduleName; n != "" {
			bySched[n] = append(bySched[n], sess)
		}
	}

	var b strings.Builder
	var ordered []*supervisor.Session
	selLine := -1
	line := 0
	write := func(s string) {
		b.WriteString(s + "\n")
		line++
	}

	for _, s := range scheds {
		mode := styleDim.Render("  [read-only]")
		if s.Write {
			mode = styleErrSt.Render("  [writes]")
		}
		precheck := ""
		if s.HasPrecheck {
			precheck = styleDim.Render("  [precheck]")
		}
		next := "never"
		if !s.NextFire.IsZero() {
			next = "next " + relTime(s.NextFire)
		}
		since := "no updates yet"
		if !s.LastUpdate.IsZero() {
			since = "updated " + relTime(s.LastUpdate)
		}
		write(styleSection.Render("  "+s.Name) + styleDim.Render("  "+s.Cron) + mode + precheck)
		write(styleDim.Render(fmt.Sprintf("    %s · %s", since, next)))

		// The sessions this schedule has produced, newest first, as a
		// selectable list — the "link" into each run.
		sessions := bySched[s.Name]
		sort.SliceStable(sessions, func(i, j int) bool {
			return sessions[i].View().Created.After(sessions[j].View().Created)
		})
		for _, sess := range sessions {
			selected := len(ordered) == m.schedCursor
			if selected {
				selLine = line
			}
			write(scheduledSessionLine(sess.View(), selected))
			ordered = append(ordered, sess)
		}

		// Quiet/older fires from the run log, for context (no session to open).
		if len(sessions) == 0 && len(s.Runs) == 0 {
			write(styleDim.Render("    no runs recorded yet"))
		}
		for _, r := range s.Runs {
			// Skip "updated" runs whose session is already shown above.
			if r.Result == "updated" && r.Session != "" {
				continue
			}
			write("    " + runLine(r))
		}
		write("")
	}
	return b.String(), ordered, selLine
}

// scheduledSessionLine renders one selectable session row in the Scheduled
// view: a cursor marker, status, name, and age.
func scheduledSessionLine(v supervisor.SessionView, selected bool) string {
	marker := "    "
	if selected {
		marker = "  " + styleSel.Render("▸") + " "
	}
	row := fmt.Sprintf("%s%s  %s  %s",
		marker, padANSI(statusLabel(v.Status), statusWidth), v.Name, styleDim.Render(relTime(v.Created)))
	return row
}

// runLine renders one timeline entry: when it fired and what came of it.
func runLine(r supervisor.ScheduleRun) string {
	when := styleDim.Render(relTime(r.Time))
	switch r.Result {
	case "updated":
		return when + "  " + styleDone.Render("↗ "+r.Session)
	case "error":
		return when + "  " + styleErrSt.Render("✗ "+r.Detail)
	default:
		return when + "  " + styleDim.Render("no updates")
	}
}

// relTime formats a timestamp relative to now, e.g. "3m ago" / "in 2h".
func relTime(t time.Time) string {
	d := time.Since(t)
	future := d < 0
	if future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		s = fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}
