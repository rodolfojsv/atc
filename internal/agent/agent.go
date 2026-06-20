// Package agent defines the backend-neutral contract between atc's
// supervisor and an AI agent runtime. Adapters live in subpackages:
// copilotagent (GitHub Copilot SDK) and claudeagent (Claude Code CLI).
package agent

import (
	"context"
	"encoding/json"
	"strings"
)

// Decision answers a runtime permission request.
type Decision int

const (
	Deny Decision = iota
	ApproveOnce
	// ApproveSession asks the supervisor to remember a session-scoped
	// rule for similar requests; backends treat it like ApproveOnce.
	ApproveSession
	Cancel
)

// PermissionRequest is a runtime approval ask, normalized across
// backends. Command and Path feed the deterministic deny-list.
type PermissionRequest struct {
	Kind    string   // shell | write | read | url | mcp | other
	Command string   // full shell command text, when Kind == "shell"
	Path    string   // file path, when Kind is "read"/"write"
	Summary string   // one line for the board
	Detail  []string // full text for the approval modal
}

// PermissionFunc blocks until a decision is made; feedback explains a
// denial to the agent.
type PermissionFunc func(req PermissionRequest) (Decision, string)

// Question is an agent asking the user to choose or answer — normalized
// across backends (Copilot's ask_user / UserInputRequest). The user's
// answer is fed back to the backend. Claude Code's headless CLI can't
// take an answer for its AskUserQuestion tool, so that backend renders
// the question instead of calling this.
type Question struct {
	Prompt        string   // the question text
	Options       []string // suggested choices (may be empty)
	OptionDetails []string // per-option descriptions, parallel to Options (entries may be ""); nil if none
	AllowFreeform bool     // whether a free-text answer is allowed
	MultiSelect   bool     // the user may pick several options; the answer is the chosen set
}

// QuestionFunc blocks until the user answers, the agent withdraws the
// question (cancel is closed — e.g. the on-screen picker vanished because
// the user cleared it by hand), or the session is aborted. ok=false means
// no answer; the backend should treat it as cancelled. cancel may be nil
// for backends that never withdraw a question.
type QuestionFunc func(q Question, cancel <-chan struct{}) (answer string, ok bool)

type EventType int

const (
	EventTurnStart EventType = iota
	EventIntent
	EventTextDelta
	EventMessage // complete assistant message (markdown)
	EventUserMessage
	EventToolStart
	EventToolFailed
	EventIdle  // turn finished
	EventError // fatal session error
	EventContext
	EventUsage
	EventLimits // account rate-limit snapshot (Claude /usage scrape; Copilot quota)
)

var eventTypeNames = [...]string{
	"turn_start", "intent", "text_delta", "message", "user_message",
	"tool_start", "tool_failed", "idle", "error", "context", "usage", "limits",
}

func (t EventType) String() string {
	if int(t) < len(eventTypeNames) {
		return eventTypeNames[t]
	}
	return "unknown"
}

// Event is the normalized stream a session emits, and the unit of
// transcript history replay.
type Event struct {
	Type EventType
	Text string // delta, message, intent, tool summary, or error text

	ErrType string // EventError

	// EventContext
	CurrentTokens, TokenLimit int64

	// EventUsage (zero fields = not reported by this backend)
	InputTokens, OutputTokens int64
	CostUSD                   float64 // estimated dollars (Claude Code)
	NanoAiu                   float64 // billed AI credits ×1e-9 (Copilot)
	Model                     string

	// EventLimits (best-effort: scraped from Claude's /usage overlay, or
	// derived from Copilot's per-turn account quota snapshots)
	LimitWindows []LimitWindow // every "Current …" window reported (session, weekly, …)
	LimitText    string        // raw overlay text, surfaced verbatim too
}

// LimitWindow is one account rate-limit window from Claude's /usage, e.g.
// "week (all models)" at 36%, resetting at a given time.
type LimitWindow struct {
	Label  string  // window name, e.g. "session", "week (all models)"
	Pct    float64 // 0..100 used
	Resets string  // human reset hint, e.g. "resets Jun 20, 2:59pm"
	Used   int64   // requests consumed this window (0 when not reported)
	Max    int64   // entitlement cap (0 when unknown or unlimited)
}

// AgentDef is a backend-neutral custom-agent definition that atc injects
// into a session at launch (so the agent need not live in the repo). The
// supervisor builds these from config.Agents; each adapter maps them to
// its backend (Copilot CustomAgentConfig, Claude --agents JSON).
type AgentDef struct {
	Name        string
	Description string
	Prompt      string
	Tools       []string // empty = all tools (tool names are backend-specific)
	Model       string
}

// SessionSpec configures a new or resumed session.
type SessionSpec struct {
	SessionID  string // resume target; "" for a new session
	WorkingDir string
	Model      string
	// Agents are custom agents to make available in the session, injected
	// at launch rather than read from the repo. Agent names the one to
	// activate as the primary persona (it must match an Agents entry);
	// empty leaves the backend's default agent in charge.
	Agents []AgentDef
	Agent  string
	// Approval is config.ApprovalPrompt or ApprovalAllowAll. Backends
	// without runtime permission callbacks map it to their own native
	// permission mechanism.
	Approval string
	// ReadOnly runs the session in the backend's plan/read-only mode:
	// the agent can inspect but not modify anything.
	ReadOnly     bool
	OnEvent      func(Event)
	OnPermission PermissionFunc
	// OnQuestion answers an agent's ask-the-user request (Copilot's
	// ask_user tool). Backends that can't take an answer (Claude headless)
	// leave it unused and render the question instead.
	OnQuestion QuestionFunc
}

// Attachment is a file (typically an image) sent alongside a prompt.
type Attachment struct {
	Name      string // original filename, for transcript display
	MediaType string // e.g. "image/png"
	Data      []byte
}

// IsImage reports whether the attachment can go into an image content
// block (the API accepts these four media types).
func (a Attachment) IsImage() bool {
	switch a.MediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// AttachmentSender is optionally implemented by sessions whose backend
// can inline attachments into the model context (Claude's stream-JSON
// image blocks). For backends without it, the supervisor saves
// attachments to disk and references them by path in the prompt.
type AttachmentSender interface {
	SendWithAttachments(ctx context.Context, prompt string, atts []Attachment) error
}

// SlashCommand is one invocable "/" command or skill the backend has
// loaded for a session — used to populate prompt-box completion. Name
// carries no leading slash.
type SlashCommand struct {
	Name        string
	Description string
}

// CommandLister is optionally implemented by sessions whose backend can
// enumerate the slash commands and skills available in the running
// session (Claude reports them in its init event; Copilot via an RPC).
// The list is authoritative for "/" completion — repo + user + built-in
// + plugin — beyond what a filesystem scan can see. Best-effort: nil
// when the backend can't report yet (e.g. process not started).
type CommandLister interface {
	ListCommands(ctx context.Context) []SlashCommand
}

type Session interface {
	ID() string
	// Send submits a user prompt; the response streams via OnEvent.
	Send(ctx context.Context, prompt string) error
	// SetModel switches the model for subsequent turns.
	SetModel(ctx context.Context, model string) error
	// History returns the persisted transcript as replayable events,
	// oldest first. Best-effort: empty when unavailable.
	History(ctx context.Context) []Event
	Abort(ctx context.Context) error
	Close() error
}

type Backend interface {
	Name() string
	NewSession(ctx context.Context, spec SessionSpec) (Session, error)
	// ResumeSession reattaches to spec.SessionID.
	ResumeSession(ctx context.Context, spec SessionSpec) (Session, error)
	Stop() error
}

// ResumeReady is an optional Session capability used on atc restart.
// Backends that keep a live session across restarts (e.g. the Claude
// tmux backend) implement it so the supervisor restores working/done
// from the real session state instead of assuming a finished turn.
type ResumeReady interface {
	// Reattach probes the live session, starts streaming any
	// in-progress turn via OnEvent, and reports whether a turn is
	// running right now. A working session keeps streaming until it
	// goes quiet, at which point it emits EventIdle as usual.
	Reattach(ctx context.Context) (working bool)
}

// ToolSummary turns a tool invocation into a short human line like
// "bash · go test ./..." instead of raw JSON arguments. Shared by
// adapters so transcripts look the same across backends.
func ToolSummary(name string, args any) string {
	m, ok := args.(map[string]any)
	if !ok {
		return name
	}
	for _, k := range []string{"command", "cmd", "path", "file_path", "filePath", "absolute_path", "pattern", "query", "url", "description"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			v = strings.TrimSpace(v)
			if i := strings.IndexByte(v, '\n'); i >= 0 {
				v = v[:i] + " …"
			}
			return name + " · " + Truncate(v, 90)
		}
	}
	return name
}

// SummarizeJSON renders any value as truncated compact JSON, for
// argument display in permission details.
func SummarizeJSON(v any, n int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return Truncate(string(b), n)
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
