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

// --- Capture normalisation -------------------------------------------------

// Claude Code 2.1.x renders an AskUserQuestion whose options carry `preview`
// content in a side-by-side layout: a vertical option list on the left, the
// focused option's preview in a box drawn to the right, and — crucially — the
// selected row marked by ANSI colour (bold/bright) instead of a ❯ glyph. A plain
// `capture-pane` strips the colour, so the row looks cursor-less and detectPrompt
// (which gates a question on a visible cursor) never recognises the box, leaving
// the session wedged in "working" with the picker silently waiting.
//
// normalizePromptPane takes an escape-preserving capture (-e) and rebuilds it as
// plain text the existing detectors understand: it scrapes off the right-hand
// preview panel so it can't pollute labels, and synthesises a ❯ on the
// colour-highlighted option row so detectPrompt/cursorOptionIndex see the cursor
// they look for. Ordinary ❯-glyph pickers and permission boxes pass through
// unchanged.

// csiRe matches an ANSI CSI escape (the SGR colour/style codes capture-pane -e
// emits); stripped to recover the plain text the detectors parse.
var csiRe = regexp.MustCompile("\x1b\\[[0-9;:?]*[ -/]*[@-~]")

// sgrParamsRe captures the parameter list of an SGR escape (…m). A standalone 1
// (bold) or 7 (reverse) is how the preview-layout picker marks its selected row;
// a colour such as "38;5;153" never contains a bare 1 or 7, so it can't trip it.
var sgrParamsRe = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// optNumPrefixRe matches an option row's leading "  1." so a synthetic cursor can
// be inserted just before the number.
var optNumPrefixRe = regexp.MustCompile(`^(\s*)([0-9]+[.)])`)

// panelBorderRunes are the vertical/corner box-drawing characters that frame the
// preview panel Claude draws to the right of an option. (Horizontal runs are
// excluded: a panel row always starts with a corner, and a full-width separator
// rule must not be mistaken for a panel.)
var panelBorderRunes = map[rune]bool{
	'│': true, '┃': true, '║': true,
	'┌': true, '┐': true, '└': true, '┘': true,
	'├': true, '┤': true, '╭': true, '╮': true, '╰': true, '╯': true,
}

// lineHasSelectSGR reports whether an escaped line turns on bold or reverse video
// — the preview-layout picker's highlight for the selected option.
func lineHasSelectSGR(escLine string) bool {
	for _, m := range sgrParamsRe.FindAllStringSubmatch(escLine, -1) {
		for _, p := range strings.Split(m[1], ";") {
			if p == "1" || p == "7" {
				return true
			}
		}
	}
	return false
}

// stripSidePanel drops the preview panel from a plain line: everything from the
// first panel border that follows real text rightward. A border at the left
// margin (only whitespace before it) is a box frame, not a side panel, so it is
// left for stripBoxGlyphs to handle.
func stripSidePanel(plain string) string {
	seenText := false
	for i, r := range plain {
		switch {
		case panelBorderRunes[r]:
			if seenText {
				return strings.TrimRight(plain[:i], " ")
			}
		case r != ' ':
			seenText = true
		}
	}
	return plain
}

// normalizePromptPane converts an escape-preserving capture into plain text with
// the preview panel removed and a synthetic ❯ injected on any colour-highlighted
// option row (see the block comment above).
func normalizePromptPane(escPane string) string {
	lines := strings.Split(escPane, "\n")
	out := make([]string, len(lines))
	for i, esc := range lines {
		plain := stripSidePanel(csiRe.ReplaceAllString(esc, ""))
		if lineHasSelectSGR(esc) {
			if m := promptOptionRe.FindStringSubmatch(plain); m != nil && m[1] == "" {
				plain = optNumPrefixRe.ReplaceAllString(plain, "$1❯ $2")
			}
		}
		out[i] = plain
	}
	return strings.Join(out, "\n")
}

// --- Detection ------------------------------------------------------------

type promptOption struct {
	label  string
	detail string // the option's description (the indented line(s) below it), if any
}

type promptInfo struct {
	kind        string // "permission" | "question"
	title       string
	prose       string // for questions: the assistant's lead-in text scraped from above the box (often not yet in the JSONL — see emitQuestionProse)
	options     []promptOption
	multiSelect bool // a checkbox picker (toggle each choice with Space, submit with Enter)
}

// A multi-select AskUserQuestion is a checkbox frame: toggle each choice with
// Space, submit the set with Enter — versus single-select, where Enter picks the
// highlighted row. Two signals classify it, both chosen to avoid a false
// positive from the multi-*question* tab bar (which also draws ☐/✔ for its
// unanswered/submit tabs, but never on a numbered option line):
//   - a checkbox glyph leading an actual option label ("1. ☐ Serif"), detected
//     per-option so the tab bar can't trip it;
//   - the "Space to …" toggle hint, which the single-select frame never shows.
//
// optionCheckboxGlyphs are the box states that mark such an option;
// checkboxGlyphs additionally covers ✔/✓ for stripping a box off a label.
var multiSelectHintMarkers = []string{"Space to", "space to", "SPACE to"}
var optionCheckboxGlyphs = []string{"☐", "☒", "◻", "◼", "▢", "[ ]", "[x]", "[X]"}
var checkboxGlyphs = append([]string{"✔", "✓"}, optionCheckboxGlyphs...)

// pickerChromeMarkers are strings that only Claude's interactive select boxes
// (permission prompts, AskUserQuestion pickers, the session-start menu) render —
// the navigation hint line and the tab-state glyphs. Requiring one of these to
// classify a box as a question is what stops ordinary assistant prose that
// happens to contain "❯ 1. …" lines (e.g. a TUI example pasted into a reply)
// from being mistaken for a live picker.
var pickerChromeMarkers = []string{"Enter to select", "to navigate", "Esc to cancel", "☐", "☒", "✔"}

// submitMarkers identify the picker's final "Review your answers" tab. The
// multi-question form (one question per tab) ends on a "Ready to submit your
// answers?" confirmation; we answer it automatically rather than surfacing a
// bogus extra question, since every real question has already been answered to
// reach it.
var submitMarkers = []string{"Submit answers"}

// metaOptionMarkers are the escape-hatch choices Claude Code appends to every
// AskUserQuestion picker. They are not real answers: "Type something" declines
// the structured question and drops to a free-text reply, and "Chat about this"
// bails out to open chat. We hide them from the options surfaced to the user
// (atc already offers a freeform reply box) and instead route a typed answer
// through "Type something" itself.
var metaOptionMarkers = []string{"Type something", "Chat about this"}
var typeSomethingMarkers = []string{"Type something"}

// detectPrompt parses a captured pane into a prompt, if one is showing. It
// requires at least two numbered options plus, for a question, the picker's
// chrome (a real select box, not numbered prose) — or, for an approval,
// permission-style wording.
//
// Claude's multi-question AskUserQuestion renders as a *tabbed* form: one
// question visible per pane, the rest behind tabs, each option carrying a
// wrapping description line below it (which has no number, so the option scan
// skips it). Pressing Enter selects the highlighted option and auto-advances to
// the next tab, so a single promptInfo per capture — the visible question — is
// all a caller needs; the watcher re-fires for each tab as the form advances.
func detectPrompt(pane string) (promptInfo, bool) {
	lines := strings.Split(pane, "\n")
	var options []promptOption
	var optLines []int // line index of each option, parallel to options
	firstOpt := -1
	cursorOnOption := false
	optHadCheckbox := false // an option line carried a ☐/☒ box (multi-select)
	for i, ln := range lines {
		if m := promptOptionRe.FindStringSubmatch(ln); m != nil {
			if firstOpt < 0 {
				firstOpt = i
			}
			if m[1] != "" {
				cursorOnOption = true
			}
			raw := strings.TrimSpace(m[3])
			for _, g := range optionCheckboxGlyphs {
				if strings.HasPrefix(raw, g) {
					optHadCheckbox = true
					break
				}
			}
			options = append(options, promptOption{label: stripCheckbox(raw)})
			optLines = append(optLines, i)
		}
	}
	if len(options) < 2 || firstOpt < 0 {
		return promptInfo{}, false
	}

	// Each option's description is the indented line(s) between it and the next
	// option (Claude renders one under each choice). Scraped here so the UI can
	// show the context Claude wrote, not just the bare label.
	for k := range options {
		end := len(lines)
		if k+1 < len(optLines) {
			end = optLines[k+1]
		}
		options[k].detail = detailBelow(lines, optLines[k]+1, end)
	}

	// The question text is the first non-empty line above the options, skipping
	// the blank gap the picker leaves between them. Box glyphs (from a framed
	// variant) are stripped so the title isn't polluted with border pieces.
	title := ""
	titleIdx := -1
	for i := firstOpt - 1; i >= 0; i-- {
		if t := strings.TrimSpace(stripBoxGlyphs(lines[i])); t != "" {
			title = t
			titleIdx = i
			break
		}
	}

	isPermission := containsAny(title, permissionTitleMarkers)
	for _, o := range options {
		if containsAny(o.label, permissionOptionMarkers) {
			isPermission = true
		}
	}

	// Gate against false positives from numbered prose: an approval is matched by
	// its wording; any other box must carry both a selection cursor and the
	// picker's own chrome to count.
	if !isPermission && !(cursorOnOption && containsAny(pane, pickerChromeMarkers)) {
		return promptInfo{}, false
	}

	kind := "question"
	if isPermission {
		kind = "permission"
	}
	// A multi-select question is a checkbox frame; permissions are always
	// single-select, so only a question can carry the multi flag. Signalled by a
	// box glyph on an option line, or the Space-toggle hint.
	multi := !isPermission && (optHadCheckbox || containsAny(pane, multiSelectHintMarkers))
	prose := ""
	if !isPermission && titleIdx >= 0 {
		prose = proseAbove(lines, titleIdx)
	}
	return promptInfo{kind: kind, title: title, prose: prose, options: options, multiSelect: multi}, true
}

// proseScanLimit bounds how far above a question's title we look for its lead-in
// prose, so the scan can't wander up into an earlier turn's output.
const proseScanLimit = 12

// proseAbove returns the assistant's explanatory text rendered immediately above
// a question box — the prose Claude writes before AskUserQuestion, which is often
// not yet in the on-disk JSONL when the picker appears (see emitQuestionProse).
// It walks up from the title past the box border, the blank gap, and the
// multi-question tab bar, then gathers the contiguous block of plain text,
// stopping at the next blank line, option, or picker chrome so it can't bleed
// into the previous turn. Lines join with spaces (the pane hard-wraps a
// paragraph at the pane width). Returns "" when there's no lead-in text.
func proseAbove(lines []string, titleIdx int) string {
	isBoundary := func(line string) bool {
		return promptOptionRe.MatchString(line) ||
			containsAny(line, pickerChromeMarkers) ||
			containsAny(line, multiSelectHintMarkers)
	}
	lo := titleIdx - proseScanLimit
	if lo < 0 {
		lo = 0
	}
	// Walk up past the gap/border/tab-bar to the bottom line of the prose block.
	i := titleIdx - 1
	for i >= lo {
		if t := strings.TrimSpace(stripBoxGlyphs(lines[i])); t != "" && !isBoundary(lines[i]) {
			break
		}
		i--
	}
	// Collect the contiguous prose block, top-to-bottom.
	var rev []string
	for ; i >= lo; i-- {
		if isBoundary(lines[i]) {
			break
		}
		t := strings.TrimSpace(stripBoxGlyphs(lines[i]))
		if t == "" {
			break
		}
		rev = append(rev, stripLeadBullet(t))
	}
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	return strings.TrimSpace(strings.Join(rev, " "))
}

// stripLeadBullet drops Claude Code's leading message bullet so the scraped prose
// matches the JSONL text (which has no bullet) and reads cleanly.
func stripLeadBullet(s string) string {
	for _, g := range []string{"⏺ ", "● ", "• ", "◦ ", "▪ ", "‣ "} {
		if strings.HasPrefix(s, g) {
			return strings.TrimSpace(s[len(g):])
		}
	}
	return s
}

// proseMatchMin is the shortest normalized prefix overlap we trust as "the same
// prose" — long enough that an incidental shared opening can't trigger a false
// dedup.
const proseMatchMin = 24

// normalizeProse canonicalizes prose for comparison: strip box glyphs and the
// message bullet, then collapse all whitespace runs to single spaces. The pane
// hard-wraps while the JSONL does not, so only a whitespace-insensitive compare
// can match the two.
func normalizeProse(s string) string {
	s = stripLeadBullet(strings.TrimSpace(stripBoxGlyphs(s)))
	return strings.Join(strings.Fields(s), " ")
}

// proseMatches reports whether two already-normalized prose strings are the same
// text. The pane copy can be shorter than the JSONL copy (wrapping cut off the
// bottom, or it scrolled), so a prefix overlap of at least proseMatchMin counts.
func proseMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= proseMatchMin && strings.HasPrefix(b, a)
}

// questionSig is a stable identity for a question box — its title plus the
// labels of every option — used to tell whether a freshly captured picker is
// the same box already surfaced (suppress) or a different/advanced one (surface).
func questionSig(p promptInfo) string {
	parts := make([]string, 0, len(p.options)+1)
	parts = append(parts, p.title)
	for _, o := range p.options {
		parts = append(parts, o.label)
	}
	return strings.Join(parts, "\x1f")
}

// stripCheckbox removes a leading checkbox glyph (and the space after it) from
// a scraped option label, so a multi-select row like "1. ☐ Serif" yields the
// bare "Serif" rather than a label polluted with the box state.
func stripCheckbox(label string) string {
	s := strings.TrimSpace(label)
	for _, g := range checkboxGlyphs {
		if strings.HasPrefix(s, g) {
			return strings.TrimSpace(s[len(g):])
		}
	}
	return s
}

// answerOptions are the labels (and their descriptions) surfaced to the user:
// the picker's options minus Claude Code's escape-hatch meta-options ("Type
// something" / "Chat about this"), which decline rather than answer. The two
// slices are parallel.
func (p promptInfo) answerOptions() (labels, details []string) {
	for _, o := range p.options {
		if containsAny(o.label, metaOptionMarkers) {
			continue
		}
		labels = append(labels, o.label)
		details = append(details, o.detail)
	}
	return labels, details
}

// isSubmitConfirm reports whether the box is the multi-question form's final
// "Ready to submit your answers?" tab, which we answer automatically.
func (p promptInfo) isSubmitConfirm() bool {
	for _, o := range p.options {
		if containsAny(o.label, submitMarkers) {
			return true
		}
	}
	return false
}

// detailBelow joins the description lines that sit under an option — the
// contiguous indented run from start up to end (the next option, or the box
// bottom). A blank line, a horizontal rule (which strips to empty), or the
// picker's chrome ends the run, so the hint bar and separators never leak in.
func detailBelow(lines []string, start, end int) string {
	var parts []string
	for i := start; i < end && i < len(lines); i++ {
		t := strings.TrimSpace(stripBoxGlyphs(lines[i]))
		if t == "" || containsAny(lines[i], pickerChromeMarkers) {
			break
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// stripBoxGlyphs removes the picker's frame characters so a scraped title isn't
// polluted with border pieces from `tmux capture-pane`.
func stripBoxGlyphs(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '│', '┃', '╭', '╮', '╰', '╯', '─', '━', '┌', '┐', '└', '┘', '├', '┤':
			return -1
		}
		return r
	}, s)
}

// capturePrompt reads the pane with ANSI escapes preserved and normalises it for
// prompt detection — see normalizePromptPane. Every site that feeds detectPrompt
// or cursorOptionIndex goes through here so colour-highlighted preview pickers are
// recognised and navigated like ordinary ones.
func (s *session) capturePrompt(ctx context.Context, name string) (string, error) {
	pane, err := s.tm.Capture(ctx, name, tmux.CaptureOpts{Escapes: true})
	if err != nil {
		return "", err
	}
	return normalizePromptPane(pane), nil
}

// --- Answering ------------------------------------------------------------

// handlePrompt resolves a detected permission box via OnPermission and submits
// the answer, then waits for the box to clear. It runs on its own goroutine (the
// watcher keeps tailing meanwhile); the watcher sets the answering guard so only
// one runs per box, and this clears it when the box is gone — mirroring
// handleQuestion. Questions take the separate handleQuestion path.
func (s *session) handlePrompt(ctx context.Context, p promptInfo) {
	defer func() {
		s.mu.Lock()
		s.answering = false
		s.mu.Unlock()
	}()
	s.answerPermission(ctx, p)
	s.waitPromptCleared(ctx)
}

// handleQuestion surfaces an AskUserQuestion box through OnQuestion — which
// frames it for the user and marks the session "waiting" — blocks until they
// reply, then drives that reply into the on-screen picker and waits for it to
// clear. It runs on its own goroutine (the watcher keeps tailing meanwhile) and
// the caller sets the questioning guard so only one runs per box; this clears
// it when the box is gone.
//
// cancel is closed by the watcher when the picker vanishes off-screen while we
// were still waiting on the user (they cleared it by hand in tmux, or Claude
// withdrew it). OnQuestion then returns ok=false; we don't press Escape unless a
// box is actually still up, since Escape into a live turn would interrupt it.
//
// The reply itself arrives via a normal chat message: while a question is
// pending the supervisor routes the user's next message to answerQuestion
// (feeding OnQuestion's channel) instead of starting a new turn, so the answer
// returned here is what to select.
func (s *session) handleQuestion(ctx context.Context, p promptInfo, cancel chan struct{}) {
	defer func() {
		s.mu.Lock()
		s.questioning = false
		s.questionCancel = nil
		s.mu.Unlock()
	}()

	name := s.tmuxName()

	// The multi-question form's final tab is a "Ready to submit your answers?"
	// confirmation. Every real question was already answered to reach it, so
	// submit it automatically instead of surfacing a bogus extra question.
	if p.isSubmitConfirm() {
		tracef("question submit id=%s", s.id)
		s.withPane(func() { s.selectMatch(ctx, p, submitMarkers, 0) })
		s.waitPromptCleared(ctx)
		return
	}

	// Surface the one question visible on this tab — minus Claude Code's
	// escape-hatch meta-options, which decline rather than answer. Selecting a
	// real answer makes the picker auto-advance to the next tab; the watcher
	// re-fires and we frame that next question on its own — so multi-question
	// asks walk one at a time, each with its own title and options.
	labels, details := p.answerOptions()
	// Prefer the full descriptions from the transcript (the pane truncates long
	// ones); keep the scraped detail as a fallback when no match is found.
	if tm := s.latestQuestionDetails(); len(tm) > 0 {
		for i := range labels {
			if d := matchDetail(tm, labels[i]); d != "" {
				details[i] = d
			}
		}
	}
	q := agent.Question{Prompt: p.title, Options: labels, OptionDetails: details, AllowFreeform: true, MultiSelect: p.multiSelect}
	tracef("question id=%s title=%q opts=%d multi=%t", s.id, p.title, len(q.Options), p.multiSelect)
	answer, ok := s.spec.OnQuestion(q, cancel)

	if !ok {
		// Withdrawn or aborted. Only dismiss a box that's still up; if it
		// already vanished (the user cleared it by hand, or Claude moved on)
		// pressing Escape would interrupt the now-live turn.
		if pane, err := s.capturePrompt(ctx, name); err == nil {
			if _, up := detectPrompt(pane); up {
				s.withPane(func() { _ = s.tm.SendKeys(ctx, name, "Escape") })
				s.waitPromptCleared(ctx)
			}
		}
		return
	}
	// Re-capture: the user may have taken a while, so drive the answer into the
	// box as it stands now rather than the snapshot we detected it from.
	cur := p
	if pane, err := s.capturePrompt(ctx, name); err == nil {
		if fresh, ok := detectPrompt(pane); ok {
			cur = fresh
		}
	}
	// Drive the whole selection as one atomic pane sequence so navigation keys,
	// toggles and any freeform text can't interleave with another goroutine's.
	s.withPane(func() {
		if cur.multiSelect {
			// Checkbox frame: toggle every chosen row with Space, then submit once
			// with Enter. A freeform reply that matches no option falls back to the
			// "Type something" hatch like the single-select path.
			if idxs := optionIndicesFor(answer, cur.options); len(idxs) > 0 {
				s.selectMulti(ctx, idxs)
			} else if ti := indexMatching(cur.options, typeSomethingMarkers); ti >= 0 {
				s.selectIndex(ctx, ti)
				s.typeFreeform(ctx, answer)
			} else {
				_ = s.tm.SendText(ctx, name, answer)
				_ = s.tm.SendEnter(ctx, name)
			}
		} else if i := optionIndexFor(answer, cur.options); i >= 0 {
			// A real option (matched against the full on-screen list, so the index
			// lines up even though we hid the meta-options from the user).
			s.selectIndex(ctx, i)
		} else if ti := indexMatching(cur.options, typeSomethingMarkers); ti >= 0 {
			// A freeform answer: take the picker's "Type something" hatch, which
			// opens a free-text reply, then type the answer into it.
			s.selectIndex(ctx, ti)
			s.typeFreeform(ctx, answer)
		} else {
			_ = s.tm.SendText(ctx, name, answer)
			_ = s.tm.SendEnter(ctx, name)
		}
	})
	s.waitQuestionAdvanced(ctx, cur.title)
}

// typeFreeform types a free-text answer after the picker's "Type something"
// option has opened the reply input, giving the box a moment to switch into
// text-entry mode first. Caller must hold the pane lock (run in withPane).
func (s *session) typeFreeform(ctx context.Context, answer string) {
	name := s.tmuxName()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pane, err := s.capturePrompt(ctx, name); err == nil {
			if _, ok := detectPrompt(pane); !ok {
				break // the select box is gone; the text input is ready
			}
		}
		time.Sleep(pollInterval)
	}
	_ = s.tm.SendText(ctx, name, answer)
	_ = s.tm.SendEnter(ctx, name)
}

// waitQuestionAdvanced blocks until the box clears or the visible question
// changes from prev. Selecting an option advances the tabbed form to the next
// question without clearing the box, so we can't wait for a full clear; watching
// the title change lets the next tab surface promptly instead of stalling out
// the settle timeout on every question.
func (s *session) waitQuestionAdvanced(ctx context.Context, prev string) {
	deadline := time.Now().Add(promptSettle)
	for time.Now().Before(deadline) {
		pane, err := s.capturePrompt(ctx, s.tmuxName())
		if err != nil {
			return
		}
		p, ok := detectPrompt(pane)
		if !ok || p.title != prev {
			return
		}
		time.Sleep(pollInterval)
	}
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
	// Drive the choice as one atomic pane sequence (OnPermission above may have
	// blocked on the user for a while, so it stays outside the lock).
	s.withPane(func() {
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
	})
}

// selectMatch selects the first option matching any marker, or the fallback
// slice index if none match. Caller must hold the pane lock (run in withPane).
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
// the cursor actually is keeps the selection correct. Caller must hold the pane
// lock (run in withPane) so the cursor read and the navigation stay atomic.
func (s *session) selectIndex(ctx context.Context, target int) {
	name := s.tmuxName()
	cur := 0
	if pane, err := s.capturePrompt(ctx, name); err == nil {
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

// selectMulti toggles each target row of a multi-select checkbox picker with
// Space, then submits the whole set with one Enter. Rows are visited in the
// given order, navigating relative to the live cursor like selectIndex; toggles
// assume every box starts unchecked (Claude renders a fresh picker that way).
// Caller must hold the pane lock (run in withPane).
func (s *session) selectMulti(ctx context.Context, targets []int) {
	if len(targets) == 0 {
		return
	}
	name := s.tmuxName()
	cur := 0
	if pane, err := s.capturePrompt(ctx, name); err == nil {
		if c := cursorOptionIndex(pane); c >= 0 {
			cur = c
		}
	}
	for _, t := range targets {
		for cur < t {
			_ = s.tm.SendKeys(ctx, name, "Down")
			cur++
		}
		for cur > t {
			_ = s.tm.SendKeys(ctx, name, "Up")
			cur--
		}
		_ = s.tm.SendKeys(ctx, name, "Space")
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
	s.withPane(func() {
		if i := optionIndexFor(answer, p.options); i >= 0 {
			s.selectIndex(ctx, i)
			return
		}
		name := s.tmuxName()
		_ = s.tm.SendText(ctx, name, answer)
		_ = s.tm.SendEnter(ctx, name)
	})
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

// optionIndicesFor maps a multi-select answer to the option indices it names.
// The whole answer is tried as one option first (so a single chosen label that
// contains a comma still resolves); failing that it is split on newlines and
// commas — the web picker joins chosen labels with newlines, a typed reply may
// use either — and each piece resolved on its own. Duplicates drop, order kept.
func optionIndicesFor(answer string, options []promptOption) []int {
	if i := optionIndexFor(answer, options); i >= 0 {
		return []int{i}
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.FieldsFunc(answer, func(r rune) bool { return r == '\n' || r == ',' }) {
		if i := optionIndexFor(part, options); i >= 0 && !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

// waitPromptCleared polls until the box is gone (or a deadline), so the watch
// loop doesn't answer the same prompt twice.
func (s *session) waitPromptCleared(ctx context.Context) {
	deadline := time.Now().Add(promptSettle)
	for time.Now().Before(deadline) {
		pane, err := s.capturePrompt(ctx, s.tmuxName())
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
