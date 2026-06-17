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
//   - `tmux capture-pane` is used only to detect turn-end (when claude stops
//     "working" and is idle), and `pane_current_command` to detect a claude
//     that died inside a still-living tmux session (layer-2 recovery).
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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/tmux"
)

// Tunables for driving and observing the TUI. These are the knobs most likely
// to need adjustment against a specific Claude Code version.
const (
	paneWidth    = 200
	paneHeight   = 50
	historyLimit = "50000" // tmux scrollback lines

	pollInterval = 300 * time.Millisecond // how often we tail jsonl + capture pane
	quiescence   = 800 * time.Millisecond // idle = no new transcript for this long while not "working"
	idDiscovery  = 8 * time.Second        // how long to wait for the session jsonl to appear
	readyTimeout = 30 * time.Second       // how long to wait for the TUI to accept input after launch
	sendKeyDelay = 150 * time.Millisecond // pause between typing a prompt and pressing Enter
)

// readyMarkers are chrome the claude TUI shows once it is up and accepting
// input. We wait for one of these after launching before sending the first
// prompt — otherwise keystrokes typed during startup (config + MCP load) are
// dropped. Tunable against a live capture-pane.
var readyMarkers = []string{"shift+tab", "for shortcuts", "bypass permissions"}

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
	id := uuid.NewString()
	return &session{id: id, claudeID: id, spec: spec, tm: tm}, nil
}

func (b *Backend) ResumeSession(_ context.Context, spec agent.SessionSpec) (agent.Session, error) {
	tm, err := requirements()
	if err != nil {
		return nil, err
	}
	return &session{id: spec.SessionID, claudeID: spec.SessionID, spec: spec, tm: tm, started: true}, nil
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

	id       string // atc session id (uuid); also seeds claude --session-id and the tmux name
	claudeID string // id of the on-disk jsonl transcript; usually == id, re-discovered if needed
	spec     agent.SessionSpec
	tm       *tmux.Client

	started bool  // claude has been launched at least once for this id (resume vs new)
	closed  bool  // Close was called; watchers should stop
	offset  int64 // byte offset into the transcript already emitted this run
}

func (s *session) ID() string { return s.id }

// tmuxName is the tmux session that hosts this conversation's claude process.
func (s *session) tmuxName() string { return "atc-" + s.id }

func (s *session) emit(e agent.Event) {
	if s.spec.OnEvent != nil {
		s.spec.OnEvent(e)
	}
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Send submits a prompt and starts watching the transcript for the response.
func (s *session) Send(ctx context.Context, prompt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLaunched(ctx); err != nil {
		return err
	}
	// Mark where this turn begins so the watcher only emits new transcript
	// lines, then type the prompt and submit it.
	s.offset = transcriptSize(s.transcriptPath())
	if err := s.tm.SendText(ctx, s.tmuxName(), prompt); err != nil {
		return err
	}
	// Some TUIs drop an Enter that arrives in the same instant as the pasted
	// text; a short pause makes submission reliable.
	time.Sleep(sendKeyDelay)
	if err := s.tm.SendEnter(ctx, s.tmuxName()); err != nil {
		return err
	}
	go s.watchTurn()
	return nil
}

// ensureLaunched makes sure claude is running in the tmux session, creating the
// session (or recovering a dead claude) as needed. Caller holds mu.
func (s *session) ensureLaunched(ctx context.Context) error {
	name := s.tmuxName()
	has, err := s.tm.HasSession(ctx, name)
	if err != nil {
		return err
	}
	if has {
		// Layer-2: tmux is alive but claude may have exited, leaving a shell.
		if cmd, err := s.tm.PaneCommand(ctx, name); err == nil && isShell(cmd) {
			// Relaunch claude --resume into the same pane.
			line := shellJoin(append([]string{"claude"}, s.claudeArgs(true)...))
			if err := s.tm.SendText(ctx, name, "unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN; exec "+line); err != nil {
				return err
			}
			if err := s.tm.SendEnter(ctx, name); err != nil {
				return err
			}
			s.waitReady(ctx)
			return nil
		}
		return nil // claude assumed alive
	}

	// Fresh tmux session. Launch via a shell that strips API-key env vars, so
	// claude authenticates with the subscription OAuth token (subscription
	// billing) rather than pay-as-you-go API credits.
	resume := s.started
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
	s.started = true
	s.waitReady(ctx) // let the TUI finish booting before the first prompt
	if !resume {
		s.discoverClaudeID()
	}
	return nil
}

// waitReady blocks until the TUI shows it is up and accepting input, or a
// deadline elapses. Without this, the first prompt can be typed into a
// still-booting claude (config + MCP load) and silently dropped.
func (s *session) waitReady(ctx context.Context) {
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if s.isClosed() {
			return
		}
		if pane, err := s.tm.Capture(ctx, s.tmuxName(), tmux.CaptureOpts{}); err == nil && containsAny(pane, readyMarkers) {
			return
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
	return args
}

// discoverClaudeID waits for the session's jsonl to appear. If the file we
// expect (named by --session-id) shows up, claudeID is already correct;
// otherwise we adopt the newest transcript created since launch. Caller holds mu.
func (s *session) discoverClaudeID() {
	dir := s.transcriptDir()
	expected := filepath.Join(dir, s.id+".jsonl")
	deadline := time.Now().Add(idDiscovery)
	start := time.Now().Add(-2 * time.Second) // small skew allowance
	for time.Now().Before(deadline) {
		if _, err := os.Stat(expected); err == nil {
			return // --session-id honored; claudeID already == id
		}
		if newest := newestTranscript(dir, start); newest != "" {
			s.claudeID = strings.TrimSuffix(filepath.Base(newest), ".jsonl")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// watchTurn tails the transcript and the pane until the turn completes,
// emitting transcript events and a final EventIdle.
func (s *session) watchTurn() {
	name := s.tmuxName()
	lastChange := time.Now()
	sawOutput := false
	for {
		if s.isClosed() {
			return
		}
		time.Sleep(pollInterval)
		ctx := context.Background()

		// Emit any new transcript lines (assistant text, tool calls, usage).
		if evs := s.drainTranscript(); len(evs) > 0 {
			for _, e := range evs {
				s.emit(e)
			}
			sawOutput = true
			lastChange = time.Now()
		}

		// If claude died inside the session, surface it and stop.
		if cmd, err := s.tm.PaneCommand(ctx, name); err == nil && isShell(cmd) {
			s.emit(agent.Event{Type: agent.EventError, ErrType: "process", Text: "claude exited inside tmux (will --resume on next prompt)"})
			return
		}

		pane, err := s.tm.Capture(ctx, name, tmux.CaptureOpts{})
		if err == nil {
			// A permission box or AskUserQuestion picker means claude is
			// blocked on us — answer it (this routes through OnPermission/
			// OnQuestion and blocks until the user decides), and don't treat
			// the wait as either "working" or "idle".
			if p, ok := detectPrompt(pane); ok {
				s.handlePrompt(ctx, p)
				lastChange = time.Now()
				sawOutput = true
				continue
			}
		}

		// Turn is done when claude is no longer "working" and the transcript
		// has been quiet for a moment after producing something.
		working := err == nil && isWorking(pane)
		if sawOutput && !working && time.Since(lastChange) > quiescence {
			s.emit(agent.Event{Type: agent.EventIdle})
			return
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
	}
	return out
}

// SetModel switches the model for subsequent turns via the TUI's /model
// command (no relaunch). If claude isn't running yet, the new model is applied
// at launch.
func (s *session) SetModel(ctx context.Context, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spec.Model = model
	has, err := s.tm.HasSession(ctx, s.tmuxName())
	if err != nil || !has {
		return nil // applied via claudeArgs on next launch
	}
	if err := s.tm.SendText(ctx, s.tmuxName(), "/model "+model); err != nil {
		return err
	}
	return s.tm.SendEnter(ctx, s.tmuxName())
}

// Abort interrupts the current turn by sending Escape — the interactive TUI's
// stop key — leaving the conversation intact for the next prompt.
func (s *session) Abort(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	has, err := s.tm.HasSession(ctx, s.tmuxName())
	if err != nil || !has {
		return nil
	}
	return s.tm.SendKeys(ctx, s.tmuxName(), "Escape")
}

// Close stops watchers and tears down the tmux session. The on-disk transcript
// persists, so the conversation can still be resumed later via --resume.
func (s *session) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
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

// newestTranscript returns the most recently modified *.jsonl in dir whose
// modtime is at/after `after`, or "" if none.
func newestTranscript(dir string, after time.Time) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(after) {
			continue
		}
		cands = append(cands, cand{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path
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

// eventsFromLine converts one transcript line to events. includeUser controls
// whether the user's own prompts are emitted (true for History replay, false
// for live tailing where the prompt was just typed by the user).
func eventsFromLine(raw []byte, includeUser bool) []agent.Event {
	var line transcriptLine
	if json.Unmarshal(raw, &line) != nil || line.IsMeta || line.Message == nil {
		return nil
	}
	switch line.Type {
	case "user":
		if !includeUser {
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
		out := messageEvents(line.Message.Content)
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
// events. Content is either a plain string or a block array.
func messageEvents(content json.RawMessage) []agent.Event {
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
			// AskUserQuestion has no answer channel here; render it so the user
			// can reply in their next prompt.
			if b.Name == "AskUserQuestion" {
				if q := formatAskUserQuestion(b.Input); q != "" {
					out = append(out, agent.Event{Type: agent.EventMessage, Text: q})
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
