// Package supervisor owns the Copilot SDK client and the set of live
// agent sessions: spawning (optionally in a fresh git worktree),
// resuming previous sessions, prompting, permission flow, usage
// accounting, and teardown.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	store    store

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
		store:  defaultStore(),
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

// NewSession validates the target directory, registers a session
// immediately (so the board shows it in "starting"), and finishes the
// launch — including worktree creation — in the background. Launch
// failures land on the session row as a visible error state.
func (s *Supervisor) NewSession(opts NewSessionOptions) (*Session, error) {
	if opts.Repo == "" {
		return nil, errors.New("repo/directory is required")
	}
	repo, err := filepath.Abs(opts.Repo)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", repo)
	}

	name := wt.Slug(opts.Name)
	if opts.Name == "" {
		name = fmt.Sprintf("session-%s", time.Now().Format("1504-05"))
	}
	name = s.uniqueName(name)

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
		Name: name, Repo: repo, Dir: repo,
		Preset: presetName, Created: time.Now(),
		status: StatusStarting,
	}
	s.mu.Lock()
	s.sessions = append(s.sessions, sess)
	s.mu.Unlock()
	s.poke()

	go s.launch(sess, model, opts.Prompt, opts.UseWorktree)
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

func (s *Supervisor) launch(sess *Session, model, prompt string, useWorktree bool) {
	if useWorktree {
		wtPath, branch, err := s.trees.Create(sess.Repo, sess.Name)
		if err != nil {
			sess.setError("worktree: " + err.Error())
			s.publish(bus.Error, sess, map[string]any{"error": err.Error()})
			s.poke()
			return
		}
		sess.mu.Lock()
		sess.Worktree, sess.Branch, sess.Dir = wtPath, branch, wtPath
		sess.mu.Unlock()
	}
	sess.mu.Lock()
	dir := sess.Dir
	sess.mu.Unlock()
	sess.appendEntry(EntrySystem, "starting agent in "+dir)
	s.poke()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sdkSess, err := s.client.CreateSession(ctx, &copilot.SessionConfig{
		Model:               model,
		WorkingDirectory:    dir,
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
	s.persist()
	s.publish(bus.SessionStarted, sess, map[string]any{"dir": dir, "model": model})
	s.poke()

	if prompt != "" {
		if err := s.Prompt(sess, prompt); err != nil {
			sess.appendEntry(EntryError, "failed to send prompt: "+err.Error())
			s.poke()
		}
	}
}

// ResumeAll restores sessions recorded by a previous run. Each appears
// on the board immediately and resumes in the background; sessions the
// runtime no longer knows show up as error rows to discard with K.
func (s *Supervisor) ResumeAll() int {
	saved := s.store.load()
	for _, sv := range saved {
		sess := &Session{
			Name: s.uniqueName(wt.Slug(sv.Name)), Repo: sv.Repo, Dir: sv.Dir,
			Worktree: sv.Worktree, Branch: sv.Branch, Preset: sv.Preset,
			Created: sv.Created, status: StatusStarting, id: sv.ID,
		}
		s.mu.Lock()
		s.sessions = append(s.sessions, sess)
		s.mu.Unlock()
		go s.resume(sess, sv)
	}
	if len(saved) > 0 {
		s.poke()
	}
	return len(saved)
}

func (s *Supervisor) resume(sess *Session, sv savedSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sdkSess, err := s.client.ResumeSession(ctx, sv.ID, &copilot.ResumeSessionConfig{
		ClientName:          "atc",
		WorkingDirectory:    sv.Dir,
		Model:               sv.Model,
		Streaming:           copilot.Bool(true),
		OnPermissionRequest: s.permissionHandler(sess),
	})
	if err != nil {
		sess.setError(fmt.Sprintf("resume failed: %v (K to discard)", err))
		s.poke()
		return
	}
	sess.mu.Lock()
	sess.sdk = sdkSess
	sess.id = sdkSess.SessionID
	sess.status = StatusDone
	sess.everWorked = true
	sess.mu.Unlock()
	sess.appendEntry(EntrySystem, "resumed from previous run — earlier transcript lives in the Copilot session log")
	sdkSess.On(func(ev copilot.SessionEvent) { s.handleEvent(sess, ev) })
	s.persist()
	s.poke()
}

// persist snapshots resumable sessions to disk. Best-effort: a failed
// write only costs resume-on-restart, never a running session.
func (s *Supervisor) persist() {
	var saved []savedSession
	for _, sess := range s.Sessions() {
		sess.mu.Lock()
		if sess.id != "" && sess.status != StatusClosed {
			saved = append(saved, savedSession{
				ID: sess.id, Name: sess.Name, Repo: sess.Repo, Dir: sess.Dir,
				Worktree: sess.Worktree, Branch: sess.Branch,
				Preset: sess.Preset, Model: sess.usage.Model, Created: sess.Created,
			})
		}
		sess.mu.Unlock()
	}
	_ = s.store.save(saved)
}

// Prompt sends a user message to the session.
func (s *Supervisor) Prompt(sess *Session, text string) error {
	sdk := sess.sdkSession()
	if sdk == nil {
		return errors.New("session is still starting")
	}
	sess.appendEntry(EntryUser, text)
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

// Kill tears a session down, forgets it in the resume store, and
// optionally removes its worktree.
func (s *Supervisor) Kill(sess *Session, removeWorktree bool) {
	sess.Respond(&rpc.PermissionDecisionCancelled{})
	if sdk := sess.sdkSession(); sdk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = sdk.Abort(ctx)
		cancel()
		_ = sdk.Disconnect()
	}
	sess.setStatus(StatusClosed)
	sess.mu.Lock()
	repo, worktree, branch := sess.Repo, sess.Worktree, sess.Branch
	sess.mu.Unlock()
	if removeWorktree && worktree != "" {
		if err := s.trees.Remove(repo, worktree, branch); err != nil {
			sess.appendEntry(EntryError, "worktree cleanup: "+err.Error())
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
	s.persist()
	s.publish(bus.SessionClosed, sess, nil)
	s.poke()
}

// Stop shuts the process down but leaves the resume store intact, so
// the next run can pick the sessions back up.
func (s *Supervisor) Stop() {
	s.persist()
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
		sess.appendEntry(EntryTool, toolSummary(d.ToolName, d.Arguments))
		s.publish(bus.ToolCall, sess, map[string]any{"tool": d.ToolName})
	case *rpc.ToolExecutionCompleteData:
		if !d.Success {
			msg := "tool call failed"
			if d.Error != nil && d.Error.Message != "" {
				msg = "tool failed: " + d.Error.Message
			}
			sess.appendEntry(EntryError, msg)
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
			sess.appendEntry(EntrySystem, "⛔ denied ("+reason+"): "+summary)
			s.poke()
			return &rpc.PermissionDecisionReject{Feedback: copilot.String("blocked by atc deny-list: " + reason)}, nil
		case policy.Allow:
			sess.appendEntry(EntrySystem, "auto-approved: "+summary)
			s.poke()
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}

		p := &Permission{Kind: kind, Summary: summary, Detail: detail, respond: make(chan rpc.PermissionDecision, 1)}
		sess.setPending(p)
		sess.appendEntry(EntrySystem, "permission requested: "+summary)
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
		return "shell", "run: " + truncate(r.FullCommandText, 80), detail
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

// toolSummary turns a tool invocation into a short human line like
// "bash · go test ./..." instead of raw JSON arguments.
func toolSummary(name string, args any) string {
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
			return name + " · " + truncate(v, 90)
		}
	}
	return name
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
