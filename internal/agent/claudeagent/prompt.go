package claudeagent

// Interactive prompt handling: detect the TUI's permission boxes and
// AskUserQuestion pickers from a captured pane, route them to atc's existing
// OnPermission/OnQuestion callbacks (the same ones Copilot uses), and drive the
// answer back in with keystrokes.
//
// Everything in the "Tunables" block below is version-specific to the Claude
// Code TUI and is the first place to adjust if detection or selection misfires
// against a live `tmux capture-pane`.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/tmux"
)

// --- Tunables -------------------------------------------------------------

// promptOptionRe matches one selectable option line, capturing an optional
// leading selection cursor, the number, and the label — e.g. "❯ 1. Yes" or
// "  2. No, and tell Claude what to do differently".
//
// The cursor must be on the option line itself: `❯` is also the input-prompt
// cursor (always on screen), so its mere presence anywhere is not a signal.
var promptOptionRe = regexp.MustCompile(`^\s*([❯►])?\s*([0-9]+)[.)]\s+(.*\S)\s*$`)

// permissionTitleMarkers / permissionOptionMarkers classify a box as a
// permission/approval prompt (→ OnPermission) rather than a question
// (→ OnQuestion).
var permissionTitleMarkers = []string{"Do you want", "wants to", "proceed?"}
var permissionOptionMarkers = []string{"don't ask again", "tell Claude", "Yes, and", "No, and"}

// Option-text fragments used to pick the right choice for a decision.
var (
	yesMarkers     = []string{"Yes"}
	alwaysMarkers  = []string{"don't ask", "always"}
	denyTalkMarker = []string{"tell Claude", "No, and"}
	noMarkers      = []string{"No"}
)

// promptSettle bounds how long we wait for a box to disappear after answering,
// so we don't re-fire the same prompt on the next poll.
const promptSettle = 5 * time.Second

// --- Detection ------------------------------------------------------------

type promptOption struct {
	label string
}

type promptInfo struct {
	kind    string // "permission" | "question"
	title   string
	options []promptOption
}

// detectPrompt parses a captured pane into a prompt, if one is showing. It
// requires at least two numbered options plus either a selection cursor glyph
// or permission-style wording, so ordinary numbered prose in an answer is not
// mistaken for an interactive box.
func detectPrompt(pane string) (promptInfo, bool) {
	lines := strings.Split(pane, "\n")
	var options []promptOption
	firstOpt := -1
	cursorOnOption := false
	for i, ln := range lines {
		if m := promptOptionRe.FindStringSubmatch(ln); m != nil {
			if firstOpt < 0 {
				firstOpt = i
			}
			if m[1] != "" {
				cursorOnOption = true
			}
			options = append(options, promptOption{label: strings.TrimSpace(m[3])})
		}
	}
	if len(options) < 2 || firstOpt < 0 {
		return promptInfo{}, false
	}

	title := ""
	for i := firstOpt - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			title = t
			break
		}
	}

	isPermission := containsAny(title, permissionTitleMarkers)
	for _, o := range options {
		if containsAny(o.label, permissionOptionMarkers) {
			isPermission = true
		}
	}

	// Gate against false positives from numbered prose: a real select box has
	// the cursor on an option line, or uses permission wording.
	if !cursorOnOption && !isPermission {
		return promptInfo{}, false
	}

	kind := "question"
	if isPermission {
		kind = "permission"
	}
	return promptInfo{kind: kind, title: title, options: options}, true
}

// --- Answering ------------------------------------------------------------

// handlePrompt resolves a detected prompt via the right callback and submits
// the answer, then waits for the box to clear.
func (s *session) handlePrompt(ctx context.Context, p promptInfo) {
	switch p.kind {
	case "permission":
		s.answerPermission(ctx, p)
	default:
		s.answerQuestion(ctx, p)
	}
	s.waitPromptCleared(ctx)
}

func (s *session) answerPermission(ctx context.Context, p promptInfo) {
	name := s.tmuxName()
	req := agent.PermissionRequest{
		Kind:    "other",
		Command: p.title,
		Summary: agent.Truncate(p.title, 80),
		Detail:  promptDetail(p),
	}
	decision, feedback := agent.Deny, ""
	if s.spec.OnPermission != nil {
		decision, feedback = s.spec.OnPermission(req)
	}
	switch decision {
	case agent.ApproveOnce:
		s.selectMatch(ctx, p, yesMarkers, 0)
	case agent.ApproveSession:
		if i := indexMatching(p.options, alwaysMarkers); i >= 0 {
			s.selectIndex(ctx, i)
		} else {
			s.selectMatch(ctx, p, yesMarkers, 0)
		}
	case agent.Cancel:
		_ = s.tm.SendKeys(ctx, name, "Escape")
	default: // Deny
		if feedback != "" {
			if i := indexMatching(p.options, denyTalkMarker); i >= 0 {
				s.selectIndex(ctx, i)
				_ = s.tm.SendText(ctx, name, feedback)
				_ = s.tm.SendEnter(ctx, name)
				return
			}
		}
		if i := indexMatching(p.options, noMarkers); i >= 0 {
			s.selectIndex(ctx, i)
		} else {
			_ = s.tm.SendKeys(ctx, name, "Escape")
		}
	}
}

func (s *session) answerQuestion(ctx context.Context, p promptInfo) {
	name := s.tmuxName()
	q := agent.Question{Prompt: p.title, AllowFreeform: true}
	for _, o := range p.options {
		q.Options = append(q.Options, o.label)
	}
	answer, ok := "", false
	if s.spec.OnQuestion != nil {
		answer, ok = s.spec.OnQuestion(q)
	}
	if !ok {
		_ = s.tm.SendKeys(ctx, name, "Escape")
		return
	}
	if i := indexMatching(p.options, []string{answer}); i >= 0 {
		s.selectIndex(ctx, i)
		return
	}
	// No matching option: type the answer as freeform.
	_ = s.tm.SendText(ctx, name, answer)
	_ = s.tm.SendEnter(ctx, name)
}

// selectMatch selects the first option matching any marker, or the fallback
// slice index if none match.
func (s *session) selectMatch(ctx context.Context, p promptInfo, markers []string, fallback int) {
	i := indexMatching(p.options, markers)
	if i < 0 {
		i = fallback
	}
	s.selectIndex(ctx, i)
}

// selectIndex moves the highlight to slice position i (assuming it starts at
// the top) and confirms. Arrow navigation is used rather than number keys
// because it doesn't depend on the TUI binding digit shortcuts.
func (s *session) selectIndex(ctx context.Context, i int) {
	name := s.tmuxName()
	for k := 0; k < i; k++ {
		_ = s.tm.SendKeys(ctx, name, "Down")
	}
	_ = s.tm.SendEnter(ctx, name)
}

// waitPromptCleared polls until the box is gone (or a deadline), so the watch
// loop doesn't answer the same prompt twice.
func (s *session) waitPromptCleared(ctx context.Context) {
	deadline := time.Now().Add(promptSettle)
	for time.Now().Before(deadline) {
		pane, err := s.tm.Capture(ctx, s.tmuxName(), tmux.CaptureOpts{})
		if err != nil {
			return
		}
		if _, ok := detectPrompt(pane); !ok {
			return
		}
		time.Sleep(pollInterval)
	}
}

// indexMatching returns the slice index of the first option whose label
// contains any needle (case-insensitive), or -1.
func indexMatching(options []promptOption, needles []string) int {
	for i, o := range options {
		l := strings.ToLower(o.label)
		for _, n := range needles {
			if n != "" && strings.Contains(l, strings.ToLower(n)) {
				return i
			}
		}
	}
	return -1
}

// promptDetail renders a prompt as lines for the approval modal.
func promptDetail(p promptInfo) []string {
	out := []string{p.title}
	for i, o := range p.options {
		out = append(out, strconv.Itoa(i+1)+". "+o.label)
	}
	return out
}
