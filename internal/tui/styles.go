package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("25")).Padding(0, 1)
	styleHeader  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	styleSel     = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	styleKeybar  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleKey     = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	styleFlash   = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleBanner  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124")).Bold(true).Padding(0, 1)
	styleModal   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2)
	styleAuto    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

	styleUser     = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	styleUserText = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))

	styleWorking  = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleWaiting  = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styleDone     = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	styleErrSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleIdle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func statusLabel(st supervisor.Status) string {
	switch st {
	case supervisor.StatusStarting:
		return styleIdle.Render("◌ starting")
	case supervisor.StatusIdle:
		return styleIdle.Render("· idle")
	case supervisor.StatusWorking:
		return styleWorking.Render("● working")
	case supervisor.StatusWaiting:
		return styleWaiting.Render("⚠ WAITING")
	case supervisor.StatusDone:
		return styleDone.Render("✓ done")
	case supervisor.StatusError:
		return styleErrSt.Render("✗ error")
	case supervisor.StatusClosed:
		return styleDim.Render("— closed")
	}
	return string(st)
}

// statusWidth is the visual width statusLabel occupies (labels above
// are all ≤ 10 cells; keep the column fixed).
const statusWidth = 10

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	case n == 0:
		return "—"
	default:
		return fmt.Sprintf("%d", n)
	}
}

func keybar(pairs ...string) string {
	out := ""
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out += styleKeybar.Render("  ")
		}
		out += styleKey.Render("["+pairs[i]+"]") + styleKeybar.Render(" "+pairs[i+1])
	}
	return out
}
