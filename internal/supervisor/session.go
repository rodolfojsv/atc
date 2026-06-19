package supervisor

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusIdle     Status = "idle" // never prompted yet
	StatusWorking  Status = "working"
	StatusWaiting  Status = "waiting" // blocked on a permission request
	StatusDone     Status = "done"    // finished a turn, awaiting next prompt
	StatusError    Status = "error"
	StatusClosed   Status = "closed"
)

// EntryKind classifies transcript entries so the UI can give the
// assistant's analysis visual priority over tool noise.
type EntryKind int

const (
	EntryUser      EntryKind = iota // a prompt the user sent
	EntryAssistant                  // assistant text (markdown)
	EntryTool                       // a tool/command invocation
	EntrySystem                     // atc-side notices (starting, approvals…)
	EntryError                      // failures of any origin
)

// Entry is one transcript block. Partial marks an assistant message
// still streaming in.
type Entry struct {
	Kind    EntryKind
	Text    string
	Partial bool
	// Attachments are files the user sent with this prompt, persisted
	// under the session's .atc-attachments dir so the UI can show them
	// (and removed when the session is killed). Only set on EntryUser.
	Attachments []EntryAttachment
}

// EntryAttachment points at a saved attachment by its path relative to
// the session's working directory, with enough metadata for the UI to
// render it (thumbnail for images, a named link otherwise).
type EntryAttachment struct {
	Name      string // original filename, for display
	MediaType string // e.g. "image/png"
	Path      string // relative to the session dir, e.g. .atc-attachments/…
}

// Usage accumulates token and cost numbers from backend usage events.
type Usage struct {
	InputTokens   int64
	OutputTokens  int64
	CurrentTokens int64 // context window fill
	TokenLimit    int64
	CostUSD       float64 // estimated dollars (Claude Code)
	NanoAiu       float64 // billed AI credits ×1e-9 (Copilot; 0 when unavailable)
	Model         string
}

// Limits is a best-effort account rate-limit snapshot. For Claude it's scraped
// from the /usage overlay (a point-in-time reading, refreshed only when the
// user runs /usage); for Copilot it's derived from the account quota snapshots
// that ride each turn's usage event (refreshed automatically). AsOf records
// when it was captured. Windows holds every reported window (session, weekly,
// per-model) so the UI can show them all.
type Limits struct {
	Windows []agent.LimitWindow
	Text    string    // raw overlay text
	AsOf    time.Time // when this reading was captured
}

type permissionAnswer struct {
	decision agent.Decision
	feedback string
}

// Permission is a pending approval surfaced to the UI. The backend's
// permission goroutine blocks on respond until the user (or a
// kill/shutdown path) answers exactly once.
type Permission struct {
	Kind    string
	Summary string   // one line for the board
	Detail  []string // full text for the modal
	respond chan permissionAnswer
}

// PermissionView is the UI-safe copy of a pending permission.
type PermissionView struct {
	Kind    string
	Summary string
	Detail  []string
}

// QuestionView is the UI-safe copy of a pending agent question. When set,
// the session is waiting for the user's next message as the answer.
type QuestionView struct {
	Prompt        string
	Options       []string
	OptionDetails []string // per-option descriptions, parallel to Options
	AllowFreeform bool
}

const (
	maxTranscriptEntries = 1500
	maxHistory           = 100
)

type Session struct {
	mu sync.Mutex

	// Name, Repo, Backend, Preset and Created are immutable after
	// creation. Dir/Worktree/Branch are set once by the launch
	// goroutine (under mu) when a worktree is created.
	Name     string
	Repo     string // original repo path
	Dir      string // directory the agent runs in (worktree if one was made)
	Worktree string // worktree path, "" if none
	Branch   string // worktree branch
	Backend  string // "copilot" | "claude"
	Preset   string
	Agent    string // custom agent tagged onto the session ("" = backend default)
	ReadOnly bool   // backend plan mode: inspect but never modify
	Model    string // configured model ("" = backend default); usage reports the actual one
	Created  time.Time
	// ScheduleName is the name of the schedule that launched this session,
	// "" for manually started ones. It hides a finished scheduled run from
	// the main board (it lives in the Scheduled section instead) and scopes
	// retention cleanup. Immutable after creation.
	ScheduleName string

	// BaseBranch/BaseCommit record where the worktree branched off,
	// for diff review and merge-back.
	BaseBranch string
	BaseCommit string

	id          string
	status      Status
	intent      string // short activity description from intent events
	errMsg      string
	transcript  []Entry
	streamBuf   string // in-flight assistant message (deltas)
	shownStream string // text from the in-flight message already committed to the transcript (flushed from streamBuf by an interleaved entry), so finishMessage won't re-append it
	usage       Usage
	pending     []*Permission // FIFO queue; index 0 is surfaced in the UI
	question    *agent.Question
	questionCh  chan string // the next user message resolves a pending question
	autoApprove bool        // user flipped this session to allow-all at runtime
	everWorked  bool
	lastEvent   time.Time // last backend event; exposes stalls
	history     []string  // prompts sent, for arrow-up recall
	approvals   []approvalRule

	// Organization metadata (user-set; affects board layout only):
	// pinned floats a session to the top, category groups it.
	pinned   bool
	category string

	// createdBy is the opaque per-device clientId of whoever started the
	// session (web/app). Empty for TUI- and scheduler-started sessions.
	// Used to scope notifications to "my" sessions; never shown as text.
	createdBy string
	// notifyTopic is the ntfy topic of the device that started the
	// session, so push notifications reach that phone. Empty = fall back
	// to the configured default topic (or no push).
	notifyTopic string

	ag agent.Session
}

// SessionView is a consistent snapshot for rendering the board.
type SessionView struct {
	Name, Dir, Repo, Worktree, Branch, Backend, Preset string
	BaseBranch                                         string
	Model                                              string // best known model: actual from usage, else configured, else ""
	Status                                             Status
	Intent, Err, LastLine                              string
	Usage                                              Usage
	Pending                                            *PermissionView
	PendingCount                                       int // total queued (incl. the surfaced one)
	Question                                           *QuestionView
	AutoApprove                                        bool
	ReadOnly                                           bool
	Pinned                                             bool
	Category                                           string
	CreatedBy                                          string // per-device clientId of the creator; "" for TUI/scheduler
	NotifyTopic                                        string // ntfy topic of the creator's device; "" = none
	ScheduleName                                       string // schedule that launched this session; "" if started manually
	Created                                            time.Time
	SinceEvent                                         time.Duration // time since the last backend event (0 = none yet)
}

// approvalRule is a session-scoped "always allow this" rule created by
// the 's' answer in the permission modal.
type approvalRule struct {
	kind  string
	match string // shell: first command word; mcp: server/tool; others: ""
}

func ruleFor(req agent.PermissionRequest) approvalRule {
	r := approvalRule{kind: req.Kind}
	switch req.Kind {
	case "shell":
		if f := strings.Fields(req.Command); len(f) > 0 {
			r.match = f[0]
		}
	case "mcp":
		r.match = req.Summary
	}
	return r
}

// Label describes the rule for the transcript.
func (r approvalRule) label() string {
	if r.match != "" {
		return r.kind + " " + r.match
	}
	return "all " + r.kind + "s"
}

func (s *Session) addApproval(r approvalRule) {
	s.mu.Lock()
	s.approvals = append(s.approvals, r)
	s.mu.Unlock()
}

func (s *Session) approvedByRule(req agent.PermissionRequest) bool {
	want := ruleFor(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.approvals {
		if r == want {
			return true
		}
	}
	return false
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := SessionView{
		Name: s.Name, Dir: s.Dir, Repo: s.Repo, Worktree: s.Worktree,
		Branch: s.Branch, Backend: s.Backend, Preset: s.Preset, Status: s.status,
		BaseBranch: s.BaseBranch, Intent: s.intent, Err: s.errMsg, Usage: s.usage,
		AutoApprove: s.autoApprove, ReadOnly: s.ReadOnly, Created: s.Created,
		Pinned: s.pinned, Category: s.category, CreatedBy: s.createdBy,
		NotifyTopic: s.notifyTopic, ScheduleName: s.ScheduleName,
	}
	if len(s.pending) > 0 {
		head := s.pending[0]
		v.Pending = &PermissionView{Kind: head.Kind, Summary: head.Summary, Detail: append([]string(nil), head.Detail...)}
		v.PendingCount = len(s.pending)
	}
	if s.question != nil {
		v.Question = &QuestionView{
			Prompt: s.question.Prompt, AllowFreeform: s.question.AllowFreeform,
			Options:       append([]string(nil), s.question.Options...),
			OptionDetails: append([]string(nil), s.question.OptionDetails...),
		}
	}
	v.Model = s.usage.Model
	if v.Model == "" {
		v.Model = s.Model
	}
	v.LastLine = s.lastLineLocked()
	if !s.lastEvent.IsZero() {
		v.SinceEvent = time.Since(s.lastEvent)
	}
	return v
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastEvent = time.Now()
	s.mu.Unlock()
}

// Transcript returns a copy of the transcript; an in-flight assistant
// message appears as a final Partial entry.
func (s *Session) Transcript() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Entry(nil), s.transcript...)
	if s.streamBuf != "" {
		out = append(out, Entry{Kind: EntryAssistant, Text: s.streamBuf, Partial: true})
	}
	return out
}

// Respond resolves the currently surfaced (head) permission request,
// if any, popping it synchronously so a rapid second Respond reaches
// the next request instead of re-hitting an answered one.
func (s *Session) Respond(decision agent.Decision, feedback string) {
	s.mu.Lock()
	var p *Permission
	if len(s.pending) > 0 {
		p = s.pending[0]
		s.pending = s.pending[1:]
		if len(s.pending) == 0 && s.status == StatusWaiting {
			s.status = StatusWorking
		}
	}
	s.mu.Unlock()
	if p != nil {
		p.respond <- permissionAnswer{decision: decision, feedback: feedback}
	}
}

// RespondAll resolves every queued permission request — used by
// auto-approve, kill, and shutdown so no handler is left blocked.
func (s *Session) RespondAll(decision agent.Decision, feedback string) {
	s.mu.Lock()
	ps := s.pending
	s.pending = nil
	if s.status == StatusWaiting {
		s.status = StatusWorking
	}
	s.mu.Unlock()
	for _, p := range ps {
		p.respond <- permissionAnswer{decision: decision, feedback: feedback}
	}
}

// askQuestion records a pending agent question and returns the channel
// the answer (the user's next message) will arrive on. A closed channel
// means the question was cancelled.
func (s *Session) askQuestion(q agent.Question) chan string {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.question = &q
	s.questionCh = ch
	s.mu.Unlock()
	return ch
}

// HasQuestion reports whether the session is waiting for the user to
// answer an agent question (so the next message is routed as the answer).
func (s *Session) HasQuestion() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.question != nil
}

// answerQuestion resolves a pending question with the user's text. A
// bare option number ("2") is mapped to that option's text so the
// backend gets the choice it offered.
func (s *Session) answerQuestion(answer string) bool {
	s.mu.Lock()
	ch := s.questionCh
	if s.question != nil {
		answer = resolveChoice(answer, s.question.Options)
	}
	s.question, s.questionCh = nil, nil
	s.mu.Unlock()
	if ch != nil {
		ch <- answer
		return true
	}
	return false
}

func resolveChoice(answer string, options []string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(answer)); err == nil && n >= 1 && n <= len(options) {
		return options[n-1]
	}
	return answer
}

// cancelQuestion unblocks a waiting question handler with no answer.
func (s *Session) cancelQuestion() {
	s.mu.Lock()
	ch := s.questionCh
	s.question, s.questionCh = nil, nil
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// SetAutoApprove flips runtime allow-all for this session (deny-list
// still applies) and unblocks a pending request if one is waiting.
func (s *Session) SetAutoApprove(on bool) {
	s.mu.Lock()
	s.autoApprove = on
	s.mu.Unlock()
	if on {
		s.RespondAll(agent.ApproveOnce, "")
	}
}

// setPinned / setCategory update board-organization metadata under the
// session lock. Callers (the supervisor) persist and wake the UI after.
func (s *Session) setPinned(on bool) {
	s.mu.Lock()
	s.pinned = on
	s.mu.Unlock()
}

func (s *Session) setCategory(category string) {
	s.mu.Lock()
	s.category = category
	s.mu.Unlock()
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Session) Active() bool {
	switch s.Status() {
	case StatusClosed, StatusError:
		return false
	}
	return true
}

func (s *Session) addHistory(text string) {
	s.mu.Lock()
	s.history = append(s.history, text)
	if n := len(s.history); n > maxHistory {
		s.history = s.history[n-maxHistory:]
	}
	s.mu.Unlock()
}

// History returns the prompts sent to this session, oldest first.
func (s *Session) History() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.history...)
}

func (s *Session) lastLineLocked() string {
	if s.streamBuf != "" {
		lines := strings.Split(strings.TrimRight(s.streamBuf, "\n"), "\n")
		if l := lines[len(lines)-1]; strings.TrimSpace(l) != "" {
			return l
		}
	}
	for i := len(s.transcript) - 1; i >= 0; i-- {
		text := strings.TrimRight(s.transcript[i].Text, "\n")
		if j := strings.LastIndexByte(text, '\n'); j >= 0 {
			text = text[j+1:]
		}
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func (s *Session) setStatus(st Status) {
	s.mu.Lock()
	s.status = st
	if st == StatusWorking {
		s.everWorked = true
	}
	s.mu.Unlock()
}

func (s *Session) setIntent(intent string) {
	s.mu.Lock()
	s.intent = intent
	s.mu.Unlock()
}

func (s *Session) setError(msg string) {
	s.mu.Lock()
	s.status = StatusError
	s.errMsg = msg
	s.transcript = append(s.transcript, Entry{Kind: EntryError, Text: msg})
	s.mu.Unlock()
}

func (s *Session) appendStream(delta string) {
	s.mu.Lock()
	s.streamBuf += delta
	s.mu.Unlock()
}

// finishMessage replaces the streamed buffer with the authoritative
// full message content.
//
// If an interleaved entry (a tool start, a tool failure, …) flushed part
// of this same message out of streamBuf before the authoritative content
// arrived, that prefix is already in the transcript; the backend's full
// content repeats it. Append only the portion not yet shown so the text
// isn't duplicated. This guard is backend-neutral: in the normal ordering
// (the message arrives before any tool, so nothing was flushed mid-stream)
// shownStream is empty and the full content is appended unchanged.
func (s *Session) finishMessage(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamBuf = ""
	shown := strings.TrimRight(s.shownStream, "\n")
	s.shownStream = ""
	if content == "" {
		return
	}
	if shown != "" {
		if strings.TrimRight(content, "\n") == shown {
			return // the whole message was already committed via flushes
		}
		if rest := strings.TrimPrefix(content, shown); rest != content {
			content = strings.TrimLeft(rest, "\n")
			if strings.TrimSpace(content) == "" {
				return
			}
		}
	}
	s.transcript = append(s.transcript, Entry{Kind: EntryAssistant, Text: strings.TrimRight(content, "\n")})
	s.trimLocked()
}

func (s *Session) appendEntry(kind EntryKind, text string) {
	s.appendEntryWith(Entry{Kind: kind, Text: text})
}

// appendEntryWith appends a fully-formed entry (used when it carries more
// than text, e.g. a user prompt with attachments).
func (s *Session) appendEntryWith(e Entry) {
	s.mu.Lock()
	// Flush any in-flight stream first so ordering stays sane. Remember the
	// flushed text so the authoritative finishMessage doesn't re-append it.
	if s.streamBuf != "" {
		s.transcript = append(s.transcript, Entry{Kind: EntryAssistant, Text: strings.TrimRight(s.streamBuf, "\n")})
		s.shownStream += s.streamBuf
		s.streamBuf = ""
	}
	// A user prompt opens a new turn; any earlier flushed assistant text is
	// final and must not be deduped against the next message.
	if e.Kind == EntryUser {
		s.shownStream = ""
	}
	s.transcript = append(s.transcript, e)
	s.trimLocked()
	s.mu.Unlock()
}

func (s *Session) trimLocked() {
	if n := len(s.transcript); n > maxTranscriptEntries {
		s.transcript = s.transcript[n-maxTranscriptEntries:]
	}
}

func (s *Session) enqueuePending(p *Permission) {
	s.mu.Lock()
	s.pending = append(s.pending, p)
	s.status = StatusWaiting
	s.mu.Unlock()
}

func (s *Session) agentSession() agent.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ag
}

// SlashCommands returns the invocable "/" commands and skills the
// backend has loaded for this session (for prompt-box completion), or
// nil if the backend can't report them. Best-effort and may hit the
// backend, so callers should treat it as advisory.
func (s *Session) SlashCommands(ctx context.Context) []agent.SlashCommand {
	ag := s.agentSession()
	if cl, ok := ag.(agent.CommandLister); ok {
		return cl.ListCommands(ctx)
	}
	return nil
}

func (s *Session) updateContext(current, limit int64) {
	s.mu.Lock()
	s.usage.CurrentTokens = current
	s.usage.TokenLimit = limit
	s.mu.Unlock()
}

func (s *Session) addUsage(e agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.InputTokens += e.InputTokens
	s.usage.OutputTokens += e.OutputTokens
	s.usage.CostUSD += e.CostUSD
	s.usage.NanoAiu += e.NanoAiu
	if e.Model != "" {
		s.usage.Model = e.Model
	}
}
