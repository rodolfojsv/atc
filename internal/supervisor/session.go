package supervisor

import (
	"strings"
	"sync"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
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
	Created  time.Time

	id          string
	status      Status
	intent      string // short activity description from intent events
	errMsg      string
	transcript  []Entry
	streamBuf   string // in-flight assistant message (deltas)
	usage       Usage
	pending     *Permission
	autoApprove bool // user flipped this session to allow-all at runtime
	everWorked  bool
	history     []string // prompts sent, for arrow-up recall

	ag agent.Session
}

// SessionView is a consistent snapshot for rendering the board.
type SessionView struct {
	Name, Dir, Repo, Worktree, Branch, Backend, Preset string
	Status                                             Status
	Intent, Err, LastLine                              string
	Usage                                              Usage
	Pending                                            *PermissionView
	AutoApprove                                        bool
	Created                                            time.Time
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := SessionView{
		Name: s.Name, Dir: s.Dir, Repo: s.Repo, Worktree: s.Worktree,
		Branch: s.Branch, Backend: s.Backend, Preset: s.Preset, Status: s.status,
		Intent: s.intent, Err: s.errMsg, Usage: s.usage,
		AutoApprove: s.autoApprove, Created: s.Created,
	}
	if s.pending != nil {
		v.Pending = &PermissionView{Kind: s.pending.Kind, Summary: s.pending.Summary, Detail: append([]string(nil), s.pending.Detail...)}
	}
	v.LastLine = s.lastLineLocked()
	return v
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

// Respond resolves the pending permission request, if any.
func (s *Session) Respond(decision agent.Decision, feedback string) {
	s.mu.Lock()
	p := s.pending
	s.mu.Unlock()
	if p != nil {
		select {
		case p.respond <- permissionAnswer{decision: decision, feedback: feedback}:
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
		s.Respond(agent.ApproveOnce, "")
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
func (s *Session) finishMessage(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamBuf = ""
	if content == "" {
		return
	}
	s.transcript = append(s.transcript, Entry{Kind: EntryAssistant, Text: strings.TrimRight(content, "\n")})
	s.trimLocked()
}

func (s *Session) appendEntry(kind EntryKind, text string) {
	s.mu.Lock()
	// Flush any in-flight stream first so ordering stays sane.
	if s.streamBuf != "" {
		s.transcript = append(s.transcript, Entry{Kind: EntryAssistant, Text: strings.TrimRight(s.streamBuf, "\n")})
		s.streamBuf = ""
	}
	s.transcript = append(s.transcript, Entry{Kind: kind, Text: text})
	s.trimLocked()
	s.mu.Unlock()
}

func (s *Session) trimLocked() {
	if n := len(s.transcript); n > maxTranscriptEntries {
		s.transcript = s.transcript[n-maxTranscriptEntries:]
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

func (s *Session) agentSession() agent.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ag
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
