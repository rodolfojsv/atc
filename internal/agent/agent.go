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
)

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
}

// SessionSpec configures a new or resumed session.
type SessionSpec struct {
	SessionID  string // resume target; "" for a new session
	WorkingDir string
	Model      string
	// Approval is config.ApprovalPrompt or ApprovalAllowAll. Backends
	// without runtime permission callbacks map it to their own native
	// permission mechanism.
	Approval     string
	OnEvent      func(Event)
	OnPermission PermissionFunc
}

type Session interface {
	ID() string
	// Send submits a user prompt; the response streams via OnEvent.
	Send(ctx context.Context, prompt string) error
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
