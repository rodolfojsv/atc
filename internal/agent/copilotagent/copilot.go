// Package copilotagent adapts the GitHub Copilot SDK (Go) to atc's
// backend-neutral agent interface.
package copilotagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/rodolfojsv/atc/internal/agent"
)

// cmdCacheTTL bounds how long a session's slash-command list is reused
// before another Commands.List RPC; commands rarely change mid-session.
const cmdCacheTTL = 30 * time.Second

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
	servers, mcpErr := loadMCPServers()
	sdk, err := b.client.CreateSession(ctx, &copilot.SessionConfig{
		Model:               spec.Model,
		WorkingDirectory:    spec.WorkingDir,
		ClientName:          "atc",
		Streaming:           copilot.Bool(true),
		MCPServers:          servers,
		OnPermissionRequest: permissionHandler(spec.OnPermission),
		OnUserInputRequest:  userInputHandler(spec.OnQuestion),
	})
	if err != nil {
		return nil, err
	}
	sdk.On(eventTranslator(sdk.SessionID, spec.OnEvent))
	reportMCPLoad(spec.OnEvent, servers, mcpErr)
	return &session{sdk: sdk, readOnly: spec.ReadOnly, emit: spec.OnEvent}, nil
}

func (b *Backend) ResumeSession(ctx context.Context, spec agent.SessionSpec) (agent.Session, error) {
	servers, _ := loadMCPServers()
	sdk, err := b.client.ResumeSession(ctx, spec.SessionID, &copilot.ResumeSessionConfig{
		ClientName:          "atc",
		WorkingDirectory:    spec.WorkingDir,
		Model:               spec.Model,
		Streaming:           copilot.Bool(true),
		MCPServers:          servers,
		OnPermissionRequest: permissionHandler(spec.OnPermission),
		OnUserInputRequest:  userInputHandler(spec.OnQuestion),
	})
	if err != nil {
		return nil, err
	}
	// History is read before the live subscription is attached; a
	// freshly resumed session emits nothing until prompted.
	return &session{sdk: sdk, onEvent: spec.OnEvent, emit: spec.OnEvent, readOnly: spec.ReadOnly}, nil
}

type session struct {
	sdk      *copilot.Session
	readOnly bool
	// onEvent is held un-attached on resumed sessions until History()
	// has been replayed; see attachLive.
	onEvent func(agent.Event)
	// emit always points at the session's event sink (unlike onEvent,
	// which is cleared on attach); used to surface slash-command output
	// that doesn't arrive as a normal agent turn.
	emit func(agent.Event)

	cmdMu  sync.Mutex
	cmds   []agent.SlashCommand
	cmdsAt time.Time
}

func (s *session) ID() string { return s.sdk.SessionID }

func (s *session) attachLive() {
	if s.onEvent != nil {
		s.sdk.On(eventTranslator(s.sdk.SessionID, s.onEvent))
		s.onEvent = nil
	}
}

func (s *session) Send(ctx context.Context, prompt string) error {
	s.attachLive()
	// Copilot doesn't expand "/" commands inline in a prompt the way
	// Claude does; they go through a dedicated RPC. So when the prompt is
	// exactly a known slash command, route it through Commands.Invoke and
	// act on the result. Anything else (including a "/path/..." that
	// isn't a real command) is sent as an ordinary prompt.
	if name, input, ok := s.matchSlashCommand(ctx, prompt); ok {
		return s.invokeCommand(ctx, name, input)
	}
	return s.sendPrompt(ctx, prompt)
}

// sendPrompt submits an ordinary agent turn.
func (s *session) sendPrompt(ctx context.Context, prompt string) error {
	opts := copilot.MessageOptions{Prompt: prompt}
	if s.readOnly {
		opts.AgentMode = copilot.AgentModePlan
	}
	_, err := s.sdk.Send(ctx, opts)
	return err
}

// matchSlashCommand reports whether prompt is a "/command [input]" whose
// command name (or alias) is one the session actually has loaded. The
// known-name check avoids hijacking a bare path like "/etc/hosts".
func (s *session) matchSlashCommand(ctx context.Context, prompt string) (name, input string, ok bool) {
	if !strings.HasPrefix(prompt, "/") || strings.HasPrefix(prompt, "//") {
		return "", "", false
	}
	rest := strings.TrimPrefix(prompt, "/")
	first, args, _ := strings.Cut(rest, " ")
	if first == "" {
		return "", "", false
	}
	want := strings.ToLower(first)
	for _, c := range s.ListCommands(ctx) {
		if strings.ToLower(c.Name) == want {
			return c.Name, strings.TrimSpace(args), true
		}
	}
	return "", "", false
}

// invokeCommand runs a slash command via the SDK and reacts to the
// result union: an agent-prompt becomes a normal turn, text/completed
// surface as a transcript message, and a subcommand prompt is reported
// (headless atc can't present an interactive picker).
func (s *session) invokeCommand(ctx context.Context, name, input string) error {
	req := &rpc.CommandsInvokeRequest{Name: name}
	if input != "" {
		req.Input = &input
	}
	res, err := s.sdk.RPC.Commands.Invoke(ctx, req)
	if err != nil {
		return err
	}
	switch r := res.(type) {
	case *rpc.SlashCommandAgentPromptResult:
		// Becomes a real agent turn, which emits its own idle when it
		// finishes — don't emit one here too.
		return s.sendPrompt(ctx, r.Prompt)
	case *rpc.SlashCommandTextResult:
		s.emitMessage(r.Text)
	case *rpc.SlashCommandCompletedResult:
		if r.Message != nil {
			s.emitMessage(*r.Message)
		}
	case *rpc.SlashCommandSelectSubcommandResult:
		var names []string
		for _, o := range r.Options {
			names = append(names, "/"+r.Command+" "+o.Name)
		}
		s.emitMessage("/" + r.Command + " needs a subcommand: " + strings.Join(names, ", "))
	}
	// These results resolve synchronously without a turn lifecycle, so the
	// runtime never sends a turn-end event. Emit idle ourselves; otherwise
	// the supervisor leaves the session stuck "working" (e.g. after /mcp).
	s.emitIdle()
	return nil
}

func (s *session) emitMessage(text string) {
	if text == "" || s.emit == nil {
		return
	}
	s.emit(agent.Event{Type: agent.EventMessage, Text: text})
}

// emitIdle marks the end of a turn that resolved without the runtime's own
// turn-end event (a synchronous slash command), so the supervisor can move
// the session out of "working".
func (s *session) emitIdle() {
	if s.emit != nil {
		s.emit(agent.Event{Type: agent.EventIdle})
	}
}

// ListCommands returns the session's invocable slash commands and skills
// from the Copilot runtime (built-ins, skills, and client/extension
// commands — the .github layout included), cached briefly.
func (s *session) ListCommands(ctx context.Context) []agent.SlashCommand {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if s.cmds != nil && time.Since(s.cmdsAt) < cmdCacheTTL {
		return append([]agent.SlashCommand(nil), s.cmds...)
	}
	yes := true
	list, err := s.sdk.RPC.Commands.List(ctx, &rpc.CommandsListRequest{
		IncludeBuiltins:       &yes,
		IncludeClientCommands: &yes,
		IncludeSkills:         &yes,
	})
	if err != nil || list == nil {
		return append([]agent.SlashCommand(nil), s.cmds...) // keep any prior list
	}
	cmds := make([]agent.SlashCommand, 0, len(list.Commands))
	for _, c := range list.Commands {
		cmds = append(cmds, agent.SlashCommand{Name: c.Name, Description: c.Description})
	}
	s.cmds, s.cmdsAt = cmds, time.Now()
	return append([]agent.SlashCommand(nil), cmds...)
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

func eventTranslator(sessionID string, onEvent func(agent.Event)) copilot.SessionEventHandler {
	return func(ev copilot.SessionEvent) {
		e, ok := translateData(ev.Data)
		traceEvent(sessionID, ev, e, ok)
		if ok {
			onEvent(e)
		}
		// Copilot has no /usage overlay to scrape (the way Claude does);
		// instead it rides account quota snapshots on every usage event.
		// Surface them as a limits snapshot so the account-usage badge
		// works for Copilot too, refreshed automatically each turn.
		if u, isUsage := ev.Data.(*rpc.AssistantUsageData); isUsage {
			if lim, hasLim := limitsFromQuota(u.QuotaSnapshots); hasLim {
				onEvent(lim)
			}
		}
	}
}

// limitsFromQuota turns Copilot's per-quota snapshots into an account
// rate-limit event. Copilot reports the percentage remaining, so we surface
// the used percentage to match Claude's windows. Unlimited or empty
// entitlements carry no meaningful bar and are skipped. Windows are sorted by
// label so the snapshot is stable across the UI's repeated polls (Go map
// iteration order isn't). Returns false when nothing meaningful is present.
func limitsFromQuota(snaps map[string]rpc.AssistantUsageQuotaSnapshot) (agent.Event, bool) {
	if len(snaps) == 0 {
		return agent.Event{}, false
	}
	var windows []agent.LimitWindow
	for key, q := range snaps {
		if q.IsUnlimitedEntitlement {
			continue
		}
		used := 100 - q.RemainingPercentage
		if used < 0 {
			used = 0
		} else if used > 100 {
			used = 100
		}
		w := agent.LimitWindow{Label: quotaLabel(key), Pct: used}
		if q.ResetDate != nil {
			w.Resets = "resets " + q.ResetDate.Local().Format("Jan 2, 3:04pm")
		}
		windows = append(windows, w)
	}
	if len(windows) == 0 {
		return agent.Event{}, false
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Label < windows[j].Label })
	return agent.Event{Type: agent.EventLimits, LimitWindows: windows}, true
}

// quotaLabel makes a Copilot quota key (e.g. "premium_interactions") read
// like the other account limit windows in the UI.
func quotaLabel(key string) string {
	return strings.ReplaceAll(key, "_", " ")
}

// Copilot event tracing: when ATC_COPILOT_TRACE names a writable file,
// every raw SDK event is appended there with its exact content (whitespace
// quoted) and the agent.Event it translated into — the ground truth for
// diagnosing transcript duplication, where the dedup in the supervisor
// assumes the final authoritative message is a verbatim concatenation of
// the streamed deltas. Disabled (zero overhead past one env lookup) when
// the var is unset.
var (
	traceOnce sync.Once
	traceFile *os.File
	traceMu   sync.Mutex
)

func copilotTracer() *os.File {
	traceOnce.Do(func() {
		if path := strings.TrimSpace(os.Getenv("ATC_COPILOT_TRACE")); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				traceFile = f
			}
		}
	})
	return traceFile
}

func traceEvent(sessionID string, ev copilot.SessionEvent, e agent.Event, ok bool) {
	w := copilotTracer()
	if w == nil {
		return
	}
	id := sessionID
	if len(id) > 8 {
		id = id[:8]
	}
	action := "drop"
	if ok {
		action = "→ " + e.Type.String()
	}
	line := fmt.Sprintf("%s  %-8s  %-26s  %-14s  %s\n",
		time.Now().Format("15:04:05.000"), id, ev.Type(), action, traceContent(ev.Data))
	traceMu.Lock()
	_, _ = w.WriteString(line)
	traceMu.Unlock()
}

// traceContent renders an event's payload for the trace: lengths and
// quoted text so leading/whitespace differences between deltas and the
// final message (the suspected duplication trigger) are visible.
func traceContent(data rpc.SessionEventData) string {
	switch d := data.(type) {
	case *rpc.AssistantMessageDeltaData:
		return fmt.Sprintf("len=%d %q", len(d.DeltaContent), agent.Truncate(d.DeltaContent, 200))
	case *rpc.AssistantMessageData:
		return fmt.Sprintf("len=%d %q", len(d.Content), agent.Truncate(d.Content, 200))
	case *rpc.UserMessageData:
		return fmt.Sprintf("%q", agent.Truncate(d.Content, 120))
	case *rpc.ToolExecutionStartData:
		return d.ToolName
	case *rpc.ToolExecutionCompleteData:
		return fmt.Sprintf("success=%v", d.Success)
	default:
		return fmt.Sprintf("%T", data)
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
