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

// handlePrompt resolves a detected permission box via OnPermission and submits
// the answer, then waits for the box to clear. Only permission boxes reach
// here; questions are left to render as transcript text and be answered in chat
// (see watch), so they don't get re-asked through an interactive picker.
func (s *session) handlePrompt(ctx context.Context, p promptInfo) {
	s.answerPermission(ctx, p)
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

// selectMatch selects the first option matching any marker, or the fallback
// slice index if none match.
func (s *session) selectMatch(ctx context.Context, p promptInfo, markers []string, fallback int) {
	i := indexMatching(p.options, markers)
	if i < 0 {
		i = fallback
	}
	s.selectIndex(ctx, i)
}

// selectIndex moves the highlight to option position target and confirms.
// Arrow navigation is used rather than number keys because it doesn't depend
// on the TUI binding digit shortcuts. The starting row is read from the live
// pane rather than assumed to be the top: dialogs like the resume prompt
// default their cursor to a non-first option, so navigating relative to where
// the cursor actually is keeps the selection correct.
func (s *session) selectIndex(ctx context.Context, target int) {
	name := s.tmuxName()
	cur := 0
	if pane, err := s.tm.Capture(ctx, name, tmux.CaptureOpts{}); err == nil {
		if c := cursorOptionIndex(pane); c >= 0 {
			cur = c
		}
	}
	for cur < target {
		_ = s.tm.SendKeys(ctx, name, "Down")
		cur++
	}
	for cur > target {
		_ = s.tm.SendKeys(ctx, name, "Up")
		cur--
	}
	_ = s.tm.SendEnter(ctx, name)
}

// cursorOptionIndex returns the 0-based position of the currently highlighted
// option in the menu on screen (the option line carrying the selection cursor
// glyph), or -1 if no cursor is visible.
func cursorOptionIndex(pane string) int {
	idx := -1
	for _, ln := range strings.Split(pane, "\n") {
		if m := promptOptionRe.FindStringSubmatch(ln); m != nil {
			idx++
			if m[1] != "" {
				return idx
			}
		}
	}
	return -1
}

// answerDialogDirect answers a select dialog that is already on screen with the
// user's text — used when a prompt arrives for a dialog whose in-memory
// question didn't survive an atc restart (so it never routed through
// OnQuestion). A bare option number or matching option text selects that row;
// anything else is typed as a freeform answer.
func (s *session) answerDialogDirect(ctx context.Context, p promptInfo, answer string) {
	if i := optionIndexFor(answer, p.options); i >= 0 {
		s.selectIndex(ctx, i)
		return
	}
	name := s.tmuxName()
	_ = s.tm.SendText(ctx, name, answer)
	_ = s.tm.SendEnter(ctx, name)
}

// optionIndexFor maps a freeform answer to an option index: a bare 1-based
// number selects that option; otherwise the first option whose label contains
// the answer. Returns -1 when nothing matches.
func optionIndexFor(answer string, options []promptOption) int {
	a := strings.TrimSpace(answer)
	if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(options) {
		return n - 1
	}
	return indexMatching(options, []string{a})
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
