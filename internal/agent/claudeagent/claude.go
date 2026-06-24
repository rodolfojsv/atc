// Package claudeagent adapts the Claude Code CLI to atc's backend-neutral
// agent interface by driving the *interactive* `claude` TUI inside a detached
// tmux session.
//
// Why tmux instead of `claude -p`: as of the June 2026 billing change, the
// headless `-p`/stream-JSON path (and the Agent SDK and ACP) draw from a
// separate, capped "agent credit" pool billed at API rates, while Claude Code
// run interactively in a real terminal still draws from the user's
// subscription. tmux gives claude a genuine PTY (so it bills as interactive)
// and, being a daemon, keeps the session alive across atc restarts.
//
// Transport split:
//   - Input (prompts, model switch, interrupt) goes in via `tmux send-keys`.
//   - Output (assistant text, tool calls, usage/cost) is read from Claude's
//     own JSONL transcript (~/.claude/projects/<dir>/<id>.jsonl) — the same
//     file History() replays — so we reuse the proven parser instead of
//     scraping the TUI for content.
//   - Turn state (working/idle) is pushed by injected Claude Code lifecycle
//     hooks (UserPromptSubmit/Stop via --settings) into a per-session state
//     file the watcher reads — no polling. `tmux capture-pane` is used only
//     while a turn is live, to read and drive the interactive permission /
//     question pickers, and `pane_current_command` to detect a claude that
//     died inside a still-living tmux session (layer-2 recovery).
//
// Permission model: presets map onto Claude Code's --permission-mode at launch
// ("read-only" → plan, "allow-all" → bypassPermissions, otherwise acceptEdits),
// as before. Runtime per-tool prompts in the TUI are not yet answered
// programmatically; that is a follow-up.
package claudeagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/tmux"
)

// Optional diagnostics: when ATC_CLAUDE_TRACE names a writable file, the
// session's drive/observe loop appends a timestamped line for each key step
// (send, watch start, transcript drain, emit, idle, prompt, errors). Disabled
// (one env lookup) when unset. Mirrors copilotagent's ATC_COPILOT_TRACE.
var (
	traceOnce sync.Once
	traceFile *os.File
	traceMu   sync.Mutex
)

func tracef(format string, args ...any) {
	traceOnce.Do(func() {
		if p := strings.TrimSpace(os.Getenv("ATC_CLAUDE_TRACE")); p != "" {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				traceFile = f
			}
		}
	})
	if traceFile == nil {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	fmt.Fprintf(traceFile, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
}

// Tunables for driving and observing the TUI. These are the knobs most likely
// to need adjustment against a specific Claude Code version.
const (
	paneWidth    = 200
	paneHeight   = 50
	historyLimit = "50000" // tmux scrollback lines

	pollInterval = 300 * time.Millisecond  // active cadence: tail jsonl + capture pane while a turn runs
	idlePollMax  = 2 * time.Second         // backed-off cadence once a turn has gone quiet (saves tmux execs)
	idleRampStep = 300 * time.Millisecond  // how much each idle poll lengthens the interval toward idlePollMax
	quiescence   = 1500 * time.Millisecond // idle = no new transcript for this long while not "working"
	idDiscovery  = 8 * time.Second         // how long to wait for the session jsonl to appear
	readyTimeout = 30 * time.Second        // how long to wait for the TUI to accept input after launch
	readySettle  = 1500 * time.Millisecond // extra wait after ready chrome appears, so input is truly live
)

// readyMarkers are chrome the claude TUI shows once it is up and accepting
// input. We wait for one of these after launching before sending the first
// prompt — otherwise keystrokes typed during startup (config + MCP load) are
// dropped. Tunable against a live capture-pane.
var readyMarkers = []string{"shift+tab", "for shortcuts", "bypass permissions"}

// trustMarkers identify Claude Code's first-run "trust this folder?" dialog,
// which blocks all input until answered. atc launches claude in a working dir
// the user configured for the session, so we auto-accept it (the "Yes, I trust
// this folder" option is preselected; Enter confirms).
var trustMarkers = []string{"trust this folder", "Is this a project you created"}

func isTrustPrompt(pane string) bool { return containsAny(pane, trustMarkers) }

// workingMarkers are substrings the claude TUI shows while a turn is in
// progress. If none are present (and the transcript has gone quiet) the turn is
// considered finished. Claude Code's exact status text changes between
// versions — if turn-end is mis-detected, adjust these first.
var workingMarkers = []string{
	"esc to interrupt",
	"Esc to interrupt",
	"interrupt)",
}

// workingRe matches the busy spinner's elapsed-time counter, observed live as
// e.g. "✢ Noodling… (49s · ↓ 2.7k tokens)". The spinner word rotates
// (Noodling/Working/Forging/…), so we match the stable "(<n>s" counter rather
// than any one word.
var workingRe = regexp.MustCompile(`\(\d+s\b`)

// isWorking reports whether the pane shows a turn in progress.
func isWorking(pane string) bool {
	return workingRe.MatchString(pane) || containsAny(pane, workingMarkers)
}

type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Name() string { return "claude" }

func (b *Backend) Stop() error { return nil } // each session owns its tmux session

func (b *Backend) NewSession(_ context.Context, spec agent.SessionSpec) (agent.Session, error) {
	tm, err := requirements()
	if err != nil {
		return nil, err
	}
	return newSession(spec, tm, false), nil
}

func (b *Backend) ResumeSession(_ context.Context, spec agent.SessionSpec) (agent.Session, error) {
	tm, err := requirements()
	if err != nil {
		return nil, err
	}
	return newSession(spec, tm, true), nil
}

// Reattach implements agent.ResumeReady. On atc restart the claude TUI is
// still alive in its tmux session and may be mid-turn. We start the
// session-long watcher (so the in-progress turn streams and the eventual
// turn-end fires EventIdle) and report the live working state from the
// pane, so the supervisor restores working/done correctly instead of
// assuming the turn finished while atc was down.
func (s *session) Reattach(ctx context.Context) bool {
	s.startWatch()
	pane, err := s.tm.Capture(ctx, s.tmuxName(), tmux.CaptureOpts{})
	if err != nil {
		return false
	}
	return isWorking(pane)
}

// requirements verifies both CLIs are present and returns a tmux client.
func requirements() (*tmux.Client, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return nil, errors.New("the `claude` CLI was not found on PATH")
	}
	return tmux.New() // errors if tmux is missing
}

type session struct {
	mu sync.Mutex

	// paneMu serializes everything that types into the tmux pane: the Send
	// path, the watcher's permission/question answers, the background /usage
	// scrape, model switch, and abort. Those drivers run on several
	// goroutines at once, and without this lock their keystrokes interleave
	// into one pane and corrupt each other (a half-typed prompt, a stray
	// Escape into a live turn). It is held only around the actual key sends —
	// never across a wait for the user — so it can't stall the UI. Lock
	// order: paneMu may be taken before s.mu, never the reverse.
	paneMu sync.Mutex

	id       string // atc session id (uuid); also seeds claude --session-id and the tmux name
	claudeID string // id of the on-disk jsonl transcript; usually == id, re-discovered if needed
	spec     agent.SessionSpec
	tm       *tmux.Client

	started        bool          // claude has been launched at least once for this id (resume vs new)
	closed         bool          // Close was called; the watcher should stop
	watching       bool          // the session-long watcher goroutine is running
	questioning    bool          // a question box is being surfaced/answered (one handler at a time)
	answering      bool          // a permission box is being answered (one handler at a time)
	questionCancel chan struct{} // closed to withdraw the in-flight question when its picker vanishes
	offset         int64         // byte offset into the transcript already emitted (monotonic)
	wake           chan struct{} // nudges the watcher back to fast polling when a turn starts (buffered 1)

	lastUsageScrape time.Time // throttles the automatic /usage refresh

	lastMsg    string // normalized text of the last assistant message emitted; lets a question detect prose the JSONL already delivered
	shownProse string // normalized prose surfaced from the pane for the current question; suppresses its later (post-answer) JSONL flush
}

// newSession builds a session with its channels initialized.
func newSession(spec agent.SessionSpec, tm *tmux.Client, started bool) *session {
	id := spec.SessionID
	if id == "" {
		id = uuid.NewString()
	}
	return &session{id: id, claudeID: id, spec: spec, tm: tm, started: started, wake: make(chan struct{}, 1)}
}

func (s *session) ID() string { return s.id }

// tmuxName is the tmux session that hosts this conversation's claude process.
func (s *session) tmuxName() string { return "atc-" + s.id }

func (s *session) emit(e agent.Event) {
	if e.Type == agent.EventMessage {
		// Remember the last assistant message so a question surfacing right after
		// can tell whether its prose already reached the user from the JSONL.
		s.mu.Lock()
		s.lastMsg = normalizeProse(e.Text)
		s.mu.Unlock()
	}
	if s.spec.OnEvent != nil {
		tracef("emit id=%s type=%s textlen=%d", s.id, e.Type, len(e.Text))
		s.spec.OnEvent(e)
		return
	}
	tracef("emit DROPPED id=%s type=%s onEvent=nil", s.id, e.Type)
}

// emitDrained emits transcript events, dropping a message that merely repeats a
// question's prose already surfaced from the pane. A question turn's prose is
// often not flushed to the JSONL until the question is answered, so the watcher
// shows it from the pane up front (emitQuestionProse); this keeps the late
// JSONL copy from then printing it a second time.
func (s *session) emitDrained(evs []agent.Event) {
	for _, e := range evs {
		if e.Type == agent.EventMessage && s.suppressProse(e.Text) {
			tracef("drain suppress-prose id=%s (already shown from pane)", s.id)
			continue
		}
		s.emit(e)
	}
}

// suppressProse reports whether text is the prose we already surfaced from the
// pane for the current question (and clears the latch when it matches, so it
// fires at most once per question).
func (s *session) suppressProse(text string) bool {
	n := normalizeProse(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shownProse == "" || !proseMatches(n, s.shownProse) {
		return false
	}
	s.shownProse = ""
	return true
}

// emitQuestionProse surfaces a question's lead-in prose before the picker. The
// prose is a text block in the same assistant turn as the AskUserQuestion, which
// Claude Code frequently doesn't flush to the on-disk JSONL until the question is
// answered — so draining can't supply it in order, and it would otherwise land
// after the user has already answered. We take it from the pane instead, but only
// when the JSONL hasn't already delivered it this turn (the good case, detected
// via lastMsg), and latch it so the eventual JSONL copy is suppressed.
func (s *session) emitQuestionProse(p promptInfo) {
	prose := strings.TrimSpace(p.prose)
	n := normalizeProse(prose)
	if len(n) < proseMatchMin {
		return // nothing, or too short to dedup safely
	}
	s.mu.Lock()
	if proseMatches(n, s.lastMsg) {
		s.mu.Unlock()
		return // already emitted from the JSONL just before the question
	}
	s.shownProse = n
	s.mu.Unlock()
	tracef("question prose id=%s len=%d", s.id, len(prose))
	s.emit(agent.Event{Type: agent.EventMessage, Text: prose})
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// nudge snaps the watcher back to fast polling at the start of a turn so a
// backed-off idle interval doesn't delay the first streamed output. Non-blocking
// (the wake channel is buffered 1); a missed signal just means the next poll
// catches the new transcript line and resets the cadence itself.
func (s *session) nudge() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// withPane runs fn while holding the pane-input lock, serializing it against
// every other goroutine that drives keystrokes into this session's tmux pane.
// Wrap a whole logical input sequence (e.g. type-prompt-then-Enter, or
// capture-cursor-then-navigate-then-confirm) in one call so it lands atomically;
// the leaf drivers (selectIndex, selectMulti, typeFreeform) assume the caller
// already holds it and so must only ever run inside a withPane block.
func (s *session) withPane(fn func()) {
	s.paneMu.Lock()
	defer s.paneMu.Unlock()
	fn()
}

// Send submits a prompt. The session-long watcher (started here on first use)
// streams the response from the transcript.
//
// It deliberately does NOT hold s.mu across ensureLaunched / waitReady /
// send-keys: those block (waitReady up to readyTimeout) and call back into
// helpers that take the lock (isClosed). Holding the mutex across them both
// deadlocks the goroutine and freezes every other call on the session. The
// helpers lock only the brief field accesses they need.
func (s *session) Send(ctx context.Context, prompt string) error {
	if err := s.ensureLaunched(ctx); err != nil {
		return err
	}
	name := s.tmuxName()
	// A select dialog already on screen means this prompt is the user
	// answering it, not a new turn. This happens when the dialog's in-memory
	// question didn't survive an atc restart (the rendered question persists in
	// the transcript and the dialog persists in tmux, but question/questionCh
	// are in-memory only) — so the supervisor routes the reply here as a fresh
	// Send instead of through OnQuestion. Drive the selection directly; pasting
	// into a dialog that isn't a text field drops the input and Enter takes the
	// default. Do this before the watcher starts so we don't race its own
	// detect/answer path.
	if pane, err := s.capturePrompt(ctx, name); err == nil {
		if p, ok := detectPrompt(pane); ok {
			tracef("Send dialog-answer id=%s kind=%s sel=%q", s.id, p.kind, prompt)
			s.answerDialogDirect(ctx, p, prompt)
			s.startWatch()
			s.nudge()
			return nil
		}
	}
	s.startWatch()
	// Prime "working" so the watcher treats the turn as live immediately, before
	// the UserPromptSubmit hook lands (it will overwrite with the same value).
	s.writeState("working")
	s.nudge()
	tracef("Send id=%s claudeID=%s promptLen=%d onEvent=%t path=%q",
		s.id, s.claudeID, len(prompt), s.spec.OnEvent != nil, s.transcriptPath())
	// One atomic input sequence: type the prompt, wait until the pane reflects
	// it, then submit — with no other goroutine's keystrokes interleaving.
	// Wait until the input actually reflects what we typed before submitting.
	// A still-settling TUI (e.g. a fresh session right after a slow MCP-laden
	// boot) can lag behind a multi-line paste; pressing Enter on a fixed timer
	// then submits only the fragment it had rendered — which is how an
	// attachment prompt lost its text and kept just the image path.
	var sendErr error
	s.withPane(func() {
		if err := s.tm.SendText(ctx, name, prompt); err != nil {
			sendErr = err
			return
		}
		if !s.confirmInput(ctx, name, prompt) {
			tracef("Send id=%s WARNING: input did not reflect prompt before submit", s.id)
		}
		sendErr = s.tm.SendEnter(ctx, name)
	})
	if sendErr != nil {
		return sendErr
	}
	// /usage and /cost paint an ephemeral overlay that never reaches the
	// JSONL transcript, so the watcher can't see it. Scrape the pane out of
	// band, surface the text, and capture a limits snapshot. Reset the
	// auto-refresh throttle so it doesn't immediately scrape again.
	if isUsageCommand(prompt) {
		s.mu.Lock()
		s.lastUsageScrape = time.Now()
		s.mu.Unlock()
		go s.scrapeUsage(name, true)
	}
	return nil
}

// isUsageCommand reports whether prompt is a client-side Claude Code command
// that renders a transient overlay we must scrape rather than read from the
// transcript.
func isUsageCommand(prompt string) bool {
	switch strings.TrimSpace(prompt) {
	case "/usage", "/cost":
		return true
	}
	return false
}

// usageSettle is how long the overlay needs to finish painting before we read
// the pane. It's a TUI-version knob, like the other capture tunables.
const usageSettle = 900 * time.Millisecond

// usageRefreshInterval throttles the automatic /usage scrape. Claude has no
// per-turn account-quota event (unlike Copilot), so the watcher injects /usage
// at most this often on turn-end to keep the account-usage badge fresh without
// flashing the overlay on every turn.
const usageRefreshInterval = 10 * time.Minute

// scrapeUsage reads the /usage overlay from the pane, parses a best-effort
// limits snapshot, then dismisses the overlay so the prompt is usable again.
// announce posts the raw overlay text as a message (what a manual /usage wants);
// the throttled auto-refresh passes false so it only updates the badge.
func (s *session) scrapeUsage(name string, announce bool) {
	// A manual /usage arrived as a prompt, so the supervisor marked the session
	// working — but it runs no turn and writes no transcript, so the watcher
	// won't emit idle on its own. Return the session to "done" on every exit
	// path, or it hangs in "working" forever. (The auto-refresh fires from idle,
	// so it's already done and passes announce=false.)
	if announce {
		defer s.emit(agent.Event{Type: agent.EventIdle})
	}
	time.Sleep(usageSettle)
	if s.isClosed() {
		return
	}
	ctx := context.Background()
	pane, err := s.tm.Capture(ctx, name, tmux.CaptureOpts{})
	if err != nil {
		tracef("usage capturefail id=%s err=%v", s.id, err)
		return
	}
	text := extractUsageOverlay(pane)
	if text == "" {
		tracef("usage no-overlay id=%s", s.id)
		// Still try to dismiss in case an overlay is up but unrecognized.
		s.withPane(func() { _ = s.tm.SendKeys(ctx, name, "Escape") })
		return
	}
	if announce {
		s.emit(agent.Event{Type: agent.EventMessage, Text: "```\n" + text + "\n```"})
	}
	windows := parseUsageLimits(text)
	s.emit(agent.Event{Type: agent.EventLimits, LimitText: text, LimitWindows: windows})
	tracef("usage scraped id=%s windows=%d announce=%v", s.id, len(windows), announce)
	s.withPane(func() { _ = s.tm.SendKeys(ctx, name, "Escape") })
}

// maybeScrapeUsage injects /usage on turn-end to refresh the account-usage
// badge, throttled to usageRefreshInterval. It runs only when the turn is quiet
// (the watcher calls it right after idle), so the brief overlay it paints
// doesn't collide with an in-flight turn. Silent: it never posts the readout as
// a message.
func (s *session) maybeScrapeUsage(name string) {
	s.mu.Lock()
	if s.closed || time.Since(s.lastUsageScrape) < usageRefreshInterval {
		s.mu.Unlock()
		return
	}
	s.lastUsageScrape = time.Now()
	s.mu.Unlock()

	ctx := context.Background()
	var sendErr error
	s.withPane(func() {
		if err := s.tm.SendText(ctx, name, "/usage"); err != nil {
			sendErr = err
			return
		}
		sendErr = s.tm.SendEnter(ctx, name)
	})
	if sendErr != nil {
		return
	}
	s.scrapeUsage(name, false)
}

var (
	usageKeyword = regexp.MustCompile(`(?i)usage|limit|reset|current (session|week)|weekly|per[- ]?week|% used|tokens? (used|remaining)`)
	boxChars     = strings.NewReplacer(
		"│", " ", "┃", " ", "║", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ",
		"├", " ", "┤", " ", "┌", " ", "┐", " ", "└", " ", "┘", " ",
	)
)

// extractUsageOverlay pulls the usage panel out of a full pane capture. The
// overlay is a bordered box drawn over the transcript; we keep the contiguous
// run of lines around the usage-related keywords and strip box-drawing glyphs.
// Returns "" when nothing usage-like is on screen.
func extractUsageOverlay(pane string) string {
	lines := strings.Split(pane, "\n")
	first, last := -1, -1
	for i, ln := range lines {
		if usageKeyword.MatchString(ln) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return ""
	}
	// Widen to include adjacent non-blank lines (bars, reset dates, totals
	// that don't themselves contain a keyword).
	for first > 0 && strings.TrimSpace(stripBox(lines[first-1])) != "" {
		first--
	}
	for last < len(lines)-1 && strings.TrimSpace(stripBox(lines[last+1])) != "" {
		last++
	}
	var out []string
	for _, ln := range lines[first : last+1] {
		out = append(out, strings.TrimRight(stripBox(ln), " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func stripBox(s string) string {
	s = boxChars.Replace(s)
	// Drop runs of horizontal rule glyphs.
	s = strings.Map(func(r rune) rune {
		switch r {
		case '─', '━', '═':
			return -1
		}
		return r
	}, s)
	return s
}

// The /usage dialog renders each limit window across three lines, e.g.
//
//	Current week (all models)
//	██████████████████                                 36% used
//	Resets Jun 20, 3pm (America/Chicago)
//
// so we anchor on the "N% used" bar line and read the label from just above and
// the reset hint from just below. Anchoring on "% used" (not bare "%") is what
// keeps the "What's contributing to your limits usage?" section out — its
// percentages read "61% of your usage…", never "… % used".
var (
	usagePctRe   = regexp.MustCompile(`(\d{1,3})\s*%\s+used\b`)
	usageLabelRe = regexp.MustCompile(`(?i)^\s*current\s+(.+?)\s*$`)
	usageResetRe = regexp.MustCompile(`(?i)resets\b.*`)
)

// parseUsageLimits returns every "Current …" limit window in the scraped
// /usage text, in display order (session, weekly, per-model). If a full
// scrollback capture contains more than one /usage block, the latest reading
// of each window wins. Empty when no limit line is found.
func parseUsageLimits(text string) []agent.LimitWindow {
	lines := strings.Split(text, "\n")
	var out []agent.LimitWindow
	seen := map[string]int{} // label -> index in out, so a later block overwrites
	for i, ln := range lines {
		pm := usagePctRe.FindStringSubmatch(ln)
		if pm == nil {
			continue
		}
		// Label: nearest "Current …" line at or just above the bar.
		label := ""
		for j := i; j >= 0 && j >= i-3; j-- {
			if lm := usageLabelRe.FindStringSubmatch(lines[j]); lm != nil {
				label = strings.TrimSpace(lm[1])
				break
			}
		}
		if label == "" {
			continue // a "% used" bar with no Current header isn't a window
		}
		// Resets hint: nearest "Resets …" line at or just below the bar.
		resets := ""
		for j := i; j < len(lines) && j <= i+3; j++ {
			if rm := usageResetRe.FindString(lines[j]); rm != "" {
				resets = strings.TrimSpace(rm)
				break
			}
		}
		var pct float64
		fmt.Sscanf(pm[1], "%f", &pct)
		w := agent.LimitWindow{
			Label:  label,
			Pct:    pct,
			Resets: resets,
		}
		if i, ok := seen[w.Label]; ok {
			out[i] = w
		} else {
			seen[w.Label] = len(out)
			out = append(out, w)
		}
	}
	return out
}

// confirmInput polls until the pane shows the start of the prompt we just
// typed (or a short deadline elapses), so Enter isn't pressed before a
// slow/booting TUI has finished accepting a multi-line prompt.
func (s *session) confirmInput(ctx context.Context, name, prompt string) bool {
	probe := prompt
	if i := strings.IndexByte(probe, '\n'); i >= 0 {
		probe = probe[:i]
	}
	probe = strings.TrimSpace(probe)
	if len(probe) > 40 {
		probe = probe[:40]
	}
	if probe == "" {
		return true
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.isClosed() {
			return false
		}
		if pane, err := s.tm.Capture(ctx, name, tmux.CaptureOpts{}); err == nil && strings.Contains(pane, probe) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// isStarted / markStarted guard the started flag for callers that no longer
// hold s.mu themselves.
func (s *session) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *session) markStarted() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
}

// startWatch launches the single, session-long transcript watcher the first
// time it is called (caller holds mu). Keeping one watcher for the whole
// session — rather than one per turn — means the read offset only ever moves
// forward as content is emitted. A turn whose tail is written shortly after we
// detect idle is therefore still picked up on a later poll, never skipped (the
// per-turn design reset the offset on every Send and lost any late-written
// tail).
func (s *session) startWatch() {
	s.mu.Lock()
	if s.watching {
		s.mu.Unlock()
		return
	}
	s.watching = true
	// Start at the current end of file so we don't re-emit history the
	// supervisor already replayed via History().
	s.offset = transcriptSize(s.transcriptPath())
	off := s.offset
	s.mu.Unlock()
	// Drop any state file left by a previous run so the watcher starts unlatched
	// and uses the legacy heuristic until a fresh Stop hook proves itself — a
	// stale "idle" here would otherwise emit a false turn-end over a reattached
	// in-flight turn. Guarded by s.watching, so this clears only at session start.
	if p := s.statePath(); p != "" {
		_ = os.Remove(p)
	}
	tracef("watch start id=%s off=%d path=%q", s.id, off, s.transcriptPath())
	go s.watch()
}

// ensureLaunched makes sure claude is running in the tmux session, creating the
// session (or recovering a dead claude) as needed. It does its own brief
// locking; the caller must NOT hold s.mu (it blocks in waitReady).
func (s *session) ensureLaunched(ctx context.Context) error {
	name := s.tmuxName()
	has, err := s.tm.HasSession(ctx, name)
	if err != nil {
		return err
	}
	started := s.isStarted()
	tracef("ensureLaunched id=%s started=%t hasSession=%t", s.id, started, has)
	if has {
		// Layer-2: tmux is alive but claude may have exited, leaving a shell.
		if cmd, err := s.tm.PaneCommand(ctx, name); err == nil && isShell(cmd) {
			tracef("ensureLaunched id=%s relaunch-claude (pane was shell)", s.id)
			// Relaunch claude --resume into the same pane.
			line := shellJoin(append([]string{"claude"}, s.claudeArgs(true)...))
			var sendErr error
			s.withPane(func() {
				if err := s.tm.SendText(ctx, name, "unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN; exec "+line); err != nil {
					sendErr = err
					return
				}
				sendErr = s.tm.SendEnter(ctx, name)
			})
			if sendErr != nil {
				return sendErr
			}
			s.waitReady(ctx)
			tracef("ensureLaunched id=%s relaunch ready", s.id)
			return nil
		}
		tracef("ensureLaunched id=%s claude-alive", s.id)
		return nil // claude assumed alive
	}
	tracef("ensureLaunched id=%s fresh-launch", s.id)

	// Fresh tmux session. Launch via a shell that strips API-key env vars, so
	// claude authenticates with the subscription OAuth token (subscription
	// billing) rather than pay-as-you-go API credits.
	resume := started
	launch := "unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN; exec " +
		shellJoin(append([]string{"claude"}, s.claudeArgs(resume)...))
	if err := s.tm.NewSession(ctx, tmux.NewSessionOpts{
		Name:       name,
		Command:    []string{"sh", "-c", launch},
		WorkingDir: s.spec.WorkingDir,
		Width:      paneWidth,
		Height:     paneHeight,
	}); err != nil {
		return err
	}
	_ = s.tm.SetOption(ctx, name, "history-limit", historyLimit)
	s.markStarted()
	s.waitReady(ctx) // let the TUI finish booting before the first prompt
	if !resume {
		s.discoverClaudeID()
	}
	tracef("ensureLaunched id=%s fresh-launch ready", s.id)
	return nil
}

// waitReady blocks until the TUI shows it is up and accepting input, or a
// deadline elapses. Without this, the first prompt can be typed into a
// still-booting claude (config + MCP load) and silently dropped.
func (s *session) waitReady(ctx context.Context) {
	name := s.tmuxName()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if s.isClosed() {
			return
		}
		pane, err := s.tm.Capture(ctx, name, tmux.CaptureOpts{})
		if err == nil {
			// The first-run trust dialog blocks input; auto-accept it (the
			// "Yes" option is preselected, so Enter confirms) and keep waiting.
			if isTrustPrompt(pane) {
				s.withPane(func() { _ = s.tm.SendEnter(ctx, name) })
				time.Sleep(time.Second)
				continue
			}
			if containsAny(pane, readyMarkers) {
				time.Sleep(readySettle) // let the input box become live before we type
				return
			}
		}
		time.Sleep(pollInterval)
	}
}

// claudeArgs builds the interactive launch flags. With resume it continues the
// known conversation; otherwise it pins a fresh session id we control.
func (s *session) claudeArgs(resume bool) []string {
	var args []string
	if resume {
		args = append(args, "--resume", s.claudeID)
	} else {
		args = append(args, "--session-id", s.id)
	}
	if s.spec.Model != "" {
		args = append(args, "--model", s.spec.Model)
	}
	switch {
	case s.spec.ReadOnly:
		args = append(args, "--permission-mode", "plan")
	case s.spec.Approval == config.ApprovalAllowAll:
		args = append(args, "--permission-mode", "bypassPermissions")
	default:
		args = append(args, "--permission-mode", "acceptEdits")
	}
	// Stamp turn state via injected lifecycle hooks so the watcher reads a file
	// instead of polling capture-pane (see the hook-driven state section).
	if js := s.hookSettingsJSON(); js != "" {
		args = append(args, "--settings", js)
	}
	// Inject atc-config agents inline so they need not live in the repo's
	// .claude/agents, then activate the tagged one as the primary persona.
	if js := agentsJSON(s.spec.Agents); js != "" {
		args = append(args, "--agents", js)
	}
	if s.spec.Agent != "" {
		args = append(args, "--agent", s.spec.Agent)
	}
	return args
}

// agentsJSON renders atc's agent definitions as Claude Code's --agents
// payload: a JSON object keyed by agent name. Empty tools/model are
// omitted (Claude treats a missing tools list as "all tools"). Returns ""
// when there are no agents to inject.
func agentsJSON(defs []agent.AgentDef) string {
	if len(defs) == 0 {
		return ""
	}
	type claudeAgent struct {
		Description string   `json:"description,omitempty"`
		Prompt      string   `json:"prompt"`
		Tools       []string `json:"tools,omitempty"`
		Model       string   `json:"model,omitempty"`
	}
	m := make(map[string]claudeAgent, len(defs))
	for _, d := range defs {
		m[d.Name] = claudeAgent{
			Description: d.Description,
			Prompt:      d.Prompt,
			Tools:       d.Tools,
			Model:       d.Model,
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// --- Hook-driven state -----------------------------------------------------
//
// Rather than scrape `capture-pane` every poll to guess whether a turn is
// running, we inject two Claude Code lifecycle hooks (via --settings) that
// stamp the turn state into a per-session file the moment it changes:
//   - UserPromptSubmit -> "working" (a turn just started)
//   - Stop             -> "idle"    (the turn finished; Stop fires only on full
//                          turn completion, never for a pending permission box
//                          or AskUserQuestion — verified against the hooks docs)
// The watcher reads that file (a cheap stat+read, no subprocess) and only does
// the expensive pane work while a turn is actually in flight. An idle board
// therefore spawns no tmux processes at all. capture-pane is still used, but
// only while "working", to read and drive the interactive permission/question
// pickers (the hook says *when* a box is up, not its rendered options).

// statePath is the per-session file the hooks stamp the turn state into.
func (s *session) statePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".atc", "state", s.id+".state")
}

// writeState records the turn state directly (used to prime "working" the
// instant we submit a prompt, closing the gap before UserPromptSubmit fires).
func (s *session) writeState(v string) {
	p := s.statePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(v), 0o644)
}

// readHookState returns the stamped turn state and whether the file exists yet.
func (s *session) readHookState() (string, bool) {
	p := s.statePath()
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// hookSettingsJSON builds the --settings payload that stamps turn state into the
// session's state file. The commands are plain POSIX shell redirects (the tmux
// backend always runs under a Unix shell); they merge additively with whatever
// hooks the user's own settings define. Returns "" if the state path is
// unavailable, so launch falls back cleanly to the legacy heuristic.
func (s *session) hookSettingsJSON() string {
	p := s.statePath()
	if p == "" {
		return ""
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	type cmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []cmd `json:"hooks"`
	}
	write := func(state string) []matcher {
		// %s-format so a single-quote in the (uuid-derived) path can't break out.
		return []matcher{{Hooks: []cmd{{Type: "command", Command: fmt.Sprintf("printf %%s %s > %s", state, shellQuote(p))}}}}
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": write("working"),
			"Stop":             write("idle"),
		},
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return ""
	}
	return string(b)
}

// shellQuote wraps s in single quotes for a POSIX shell, escaping any embedded
// single quote. Used so the hook redirect target is a safe shell token.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// discoverClaudeID waits for the session's own jsonl (named by --session-id)
// to appear so the first tail/replay reads from the right file. claudeID is
// seeded to s.id in the constructor and is never reassigned to another file:
// modern Claude Code honors --session-id, and adopting the "newest" transcript
// in the dir cross-binds concurrent sessions that share a project directory
// (one session ends up tailing another's transcript). If the expected file
// never shows up within the deadline we leave claudeID == id and let the tail
// loop pick it up once it does. Caller holds mu.
func (s *session) discoverClaudeID() {
	expected := filepath.Join(s.transcriptDir(), s.id+".jsonl")
	deadline := time.Now().Add(idDiscovery)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(expected); err == nil {
			return // --session-id honored; claudeID already == id
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// watch is the session-long loop: it tails the transcript (emitting assistant
// text, tool calls, and usage as they are written), answers permission /
// question prompts, and emits EventIdle once each time a turn goes quiet. It
// runs until Close or until claude dies inside the tmux session. Because it
// never resets the offset, a turn's tail written shortly after a (possibly
// early) idle is still emitted on a later poll rather than skipped.
func (s *session) watch() {
	name := s.tmuxName()
	lastActivity := time.Now()
	idleEmitted := true  // armed only after the first activity, so no idle before turn 1
	hooksActive := false // latches once the Stop hook proves it's firing for this session
	surfacedSig := ""    // signature of the question box already surfaced; suppresses re-surfacing the same box
	interval := pollInterval
	for {
		if s.isClosed() {
			return
		}
		// Adaptive cadence: poll fast while a turn is live, then ramp toward
		// idlePollMax once it's gone quiet. Each live session otherwise spawns
		// ~2 tmux processes every poll forever; backing off when idle cuts the
		// steady-state exec rate ~5x. A Send (or other turn start) closes over
		// the wake channel to snap us back to fast polling without waiting out
		// a long idle interval.
		select {
		case <-time.After(interval):
		case <-s.wake:
			interval = pollInterval
			lastActivity = time.Now()
			idleEmitted = false
		}
		ctx := context.Background()

		// Emit any new transcript lines (assistant text, tool calls, usage).
		if evs := s.drainTranscript(); len(evs) > 0 {
			s.emitDrained(evs)
			lastActivity = time.Now()
			idleEmitted = false
			interval = pollInterval
		}

		// Hook-driven turn state. hooksActive latches once we observe an "idle"
		// stamped by the Stop hook (only a hook writes "idle"; Send and
		// UserPromptSubmit write "working"). Until it latches — the first turn, an
		// older claude that ignores --settings hooks, or a freshly resumed session
		// — we fall through to the legacy capture-pane heuristic so nothing
		// regresses.
		hookState, hookPresent := s.readHookState()
		if hookPresent && hookState == "idle" {
			hooksActive = true
		}
		if hooksActive && hookState != "working" {
			// Hooks report no turn in flight. Skip the death check and pane scrape
			// entirely — an idle board spawns zero tmux processes — and emit
			// EventIdle once on the working->idle edge. A new Send re-primes
			// "working" and wakes us, so responsiveness is unaffected.
			if !idleEmitted {
				tracef("watch idle(hook) id=%s", s.id)
				s.emit(agent.Event{Type: agent.EventIdle})
				idleEmitted = true
				go s.maybeScrapeUsage(name)
			}
			if interval < idlePollMax {
				interval += idleRampStep
				if interval > idlePollMax {
					interval = idlePollMax
				}
			}
			continue
		}
		if hooksActive {
			// state == "working": a turn is live (this also covers a pending
			// permission box or AskUserQuestion — Stop doesn't fire for those, so
			// the state stays "working" and the pane scrape below catches them).
			idleEmitted = false
			interval = pollInterval
		}

		// If claude died inside the session, surface it and stop; the next
		// Send will --resume it and restart the watcher.
		if cmd, err := s.tm.PaneCommand(ctx, name); err == nil && isShell(cmd) {
			tracef("watch claude-died id=%s", s.id)
			s.mu.Lock()
			s.watching = false
			s.mu.Unlock()
			s.emit(agent.Event{Type: agent.EventError, ErrType: "process", Text: "claude exited inside tmux (will --resume on next prompt)"})
			return
		}

		pane, err := s.capturePrompt(ctx, name)
		if err == nil {
			if p, ok := detectPrompt(pane); ok {
				// Either kind means claude is blocked on us, so never let the
				// idle timer below fire a false turn-end ("done") while a box is
				// on screen.
				switch p.kind {
				case "permission":
					// Surface the permission box through OnPermission on its own
					// goroutine — like the question path — so the watcher keeps
					// tailing the transcript and watching for a dead claude while
					// the user decides, instead of freezing on the blocking
					// OnPermission call. The answering guard launches exactly one
					// handler per box: the box stays on screen until answered, so
					// without it every poll would fire another OnPermission and
					// enqueue a duplicate permission. Always hold off idle while a
					// box is up — claude is blocked on us.
					s.mu.Lock()
					already := s.answering
					if !already {
						s.answering = true
					}
					s.mu.Unlock()
					if !already {
						tracef("watch permission id=%s title=%q", s.id, p.title)
						go s.handlePrompt(context.Background(), p)
					}
					lastActivity = time.Now()
					idleEmitted = false
					continue
				default: // "question" (AskUserQuestion)
					// Surface the question exactly once per box via OnQuestion
					// (frames it + sets the session "waiting"), then drive the
					// reply into the picker — all on a background goroutine so the
					// watcher keeps tailing. Two guards keep one ask from becoming a
					// flood of repeated chip prompts: the questioning flag suppresses
					// re-firing while the handler is in flight, and surfacedSig
					// suppresses re-firing the *same* box after it's been answered —
					// a freeform reply that leaves the identical question on screen
					// (Claude re-renders the same tab) would otherwise bounce back on
					// every answer. surfacedSig is cleared only when the box actually
					// clears (the no-box branch below), so a genuinely new ask — or
					// the same one after it's gone away once — still surfaces.
					sig := questionSig(p)
					if s.spec.OnQuestion != nil && sig != surfacedSig {
						s.mu.Lock()
						already := s.questioning
						var cancel chan struct{}
						if !already {
							s.questioning = true
							cancel = make(chan struct{})
							s.questionCancel = cancel
						}
						s.mu.Unlock()
						if !already {
							surfacedSig = sig
							// Emit any prose that did make it to the JSONL in order,
							// synchronously in the watcher goroutine so it can't race
							// the loop's own offset.
							s.emitDrained(s.drainTranscript())
							// The prose preceding a question shares the AskUserQuestion's
							// assistant turn, which Claude Code often doesn't flush to the
							// JSONL until the question is answered — so the drain above
							// can't supply it and it would otherwise land after the user
							// has already answered. Surface it from the pane instead.
							s.emitQuestionProse(p)
							tracef("watch question id=%s title=%q multi=%t", s.id, p.title, p.multiSelect)
							go s.handleQuestion(context.Background(), p, cancel)
						}
					}
					if s.spec.OnQuestion != nil {
						// Blocked on the user: hold off idle/done entirely.
						continue
					}
				}
			} else if err == nil {
				// No box on screen: the picker cleared, so re-arm surfacing — the
				// same question may legitimately ask again later (and a new one
				// always differs). If a question handler is still waiting on the
				// user, the picker vanished (they cleared it by hand in tmux, or
				// claude withdrew it) — withdraw the question so the user's input
				// stops being routed as an answer and starts a fresh turn again.
				surfacedSig = ""
				s.mu.Lock()
				cancel := s.questionCancel
				if s.questioning && cancel != nil {
					s.questionCancel = nil
				} else {
					cancel = nil
				}
				s.mu.Unlock()
				if cancel != nil {
					tracef("watch question-vanished id=%s", s.id)
					close(cancel)
				}
			}
		}

		// Still working: keep the idle timer pushed forward and poll fast.
		if err == nil && isWorking(pane) {
			lastActivity = time.Now()
			idleEmitted = false
			interval = pollInterval
			continue
		}

		// Quiet and not working: emit one EventIdle per quiet period. The
		// watcher keeps running, so any late tail re-arms and is still emitted.
		// Skipped once hooks are proven — the Stop hook is the authoritative,
		// exact turn-end signal then, so the heuristic must not double-fire.
		if !hooksActive && !idleEmitted && time.Since(lastActivity) > quiescence {
			tracef("watch idle id=%s", s.id)
			s.emit(agent.Event{Type: agent.EventIdle})
			idleEmitted = true
			// Turn just went quiet — a safe moment to refresh the account-usage
			// badge (throttled), since no overlay would collide with a turn.
			go s.maybeScrapeUsage(name)
		}

		// Once the turn is quiet, lengthen the interval one step per poll toward
		// idlePollMax. New output (drainTranscript), a Send wake, or detected
		// work all snap it straight back to pollInterval above.
		if idleEmitted && interval < idlePollMax {
			interval += idleRampStep
			if interval > idlePollMax {
				interval = idlePollMax
			}
		}
	}
}

// drainTranscript reads transcript bytes written since the last read and
// converts them to events (assistant/tool/usage only — not the user's own
// prompt). It advances the offset past whole lines only.
func (s *session) drainTranscript() []agent.Event {
	path := s.transcriptPath()
	f, err := os.Open(path)
	if err != nil {
		tracef("drain openfail id=%s path=%q err=%v", s.id, path, err)
		return nil
	}
	defer f.Close()

	s.mu.Lock()
	off := s.offset
	s.mu.Unlock()

	if _, err := f.Seek(off, 0); err != nil {
		return nil
	}
	r := bufio.NewReader(f)
	var out []agent.Event
	var consumed int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			consumed += int64(len(line))
			out = append(out, eventsFromLine(line, false)...)
		}
		if err != nil { // EOF or partial trailing line: stop, keep offset at line start
			break
		}
	}
	if consumed > 0 {
		s.mu.Lock()
		s.offset += consumed
		s.mu.Unlock()
		tracef("drain id=%s consumed=%d events=%d", s.id, consumed, len(out))
	}
	return out
}

// SetModel switches the model for subsequent turns via the TUI's /model
// command (no relaunch). If claude isn't running yet, the new model is applied
// at launch.
func (s *session) SetModel(ctx context.Context, model string) error {
	s.mu.Lock()
	s.spec.Model = model
	s.mu.Unlock()
	name := s.tmuxName()
	has, err := s.tm.HasSession(ctx, name)
	if err != nil || !has {
		return nil // applied via claudeArgs on next launch
	}
	// Drive the pane outside s.mu (lock order is paneMu→s.mu) and as one
	// atomic sequence so the /model command can't interleave with a prompt.
	var sendErr error
	s.withPane(func() {
		if err := s.tm.SendText(ctx, name, "/model "+model); err != nil {
			sendErr = err
			return
		}
		sendErr = s.tm.SendEnter(ctx, name)
	})
	return sendErr
}

// Abort interrupts the current turn by sending Escape — the interactive TUI's
// stop key — leaving the conversation intact for the next prompt.
func (s *session) Abort(ctx context.Context) error {
	name := s.tmuxName()
	has, err := s.tm.HasSession(ctx, name)
	if err != nil || !has {
		return nil
	}
	var sendErr error
	s.withPane(func() { sendErr = s.tm.SendKeys(ctx, name, "Escape") })
	return sendErr
}

// Close stops watchers and tears down the tmux session. The on-disk transcript
// persists, so the conversation can still be resumed later via --resume.
func (s *session) Close() error {
	s.mu.Lock()
	s.closed = true
	s.watching = false
	s.mu.Unlock()
	if p := s.statePath(); p != "" {
		_ = os.Remove(p)
	}
	return s.tm.KillSession(context.Background(), s.tmuxName())
}

// --- on-disk transcript: path resolution, parsing, and replay -------------

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// transcriptDir is the per-project directory Claude Code stores sessions in:
// ~/.claude/projects/<cwd with non-alphanumerics dashed>/
func (s *session) transcriptDir() string {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".claude")
	}
	dir, err := filepath.Abs(s.spec.WorkingDir)
	if err != nil {
		return ""
	}
	return filepath.Join(base, "projects", nonAlnum.ReplaceAllString(dir, "-"))
}

// transcriptPath is this session's jsonl file.
func (s *session) transcriptPath() string {
	dir := s.transcriptDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, s.claudeID+".jsonl")
}

// transcriptSize returns the current size of a transcript file (0 if missing).
func transcriptSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// History replays the persisted transcript as events, oldest first.
func (s *session) History(_ context.Context) []agent.Event {
	path := s.transcriptPath()
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []agent.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		out = append(out, eventsFromLine(sc.Bytes(), true)...)
	}
	return out
}

// transcriptLine is one entry in Claude Code's session jsonl.
type transcriptLine struct {
	Type    string  `json:"type"`
	IsMeta  bool    `json:"isMeta"`
	CostUSD float64 `json:"costUSD"` // present in some Claude Code versions
	Message *struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *usageBlock     `json:"usage"`
	} `json:"message"`
}

// eventsFromLine converts one transcript line to events. replay is true for
// History replay and false for live tailing. It governs two things that the
// live UI renders out of band: the user's own prompts (just typed, so not
// re-emitted live) and an AskUserQuestion box (surfaced as interactive chips
// via OnQuestion live, but only as text on replay where no chips exist).
func eventsFromLine(raw []byte, replay bool) []agent.Event {
	var line transcriptLine
	if json.Unmarshal(raw, &line) != nil || line.IsMeta || line.Message == nil {
		return nil
	}
	switch line.Type {
	case "user":
		if !replay {
			return nil
		}
		var out []agent.Event
		var text string
		if json.Unmarshal(line.Message.Content, &text) == nil {
			if text != "" {
				out = append(out, agent.Event{Type: agent.EventUserMessage, Text: text})
			}
			return out
		}
		var blocks []contentBlock
		if json.Unmarshal(line.Message.Content, &blocks) != nil {
			return nil
		}
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				out = append(out, agent.Event{Type: agent.EventUserMessage, Text: b.Text})
			}
		}
		return out
	case "assistant":
		out := messageEvents(line.Message.Content, replay)
		if u := line.Message.Usage; u != nil && (u.InputTokens > 0 || u.OutputTokens > 0) {
			out = append(out, agent.Event{
				Type:         agent.EventUsage,
				InputTokens:  u.InputTokens,
				OutputTokens: u.OutputTokens,
				CostUSD:      line.CostUSD,
				Model:        line.Message.Model,
			})
		}
		return out
	}
	return nil
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type usageBlock struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// messageEvents converts an assistant message's content blocks into transcript
// events. Content is either a plain string or a block array. replay keeps an
// AskUserQuestion box rendered as text on History replay; live, the watcher
// surfaces it as interactive chips through OnQuestion, so emitting the text too
// would double up.
func messageEvents(content json.RawMessage, replay bool) []agent.Event {
	var out []agent.Event
	var text string
	if json.Unmarshal(content, &text) == nil {
		if text != "" {
			out = append(out, agent.Event{Type: agent.EventMessage, Text: text})
		}
		return out
	}
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, agent.Event{Type: agent.EventMessage, Text: b.Text})
			}
		case "tool_use":
			// AskUserQuestion is surfaced live as interactive chips via
			// OnQuestion, so render it as text only on replay (no chips there);
			// emitting it live too would duplicate the question.
			if b.Name == "AskUserQuestion" {
				if replay {
					if q := formatAskUserQuestion(b.Input); q != "" {
						out = append(out, agent.Event{Type: agent.EventMessage, Text: q})
					}
				}
				continue
			}
			out = append(out, agent.Event{Type: agent.EventToolStart, Text: agent.ToolSummary(b.Name, anyMap(b.Input))})
		}
	}
	return out
}

func anyMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// formatAskUserQuestion turns AskUserQuestion's input into a readable markdown
// prompt for the transcript.
// latestQuestionDetails scans the transcript for the most recent
// AskUserQuestion tool call and returns a label->description map across all its
// questions. The on-screen picker truncates long descriptions, so we read the
// full text Claude wrote from the transcript and match it to the scraped option
// labels. Returns nil if no AskUserQuestion is found.
func (s *session) latestQuestionDetails() map[string]string {
	f, err := os.Open(s.transcriptPath())
	if err != nil {
		return nil
	}
	defer f.Close()

	var found map[string]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var line transcriptLine
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Message == nil || line.Type != "assistant" {
			continue
		}
		var blocks []contentBlock
		if json.Unmarshal(line.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.Name == "AskUserQuestion" {
				if m := questionDetailMap(b.Input); m != nil {
					found = m // keep scanning: the last one wins
				}
			}
		}
	}
	return found
}

// matchDetail finds a label's description in m, tolerating a label the pane
// truncated with an ellipsis by falling back to a prefix match.
func matchDetail(m map[string]string, label string) string {
	label = strings.TrimSpace(label)
	if d, ok := m[label]; ok {
		return d
	}
	if trimmed := strings.TrimRight(label, "… "); trimmed != label && trimmed != "" {
		for k, d := range m {
			if strings.HasPrefix(k, trimmed) {
				return d
			}
		}
	}
	return ""
}

// questionDetailMap flattens an AskUserQuestion input into label->description
// across all its questions (entries without a description are skipped).
func questionDetailMap(input map[string]any) map[string]string {
	qs, _ := input["questions"].([]any)
	out := map[string]string{}
	for _, qi := range qs {
		m, _ := qi.(map[string]any)
		opts, _ := m["options"].([]any)
		for _, oi := range opts {
			om, _ := oi.(map[string]any)
			label, _ := om["label"].(string)
			desc, _ := om["description"].(string)
			if label != "" && desc != "" {
				out[strings.TrimSpace(label)] = strings.TrimSpace(desc)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func formatAskUserQuestion(input map[string]any) string {
	qs, _ := input["questions"].([]any)
	var b strings.Builder
	for _, qi := range qs {
		m, ok := qi.(map[string]any)
		if !ok {
			continue
		}
		header, _ := m["header"].(string)
		question, _ := m["question"].(string)
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("**❓ ")
		if header != "" {
			b.WriteString(header + ": ")
		}
		b.WriteString(question + "**")
		opts, _ := m["options"].([]any)
		for _, oi := range opts {
			om, ok := oi.(map[string]any)
			if !ok {
				continue
			}
			label, _ := om["label"].(string)
			desc, _ := om["description"].(string)
			b.WriteString("\n- **" + label + "**")
			if desc != "" {
				b.WriteString(" — " + desc)
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteString("\n\n_Reply with your choice._")
	return b.String()
}

// --- small helpers --------------------------------------------------------

// isShell reports whether a pane's foreground command is a shell — i.e. claude
// is no longer running in it.
func isShell(cmd string) bool {
	switch cmd {
	case "sh", "bash", "zsh", "fish", "dash":
		return true
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// shellJoin quotes args for safe inclusion in an `sh -c` command line.
func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
