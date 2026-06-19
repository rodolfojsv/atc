package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("25")).Padding(0, 1)
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	// styleSection labels a board group (Pinned / a category).
	styleSection = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
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

	styleInputBox        = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	styleInputBoxFocused = styleInputBox.BorderForeground(lipgloss.Color("75"))

	styleWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleWaiting = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styleDone    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	styleErrSt   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleIdle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
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

// humanCost picks the backend-appropriate cost figure: billed AI
// Credits for Copilot sessions, estimated dollars for Claude Code.
func humanCost(u supervisor.Usage) string {
	if u.NanoAiu > 0 {
		return humanAIC(u.NanoAiu) + " aic"
	}
	if u.CostUSD > 0 {
		if u.CostUSD < 10 {
			return fmt.Sprintf("$%.2f", u.CostUSD)
		}
		return fmt.Sprintf("$%.1f", u.CostUSD)
	}
	return "—"
}

// limitSummary renders the most-constrained account-usage window for a footer
// line, preferring absolute used/max (e.g. "15.5k/30.0k premium interactions")
// over a bare percentage when the cap is known. Returns "" when there is no
// reading yet. Shared by the board footer and the focus header.
func limitSummary(lm supervisor.Limits) string {
	if lm.AsOf.IsZero() || len(lm.Windows) == 0 {
		return ""
	}
	bind := lm.Windows[0]
	for _, w := range lm.Windows[1:] {
		if w.Pct > bind.Pct {
			bind = w
		}
	}
	amount := fmt.Sprintf("%.0f%%", bind.Pct)
	if bind.Max > 0 {
		amount = humanTokens(bind.Used) + "/" + humanTokens(bind.Max)
	}
	return amount + " " + bind.Label
}

// humanAIC formats accumulated nano-AIU as AI Credits. There is no
// fixed tokens→AIC rate (it depends on model multiplier and billing
// batches), so this shows what the runtime actually billed.
func humanAIC(nanoAiu float64) string {
	aic := nanoAiu / 1e9
	switch {
	case aic == 0:
		return "—"
	case aic < 0.01:
		return "<0.01"
	case aic < 10:
		return fmt.Sprintf("%.2f", aic)
	default:
		return fmt.Sprintf("%.1f", aic)
	}
}

// rightAlign joins left and right with padding so right lands at the
// terminal's right edge (the bottom-right corner slot).
func rightAlign(width int, left, right string) string {
	pad := width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if pad < 1 {
		return left
	}
	return left + strings.Repeat(" ", pad) + right
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
