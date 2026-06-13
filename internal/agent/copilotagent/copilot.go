// Package copilotagent adapts the GitHub Copilot SDK (Go) to atc's
// backend-neutral agent interface.
package copilotagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/rodolfojsv/atc/internal/agent"
)

type Backend struct {
	client *copilot.Client
}

// New creates the backend; the underlying CLI server process is
// spawned lazily on the first session. sdkLogLevel, when non-empty,
// enables the Copilot runtime's own diagnostics ("info", "debug", …) —
// invaluable for hangs inside the runtime that atc only sees as
// "events stopped".
func New(sdkLogLevel string) *Backend {
	var opts *copilot.ClientOptions
	if sdkLogLevel != "" {
		opts = &copilot.ClientOptions{LogLevel: sdkLogLevel}
	}
	return &Backend{client: copilot.NewClient(opts)}
}

func (b *Backend) Name() string { return "copilot" }

func (b *Backend) Stop() error { return b.client.Stop() }

func (b *Backend) NewSession(ctx context.Context, spec agent.SessionSpec) (agent.Session, error) {
	sdk, err := b.client.CreateSession(ctx, &copilot.SessionConfig{
		Model:               spec.Model,
		WorkingDirectory:    spec.WorkingDir,
		ClientName:          "atc",
		Streaming:           copilot.Bool(true),
		OnPermissionRequest: permissionHandler(spec.OnPermission),
		OnUserInputRequest:  userInputHandler(spec.OnQuestion),
	})
	if err != nil {
		return nil, err
	}
	sdk.On(eventTranslator(spec.OnEvent))
	return &session{sdk: sdk, readOnly: spec.ReadOnly}, nil
}

func (b *Backend) ResumeSession(ctx context.Context, spec agent.SessionSpec) (agent.Session, error) {
	sdk, err := b.client.ResumeSession(ctx, spec.SessionID, &copilot.ResumeSessionConfig{
		ClientName:          "atc",
		WorkingDirectory:    spec.WorkingDir,
		Model:               spec.Model,
		Streaming:           copilot.Bool(true),
		OnPermissionRequest: permissionHandler(spec.OnPermission),
		OnUserInputRequest:  userInputHandler(spec.OnQuestion),
	})
	if err != nil {
		return nil, err
	}
	// History is read before the live subscription is attached; a
	// freshly resumed session emits nothing until prompted.
	return &session{sdk: sdk, onEvent: spec.OnEvent, readOnly: spec.ReadOnly}, nil
}

type session struct {
	sdk      *copilot.Session
	readOnly bool
	// onEvent is held un-attached on resumed sessions until History()
	// has been replayed; see attachLive.
	onEvent func(agent.Event)
}

func (s *session) ID() string { return s.sdk.SessionID }

func (s *session) attachLive() {
	if s.onEvent != nil {
		s.sdk.On(eventTranslator(s.onEvent))
		s.onEvent = nil
	}
}

func (s *session) Send(ctx context.Context, prompt string) error {
	s.attachLive()
	opts := copilot.MessageOptions{Prompt: prompt}
	if s.readOnly {
		opts.AgentMode = copilot.AgentModePlan
	}
	_, err := s.sdk.Send(ctx, opts)
	return err
}

func (s *session) SetModel(ctx context.Context, model string) error {
	_, err := s.sdk.RPC.Model.SwitchTo(ctx, &rpc.ModelSwitchToRequest{ModelID: model})
	return err
}

func (s *session) Abort(ctx context.Context) error { return s.sdk.Abort(ctx) }

func (s *session) Close() error { return s.sdk.Disconnect() }

// History replays the runtime's persisted event log (experimental SDK
// API — failures just return what was read so far). It also attaches
// the live subscription afterwards on resumed sessions.
func (s *session) History(ctx context.Context) []agent.Event {
	defer s.attachLive()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	scope := rpc.EventsAgentScopePrimary // skip sub-agent chatter
	max := int64(500)
	var cursor *string
	var out []agent.Event
	total := 0
	for total < 10_000 { // hard cap, generous for any real session
		res, err := s.sdk.RPC.EventLog.Read(ctx, &rpc.EventLogReadRequest{
			AgentScope: &scope,
			Cursor:     cursor,
			Max:        &max,
		})
		if err != nil || res == nil {
			return out
		}
		for _, ev := range res.Events {
			total++
			if ev.AgentID != nil {
				continue
			}
			if e, ok := translateData(ev.Data); ok {
				switch e.Type {
				// Live-only noise has no place in a replayed transcript.
				case agent.EventTextDelta, agent.EventIdle, agent.EventTurnStart, agent.EventIntent:
				default:
					out = append(out, e)
				}
			} else if d, isUser := ev.Data.(*rpc.UserMessageData); isUser {
				out = append(out, agent.Event{Type: agent.EventUserMessage, Text: d.Content})
			}
		}
		if !res.HasMore {
			return out
		}
		cursor = &res.Cursor
	}
	return out
}

func eventTranslator(onEvent func(agent.Event)) copilot.SessionEventHandler {
	return func(ev copilot.SessionEvent) {
		if e, ok := translateData(ev.Data); ok {
			onEvent(e)
		}
	}
}

// translateData maps SDK event payloads onto normalized events.
func translateData(data rpc.SessionEventData) (agent.Event, bool) {
	switch d := data.(type) {
	case *rpc.AssistantTurnStartData:
		return agent.Event{Type: agent.EventTurnStart}, true
	case *rpc.AssistantIntentData:
		return agent.Event{Type: agent.EventIntent, Text: d.Intent}, true
	case *rpc.AssistantMessageDeltaData:
		return agent.Event{Type: agent.EventTextDelta, Text: d.DeltaContent}, true
	case *rpc.AssistantMessageData:
		return agent.Event{Type: agent.EventMessage, Text: d.Content}, true
	case *rpc.ToolExecutionStartData:
		return agent.Event{Type: agent.EventToolStart, Text: agent.ToolSummary(d.ToolName, d.Arguments)}, true
	case *rpc.ToolExecutionCompleteData:
		if d.Success {
			return agent.Event{}, false
		}
		msg := "tool call failed"
		if d.Error != nil && d.Error.Message != "" {
			msg = "tool failed: " + d.Error.Message
		}
		return agent.Event{Type: agent.EventToolFailed, Text: msg}, true
	case *rpc.SessionIdleData:
		return agent.Event{Type: agent.EventIdle}, true
	case *rpc.SessionErrorData:
		return agent.Event{Type: agent.EventError, ErrType: d.ErrorType, Text: d.Message}, true
	case *rpc.SessionUsageInfoData:
		return agent.Event{Type: agent.EventContext, CurrentTokens: d.CurrentTokens, TokenLimit: d.TokenLimit}, true
	case *rpc.AssistantUsageData:
		e := agent.Event{Type: agent.EventUsage, Model: d.Model}
		if d.InputTokens != nil {
			e.InputTokens = *d.InputTokens
		}
		if d.OutputTokens != nil {
			e.OutputTokens = *d.OutputTokens
		}
		if d.CopilotUsage != nil {
			e.NanoAiu = d.CopilotUsage.TotalNanoAiu
		}
		return e, true
	}
	return agent.Event{}, false
}

func permissionHandler(onPermission agent.PermissionFunc) copilot.PermissionHandlerFunc {
	if onPermission == nil {
		return nil
	}
	return func(req copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		decision, feedback := onPermission(describeRequest(req))
		switch decision {
		case agent.ApproveOnce, agent.ApproveSession:
			return &rpc.PermissionDecisionApproveOnce{}, nil
		case agent.Cancel:
			return &rpc.PermissionDecisionCancelled{}, nil
		default:
			if feedback == "" {
				feedback = "denied"
			}
			return &rpc.PermissionDecisionReject{Feedback: copilot.String(feedback)}, nil
		}
	}
}

// userInputHandler bridges Copilot's ask_user tool to atc's question
// flow. Registering it is what enables the ask_user tool at all; the
// returned answer is fed back to the agent. A nil onQuestion leaves the
// tool disabled.
func userInputHandler(onQuestion agent.QuestionFunc) copilot.UserInputHandler {
	if onQuestion == nil {
		return nil
	}
	return func(req copilot.UserInputRequest, _ copilot.UserInputInvocation) (copilot.UserInputResponse, error) {
		allowFreeform := req.AllowFreeform == nil || *req.AllowFreeform
		ans, ok := onQuestion(agent.Question{
			Prompt: req.Question, Options: req.Choices, AllowFreeform: allowFreeform,
		})
		if !ok {
			return copilot.UserInputResponse{}, errors.New("question cancelled in atc")
		}
		wasFreeform := true
		for _, c := range req.Choices {
			if c == ans {
				wasFreeform = false
				break
			}
		}
		return copilot.UserInputResponse{Answer: ans, WasFreeform: wasFreeform}, nil
	}
}

func describeRequest(req copilot.PermissionRequest) agent.PermissionRequest {
	switch r := req.(type) {
	case *rpc.PermissionRequestShell:
		detail := []string{"Command:", "  " + r.FullCommandText}
		if r.Intention != "" {
			detail = append(detail, "", "Intention: "+r.Intention)
		}
		if r.Warning != nil && *r.Warning != "" {
			detail = append(detail, "", "⚠ "+*r.Warning)
		}
		return agent.PermissionRequest{Kind: "shell", Command: r.FullCommandText,
			Summary: "run: " + agent.Truncate(r.FullCommandText, 80), Detail: detail}
	case *rpc.PermissionRequestWrite:
		detail := []string{"File: " + r.FileName}
		if r.Intention != "" {
			detail = append(detail, "Intention: "+r.Intention)
		}
		if r.Diff != "" {
			detail = append(detail, "", "Diff:")
			detail = append(detail, strings.Split(agent.Truncate(r.Diff, 4000), "\n")...)
		}
		return agent.PermissionRequest{Kind: "write", Path: r.FileName,
			Summary: "write: " + r.FileName, Detail: detail}
	case *rpc.PermissionRequestRead:
		detail := []string{"Path: " + r.Path}
		if r.Intention != "" {
			detail = append(detail, "Intention: "+r.Intention)
		}
		return agent.PermissionRequest{Kind: "read", Path: r.Path,
			Summary: "read: " + r.Path, Detail: detail}
	case *rpc.PermissionRequestURL:
		detail := []string{"URL: " + r.URL}
		if r.Intention != "" {
			detail = append(detail, "Intention: "+r.Intention)
		}
		return agent.PermissionRequest{Kind: "url",
			Summary: "fetch: " + r.URL, Detail: detail}
	case *rpc.PermissionRequestMCP:
		detail := []string{"MCP server: " + r.ServerName, "Tool: " + r.ToolName}
		if r.Args != nil {
			if s := agent.SummarizeJSON(r.Args, 500); s != "" {
				detail = append(detail, "Args: "+s)
			}
		}
		return agent.PermissionRequest{Kind: "mcp",
			Summary: "mcp: " + r.ServerName + "/" + r.ToolName, Detail: detail}
	default:
		kind := string(req.Kind())
		return agent.PermissionRequest{Kind: kind,
			Summary: kind + " request", Detail: []string{fmt.Sprintf("%+v", req)}}
	}
}
