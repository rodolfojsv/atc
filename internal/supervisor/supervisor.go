// Package supervisor owns the Copilot SDK client and the set of live
// agent sessions: spawning (optionally in a fresh git worktree),
// prompting, permission flow, usage accounting, and teardown.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/policy"
	"github.com/rodolfojsv/atc/internal/wt"
)

type Supervisor struct {
	mu       sync.Mutex
	cfg      *config.Config
	client   *copilot.Client
	sessions []*Session
	trees    wt.Manager
	bus      *bus.Bus

	notifyMu      sync.Mutex
	notify        func()
	notifyPending bool
}

func New(cfg *config.Config, b *bus.Bus) *Supervisor {
	return &Supervisor{
		cfg:    cfg,
		client: copilot.NewClient(nil),
		trees:  wt.Manager{Root: cfg.WorktreeRoot},
		bus:    b,
	}
}

// SetNotify registers the UI wake-up callback (e.g. tea.Program.Send).
func (s *Supervisor) SetNotify(fn func()) {
	s.notifyMu.Lock()
	s.notify = fn
	s.notifyMu.Unlock()
}

// poke wakes the UI, coalescing bursts (streaming deltas can arrive
// hundreds of times per second) to at most one wake-up per 25ms.
func (s *Supervisor) poke() {
	s.notifyMu.Lock()
	fn := s.notify
	if fn == nil || s.notifyPending {
		s.notifyMu.Unlock()
		return
	}
	s.notifyPending = true
	s.notifyMu.Unlock()
	time.AfterFunc(25*time.Millisecond, func() {
		s.notifyMu.Lock()
		s.notifyPending = false
		s.notifyMu.Unlock()
		fn()
	})
}

// Sessions returns a snapshot of the current session list.
func (s *Supervisor) Sessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Session(nil), s.sessions...)
}

// ActiveCount counts sessions that are not closed or errored.
func (s *Supervisor) ActiveCount() int {
	n := 0
	for _, sess := range s.Sessions() {
		if sess.Active() {
			n++
		}
	}
	return n
}

type NewSessionOptions struct {
	Name        string
	Repo        string // repo or plain directory the agent runs in
	UseWorktree bool
	Preset      string
	Model       string // overrides preset model, then config model
	Prompt      string // optional first prompt
}

// NewSession registers a session immediately (so the board shows it in
// "starting") and launches the SDK session in the background.
func (s *Supervisor) NewSession(opts NewSessionOptions) (*Session, error) {
	name := wt.Slug(opts.Name)
	if opts.Name == "" {
		name = fmt.Sprintf("session-%s", time.Now().Format("1504-05"))
	}
	name = s.uniqueName(name)

	if opts.Repo == "" {
		return nil, errors.New("repo/directory is required")
	}

	dir := opts.Repo
	var wtPath, branch string
	if opts.UseWorktree {
		var err error
		wtPath, branch, err = s.trees.Create(opts.Repo, name)
		if err != nil {
			return nil, fmt.Errorf("creating worktree: %w", err)
		}
		dir = wtPath
	}

	presetName := opts.Preset
	if presetName == "" {
		presetName = "default"
	}
	preset := s.cfg.Preset(presetName)
	model := opts.Model
	if model == "" {
		model = preset.Model
	}
	if model == "" {
		model = s.cfg.Model
	}

	sess := &Session{
		Name: name, Repo: opts.Repo, Dir: dir, Worktree: wtPath,
		Branch: branch, Preset: presetName, Created: time.Now(),
		status: StatusStarting,
	}
	s.mu.Lock()
	s.sessions = append(s.sessions, sess)
	s.mu.Unlock()
	s.poke()

	go s.launch(sess, model, opts.Prompt)
	return sess, nil
}

func (s *Supervisor) uniqueName(name string) string {
	taken := map[string]bool{}
	for _, sess := range s.Sessions() {
		taken[sess.Name] = true
	}
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		if c := fmt.Sprintf("%s-%d", name, i); !taken[c] {
			return c
		}
	}
}

func (s *Supervisor) launch(sess *Session, model, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sdkSess, err := s.client.CreateSession(ctx, &copilot.SessionConfig{
		Model:               model,
		WorkingDirectory:    sess.Dir,
		ClientName:          "atc",
		Streaming:           copilot.Bool(true),
		OnPermissionRequest: s.permissionHandler(sess),
	})
	if err != nil {
		sess.setError(fmt.Sprintf("failed to start: %v", err))
		s.publish(bus.Error, sess, map[string]any{"error": err.Error()})
		s.poke()
		return
	}

	sess.mu.Lock()
	sess.sdk = sdkSess
	sess.id = sdkSess.SessionID
	if sess.status == StatusStarting {
		sess.status = StatusIdle
	}
	sess.mu.Unlock()

	sdkSess.On(func(ev copilot.SessionEvent) { s.handleEvent(sess, ev) })
	s.publish(bus.SessionStarted, sess, map[string]any{"dir": sess.Dir, "model": model})
	s.poke()

	if prompt != "" {
		if err := s.Prompt(sess, prompt); err != nil {
			sess.appendLine("✗ failed to send prompt: " + err.Error())
			s.poke()
		}
	}
}

// Prompt sends a user message to the session.
func (s *Supervisor) Prompt(sess *Session, text string) error {
	sdk := sess.sdkSession()
	if sdk == nil {
		return errors.New("session is still starting")
	}
	sess.appendLine("» " + text)
	sess.setStatus(StatusWorking)
	s.poke()
	_, err := sdk.Send(context.Background(), copilot.MessageOptions{Prompt: text})
	if err != nil {
		sess.setError(fmt.Sprintf("send failed: %v", err))
		s.poke()
	}
	return err
}

// Abort cancels the session's current turn.
func (s *Supervisor) Abort(sess *Session) {
	if sdk := sess.sdkSession(); sdk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sdk.Abort(ctx)
	}
	// Unblock a waiting permission handler, if any.
	sess.Respond(&rpc.PermissionDecisionCancelled{})
}

// Kill tears a session down and optionally removes its worktree.
func (s *Supervisor) Kill(sess *Session, removeWorktree bool) {
	sess.Respond(&rpc.PermissionDecisionCancelled{})
	if sdk := sess.sdkSession(); sdk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = sdk.Abort(ctx)
		cancel()
		_ = sdk.Disconnect()
	}
	sess.setStatus(StatusClosed)
	if removeWorktree && sess.Worktree != "" {
		if err := s.trees.Remove(sess.Repo, sess.Worktree, sess.Branch); err != nil {
			sess.appendLine("✗ worktree cleanup: " + err.Error())
		}
	}
	s.mu.Lock()
	for i, x := range s.sessions {
		if x == sess {
			s.sessions = append(s.sessions[:i], s.sessions[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	s.publish(bus.SessionClosed, sess, nil)
	s.poke()
}

// Stop shuts everything down: pending permissions are cancelled,
// sessions disconnected, and the CLI server process terminated.
func (s *Supervisor) Stop() {
	for _, sess := range s.Sessions() {
		sess.Respond(&rpc.PermissionDecisionCancelled{})
		if sdk := sess.sdkSession(); sdk != nil {
			_ = sdk.Disconnect()
		}
	}
	_ = s.client.Stop()
}

func (s *Supervisor) publish(typ string, sess *Session, data map[string]any) {
	sess.mu.Lock()
	id := sess.id
	sess.mu.Unlock()
	s.bus.Publish(bus.Event{Type: typ, SessionID: id, SessionName: sess.Name, Data: data})
}

func (s *Supervisor) handleEvent(sess *Session, ev copilot.SessionEvent) {
	switch d := ev.Data.(type) {
	case *rpc.AssistantTurnStartData:
		sess.setStatus(StatusWorking)
	case *rpc.AssistantIntentData:
		sess.setIntent(d.Intent)
	case *rpc.AssistantMessageDeltaData:
		sess.appendStream(d.DeltaContent)
	case *rpc.AssistantMessageData:
		sess.finishMessage(d.Content)
	case *rpc.ToolExecutionStartData:
		sess.appendLine("⚙ " + d.ToolName + summarizeArgs(d.Arguments))
		s.publish(bus.ToolCall, sess, map[string]any{"tool": d.ToolName})
	case *rpc.ToolExecutionCompleteData:
		if !d.Success {
			msg := "⚙ " + d.ToolCallID + " failed"
			if d.Error != nil && d.Error.Message != "" {
				msg = "⚙ tool failed: " + d.Error.Message
			}
			sess.appendLine(msg)
		}
	case *rpc.SessionIdleData:
		sess.mu.Lock()
		worked := sess.everWorked
		closedOrErr := sess.status == StatusClosed || sess.status == StatusError
		if !closedOrErr {
			if worked {
				sess.status = StatusDone
			} else {
				sess.status = StatusIdle
			}
		}
		sess.mu.Unlock()
		if worked && !closedOrErr {
			s.publish(bus.Finished, sess, map[string]any{"lastLine": sess.View().LastLine})
		}
	case *rpc.SessionErrorData:
		sess.setError(fmt.Sprintf("%s: %s", d.ErrorType, d.Message))
		s.publish(bus.Error, sess, map[string]any{"errorType": d.ErrorType, "message": d.Message})
	case *rpc.SessionUsageInfoData:
		sess.updateContext(d.CurrentTokens, d.TokenLimit)
	case *rpc.AssistantUsageData:
		sess.addUsage(d)
	}
	s.poke()
}

// permissionHandler runs on an SDK goroutine for each permission
// request: deny-list first, then auto-approval, then block until the
// human answers through the UI.
func (s *Supervisor) permissionHandler(sess *Session) copilot.PermissionHandlerFunc {
	return func(req copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		kind, summary, detail := describeRequest(req)

		sess.mu.Lock()
		auto := sess.autoApprove
		sess.mu.Unlock()
		approval := s.cfg.Preset(sess.Preset).Approval
		if auto {
			approval = config.ApprovalAllowAll
		}

		verdict, reason := policy.Evaluate(approval, req)
		switch verdict {
		case policy.Deny:
			sess.appendLine("⛔ denied (" + reason + "): " + summary)
			s.poke()
			return &rpc.PermissionDecisionReject{Feedback: copilot.String("blocked by atc deny-list: " + reason)}, nil
		case policy.Allow:
			sess.appendLine("✓ auto-approved: " + summary)
			s.poke()
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}

		p := &Permission{Kind: kind, Summary: summary, Detail: detail, respond: make(chan rpc.PermissionDecision, 1)}
		sess.setPending(p)
		sess.appendLine("⚠ permission requested: " + summary)
		s.publish(bus.WaitingOnPermission, sess, map[string]any{"kind": kind, "summary": summary})
		s.poke()

		decision := <-p.respond
		sess.clearPending()
		s.publish(bus.PermissionResolved, sess, map[string]any{"kind": kind, "summary": summary, "decision": fmt.Sprintf("%T", decision)})
		s.poke()
		return decision, nil
	}
}

func describeRequest(req copilot.PermissionRequest) (kind, summary string, detail []string) {
	switch r := req.(type) {
	case *rpc.PermissionRequestShell:
		detail = []string{"Command:", "  " + r.FullCommandText}
		if r.Intention != "" {
			detail = append(detail, "", "Intention: "+r.Intention)
		}
		if r.Warning != nil && *r.Warning != "" {
			detail = append(detail, "", "⚠ "+*r.Warning)
		}
		return "shell", "run: "+truncate(r.FullCommandText, 80), detail
	case *rpc.PermissionRequestWrite:
		detail = []string{"File: " + r.FileName}
		if r.Intention != "" {
			detail = append(detail, "Intention: "+r.Intention)
		}
		if r.Diff != "" {
			detail = append(detail, "", "Diff:")
			detail = append(detail, strings.Split(truncate(r.Diff, 4000), "\n")...)
		}
		return "write", "write: " + r.FileName, detail
	case *rpc.PermissionRequestRead:
		detail = []string{"Path: " + r.Path}
		if r.Intention != "" {
			detail = append(detail, "Intention: "+r.Intention)
		}
		return "read", "read: " + r.Path, detail
	case *rpc.PermissionRequestURL:
		detail = []string{"URL: " + r.URL}
		if r.Intention != "" {
			detail = append(detail, "Intention: "+r.Intention)
		}
		return "url", "fetch: " + r.URL, detail
	case *rpc.PermissionRequestMCP:
		detail = []string{"MCP server: " + r.ServerName, "Tool: " + r.ToolName}
		if r.Args != nil {
			if b, err := json.Marshal(r.Args); err == nil {
				detail = append(detail, "Args: "+truncate(string(b), 500))
			}
		}
		return "mcp", "mcp: " + r.ServerName + "/" + r.ToolName, detail
	default:
		return string(req.Kind()), string(req.Kind()) + " request", []string{fmt.Sprintf("%+v", req)}
	}
}

func summarizeArgs(args any) string {
	if args == nil {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return " " + truncate(string(b), 100)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
