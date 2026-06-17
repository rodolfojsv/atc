package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

// openSchedules renders the scheduled-task list (read-only) into the
// shared viewport. Each task shows its cron, next fire, and recent run
// timeline — quiet "no-update" fires included, since they cost nothing.
func (m *Model) openSchedules() (tea.Model, tea.Cmd) {
	m.mode = modeSchedules
	m.layoutFocus()
	m.vp.SetContent(m.schedulesContent())
	m.vp.GotoTop()
	return m, nil
}

func (m *Model) updateSchedules(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "s":
		m.mode = modeBoard
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
	b.WriteString(keybar("↑↓/wheel", "scroll", "esc", "back"))
	if m.flash != "" {
		b.WriteString("  " + styleFlash.Render(m.flash))
	}
	return b.String()
}

func (m *Model) schedulesContent() string {
	scheds := m.sup.Schedules()
	if len(scheds) == 0 {
		return styleDim.Render(`  no schedules configured — add them under "schedules" in config.json`)
	}
	var b strings.Builder
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
		b.WriteString(styleSection.Render("  "+s.Name) + styleDim.Render("  "+s.Cron) + mode + precheck + "\n")
		b.WriteString(styleDim.Render(fmt.Sprintf("    %s · %s", since, next)) + "\n")
		if len(s.Runs) == 0 {
			b.WriteString(styleDim.Render("    no runs recorded yet") + "\n")
		}
		for _, r := range s.Runs {
			b.WriteString("    " + runLine(r) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
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
