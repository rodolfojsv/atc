package supervisor

import (
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusIdle     Status = "idle"    // never prompted yet
	StatusWorking  Status = "working"
	StatusWaiting  Status = "waiting" // blocked on a permission request
	StatusDone     Status = "done"    // finished a turn, awaiting next prompt
	StatusError    Status = "error"
	StatusClosed   Status = "closed"
)

// Usage accumulates token and cost numbers from SDK usage events.
type Usage struct {
	InputTokens   int64
	OutputTokens  int64
	CurrentTokens int64 // context window fill
	TokenLimit    int64
	Cost          float64 // sum of per-call billing multipliers (experimental SDK field)
	Model         string
}

// Permission is a pending approval surfaced to the UI. The SDK's
// permission handler goroutine blocks on respond until the user (or a
// kill/shutdown path) answers exactly once.
type Permission struct {
	Kind    string
	Summary string   // one line for the board
	Detail  []string // full text for the modal
	respond chan rpc.PermissionDecision
}

// PermissionView is the UI-safe copy of a pending permission.
type PermissionView struct {
	Kind    string
	Summary string
	Detail  []string
}

const maxTranscriptLines = 5000

type Session struct {
	mu sync.Mutex

	// Name, Repo, Preset and Created are immutable after creation.
	// Dir/Worktree/Branch are set once by the launch goroutine (under
	// mu) when a worktree is created.
	Name     string
	Repo     string // original repo path
	Dir      string // directory the agent runs in (worktree if one was made)
	Worktree string // worktree path, "" if none
	Branch   string // worktree branch
	Preset   string
	Created  time.Time

	id          string
	status      Status
	intent      string // short activity description from assistant.intent
	errMsg      string
	transcript  []string
	streamBuf   string // in-flight assistant message (deltas)
	usage       Usage
	pending     *Permission
	autoApprove bool // user flipped this session to allow-all at runtime
	everWorked  bool

	sdk *copilot.Session
}

// SessionView is a consistent snapshot for rendering the board.
type SessionView struct {
	Name, Dir, Repo, Worktree, Branch, Preset string
	Status                                    Status
	Intent, Err, LastLine                     string
	Usage                                     Usage
	Pending                                   *PermissionView
	AutoApprove                               bool
	Created                                   time.Time
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := SessionView{
		Name: s.Name, Dir: s.Dir, Repo: s.Repo, Worktree: s.Worktree,
		Branch: s.Branch, Preset: s.Preset, Status: s.status,
		Intent: s.intent, Err: s.errMsg, Usage: s.usage,
		AutoApprove: s.autoApprove, Created: s.Created,
	}
	if s.pending != nil {
		v.Pending = &PermissionView{Kind: s.pending.Kind, Summary: s.pending.Summary, Detail: append([]string(nil), s.pending.Detail...)}
	}
	v.LastLine = s.lastLineLocked()
	return v
}

// Transcript returns a copy of the transcript including any in-flight
// streamed text.
func (s *Session) Transcript() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.transcript...)
	if s.streamBuf != "" {
		out = append(out, strings.Split(s.streamBuf, "\n")...)
	}
	return out
}

// Respond resolves the pending permission request, if any.
func (s *Session) Respond(decision rpc.PermissionDecision) {
	s.mu.Lock()
	p := s.pending
	s.mu.Unlock()
	if p != nil {
		select {
		case p.respond <- decision:
		default: // already answered
		}
	}
}

// SetAutoApprove flips runtime allow-all for this session (deny-list
// still applies) and unblocks a pending request if one is waiting.
func (s *Session) SetAutoApprove(on bool) {
	s.mu.Lock()
	s.autoApprove = on
	s.mu.Unlock()
	if on {
		s.Respond(&rpc.PermissionDecisionApproveOnce{})
	}
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

func (s *Session) lastLineLocked() string {
	if s.streamBuf != "" {
		lines := strings.Split(strings.TrimRight(s.streamBuf, "\n"), "\n")
		if l := lines[len(lines)-1]; strings.TrimSpace(l) != "" {
			return l
		}
	}
	for i := len(s.transcript) - 1; i >= 0; i-- {
		if strings.TrimSpace(s.transcript[i]) != "" {
			return s.transcript[i]
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
	s.mu.Unlock()
}

func (s *Session) appendStream(delta string) {
	s.mu.Lock()
	s.streamBuf += delta
	s.mu.Unlock()
}

// finishMessage replaces the streamed buffer with the authoritative
// full message content from assistant.message.
func (s *Session) finishMessage(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamBuf = ""
	if content == "" {
		return
	}
	s.transcript = append(s.transcript, strings.Split(strings.TrimRight(content, "\n"), "\n")...)
	s.transcript = append(s.transcript, "")
	s.trimLocked()
}

func (s *Session) appendLine(line string) {
	s.mu.Lock()
	// Flush any in-flight stream first so ordering stays sane.
	if s.streamBuf != "" {
		s.transcript = append(s.transcript, strings.Split(strings.TrimRight(s.streamBuf, "\n"), "\n")...)
		s.streamBuf = ""
	}
	s.transcript = append(s.transcript, line)
	s.trimLocked()
	s.mu.Unlock()
}

func (s *Session) trimLocked() {
	if n := len(s.transcript); n > maxTranscriptLines {
		s.transcript = s.transcript[n-maxTranscriptLines:]
	}
}

func (s *Session) setPending(p *Permission) {
	s.mu.Lock()
	s.pending = p
	s.status = StatusWaiting
	s.mu.Unlock()
}

func (s *Session) clearPending() {
	s.mu.Lock()
	s.pending = nil
	if s.status == StatusWaiting {
		s.status = StatusWorking
	}
	s.mu.Unlock()
}

func (s *Session) sdkSession() *copilot.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sdk
}

func (s *Session) updateContext(current, limit int64) {
	s.mu.Lock()
	s.usage.CurrentTokens = current
	s.usage.TokenLimit = limit
	s.mu.Unlock()
}

func (s *Session) addUsage(d *rpc.AssistantUsageData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.InputTokens != nil {
		s.usage.InputTokens += *d.InputTokens
	}
	if d.OutputTokens != nil {
		s.usage.OutputTokens += *d.OutputTokens
	}
	if d.Cost != nil {
		s.usage.Cost += *d.Cost
	}
	s.usage.Model = d.Model
}
