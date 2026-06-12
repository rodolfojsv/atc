// Package supervisor owns the set of live agent sessions across
// backends (GitHub Copilot, Claude Code): spawning (optionally in a
// fresh git worktree), resuming previous sessions, prompting,
// permission flow, usage accounting, and teardown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/agent/claudeagent"
	"github.com/rodolfojsv/atc/internal/agent/copilotagent"
	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/policy"
	"github.com/rodolfojsv/atc/internal/spend"
	"github.com/rodolfojsv/atc/internal/wt"
)

// DefaultBackend is used when neither the session nor its preset names
// one.
const DefaultBackend = "copilot"

type Supervisor struct {
	mu       sync.Mutex
	cfg      *config.Config
	backends map[string]agent.Backend
	sessions []*Session
	killed   map[string]bool // session IDs killed here; never re-adopt from disk
	trees    wt.Manager
	bus      *bus.Bus
	store    store
	ledger   *spend.Ledger

	notifyMu      sync.Mutex
	notify        func()
	notifyPending bool

	headless bool
}

func New(cfg *config.Config, b *bus.Bus) *Supervisor {
	return &Supervisor{
		cfg: cfg,
		backends: map[string]agent.Backend{
			"copilot": copilotagent.New(),
			"claude":  claudeagent.New(),
		},
		killed: map[string]bool{},
		trees:  wt.Manager{Root: cfg.WorktreeRoot},
		bus:    b,
		store:  defaultStore(),
		ledger: spend.Open(spendPath()),
	}
}

func spendPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".atc", "spend.jsonl")
}

// Spend returns the cumulative usage totals for today and this month.
func (s *Supervisor) Spend() (today, month spend.Totals) {
	return s.ledger.Today(), s.ledger.Month()
}

// Backends lists the available backend names, default first.
func (s *Supervisor) Backends() []string {
	names := []string{DefaultBackend}
	for n := range s.backends {
		if n != DefaultBackend {
			names = append(names, n)
		}
	}
	return names
}

func (s *Supervisor) backend(name string) (agent.Backend, error) {
	if name == "" {
		name = DefaultBackend
	}
	b, ok := s.backends[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", name)
	}
	return b, nil
}

// SetHeadless marks this supervisor as running without a UI (atc run):
// permission requests that would normally wait for a human are denied
// with an explanatory message instead of blocking forever.
func (s *Supervisor) SetHeadless(on bool) {
	s.headless = on
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
	Backend     string // "copilot" (default) or "claude"
	Preset      string
	Model       string // overrides preset model, then config model
	Prompt      string // optional first prompt
	ReadOnly    bool   // plan mode: the agent inspects but never modifies
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

	presetName := opts.Preset
	if presetName == "" {
		presetName = "default"
	}
	preset := s.cfg.Preset(presetName)

	backendName := opts.Backend
	if backendName == "" {
		backendName = preset.Backend
	}
	if backendName == "" {
		backendName = DefaultBackend
	}
	if _, err := s.backend(backendName); err != nil {
		return nil, err
	}

	name := wt.Slug(opts.Name)
	if opts.Name == "" {
		name = fmt.Sprintf("session-%s", time.Now().Format("1504-05"))
	}
	name = s.uniqueName(name)

	model := opts.Model
	if model == "" {
		model = preset.Model
	}
	if model == "" {
		model = s.cfg.Model
	}

	sess := &Session{
		Name: name, Repo: repo, Dir: repo, Backend: backendName,
		Preset: presetName, ReadOnly: opts.ReadOnly, Created: time.Now(),
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

func (s *Supervisor) spec(sess *Session, model string) agent.SessionSpec {
	sess.mu.Lock()
	dir := sess.Dir
	sess.mu.Unlock()
	return agent.SessionSpec{
		WorkingDir:   dir,
		Model:        model,
		Approval:     s.cfg.Preset(sess.Preset).Approval,
		ReadOnly:     sess.ReadOnly,
		OnEvent:      func(e agent.Event) { s.handleEvent(sess, e) },
		OnPermission: s.permissionFunc(sess),
	}
}

func (s *Supervisor) launch(sess *Session, model, prompt string, useWorktree bool) {
	if useWorktree {
		baseBranch, baseCommit, _ := s.trees.Base(sess.Repo)
		wtPath, branch, err := s.trees.Create(sess.Repo, sess.Name)
		if err != nil {
			sess.setError("worktree: " + err.Error())
			s.publish(bus.Error, sess, map[string]any{"error": err.Error()})
			s.poke()
			return
		}
		sess.mu.Lock()
		sess.Worktree, sess.Branch, sess.Dir = wtPath, branch, wtPath
		sess.BaseBranch, sess.BaseCommit = baseBranch, baseCommit
		sess.mu.Unlock()
	}
	spec := s.spec(sess, model)
	sess.appendEntry(EntrySystem, "starting "+sess.Backend+" agent in "+spec.WorkingDir)
	if sess.Backend == "claude" && spec.Approval != config.ApprovalAllowAll {
		sess.appendEntry(EntrySystem, "claude backend has no runtime permission prompts; 'prompt' maps to permission-mode acceptEdits (other tools are denied headlessly)")
	}
	s.poke()

	backend, err := s.backend(sess.Backend)
	if err != nil {
		sess.setError(err.Error())
		s.poke()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ag, err := backend.NewSession(ctx, spec)
	if err != nil {
		sess.setError(fmt.Sprintf("failed to start: %v", err))
		s.publish(bus.Error, sess, map[string]any{"error": err.Error()})
		s.poke()
		return
	}

	sess.mu.Lock()
	sess.ag = ag
	sess.id = ag.ID()
	if sess.status == StatusStarting {
		sess.status = StatusIdle
	}
	sess.mu.Unlock()

	s.persist()
	s.publish(bus.SessionStarted, sess, map[string]any{"dir": spec.WorkingDir, "model": model, "backend": sess.Backend})
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
		backendName := sv.Backend
		if backendName == "" {
			backendName = DefaultBackend
		}
		sess := &Session{
			Name: s.uniqueName(wt.Slug(sv.Name)), Repo: sv.Repo, Dir: sv.Dir,
			Worktree: sv.Worktree, Branch: sv.Branch, Backend: backendName,
			Preset: sv.Preset, ReadOnly: sv.ReadOnly, Created: sv.Created,
			BaseBranch: sv.BaseBranch, BaseCommit: sv.BaseCommit,
			status: StatusStarting, id: sv.ID,
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
	backend, err := s.backend(sess.Backend)
	if err != nil {
		sess.setError(err.Error() + " (K to discard)")
		s.poke()
		return
	}
	spec := s.spec(sess, sv.Model)
	spec.SessionID = sv.ID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ag, err := backend.ResumeSession(ctx, spec)
	if err != nil {
		sess.setError(fmt.Sprintf("resume failed: %v (K to discard)", err))
		s.poke()
		return
	}
	sess.mu.Lock()
	sess.ag = ag
	sess.id = ag.ID()
	sess.status = StatusDone
	sess.everWorked = true
	sess.mu.Unlock()

	restored := s.replayHistory(sess, ag.History(context.Background()))
	if restored > 0 {
		sess.appendEntry(EntrySystem, fmt.Sprintf("— resumed; %d earlier events restored —", restored))
	} else {
		sess.appendEntry(EntrySystem, "resumed from previous run (no earlier transcript available)")
	}
	s.persist()
	s.poke()
}

// replayHistory feeds persisted events back into the transcript,
// restoring chat text, ↑-recall history, and usage totals.
func (s *Supervisor) replayHistory(sess *Session, events []agent.Event) int {
	restored := 0
	for _, e := range events {
		switch e.Type {
		case agent.EventUserMessage:
			sess.appendEntry(EntryUser, e.Text)
			sess.addHistory(e.Text)
			restored++
		case agent.EventMessage:
			sess.finishMessage(e.Text)
			restored++
		case agent.EventToolStart:
			sess.appendEntry(EntryTool, e.Text)
			restored++
		case agent.EventToolFailed:
			sess.appendEntry(EntryError, e.Text)
			restored++
		case agent.EventContext:
			sess.updateContext(e.CurrentTokens, e.TokenLimit)
		case agent.EventUsage:
			sess.addUsage(e)
		}
	}
	return restored
}

// persist snapshots resumable sessions to disk, merging with entries
// written by other atc processes (a Task Scheduler `atc run` writes
// here too) so neither side clobbers the other. Best-effort: a failed
// write only costs resume-on-restart, never a running session.
func (s *Supervisor) persist() {
	var saved []savedSession
	mine := map[string]bool{}
	for _, sess := range s.Sessions() {
		sess.mu.Lock()
		if sess.id != "" && sess.status != StatusClosed {
			mine[sess.id] = true
			saved = append(saved, savedSession{
				ID: sess.id, Name: sess.Name, Repo: sess.Repo, Dir: sess.Dir,
				Worktree: sess.Worktree, Branch: sess.Branch, Backend: sess.Backend,
				Preset: sess.Preset, Model: sess.usage.Model, ReadOnly: sess.ReadOnly,
				BaseBranch: sess.BaseBranch, BaseCommit: sess.BaseCommit,
				Status: string(sess.status), Created: sess.Created,
			})
		}
		sess.mu.Unlock()
	}
	s.mu.Lock()
	killed := s.killed
	s.mu.Unlock()
	for _, sv := range s.store.load() {
		if !mine[sv.ID] && !killed[sv.ID] {
			saved = append(saved, sv)
		}
	}
	_ = s.store.save(saved)
}

// WatchStore polls the session store and adopts settled sessions that
// another atc process wrote — e.g. a scheduled `atc run` that finished
// while this TUI was open. The adopted session appears on the board
// with its transcript restored, ready to continue interactively.
func (s *Supervisor) WatchStore(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		known := map[string]bool{}
		for _, sess := range s.Sessions() {
			sess.mu.Lock()
			if sess.id != "" {
				known[sess.id] = true
			}
			sess.mu.Unlock()
		}
		s.mu.Lock()
		for id := range s.killed {
			known[id] = true
		}
		s.mu.Unlock()

		for _, sv := range s.store.load() {
			if sv.ID == "" || known[sv.ID] || !sv.settled() {
				continue
			}
			backendName := sv.Backend
			if backendName == "" {
				backendName = DefaultBackend
			}
			sess := &Session{
				Name: s.uniqueName(wt.Slug(sv.Name)), Repo: sv.Repo, Dir: sv.Dir,
				Worktree: sv.Worktree, Branch: sv.Branch, Backend: backendName,
				Preset: sv.Preset, ReadOnly: sv.ReadOnly, Created: sv.Created,
				BaseBranch: sv.BaseBranch, BaseCommit: sv.BaseCommit,
				status: StatusStarting, id: sv.ID,
			}
			s.mu.Lock()
			s.sessions = append(s.sessions, sess)
			s.mu.Unlock()
			s.publish(bus.SessionAdopted, sess, map[string]any{"status": sv.Status})
			go s.resume(sess, sv)
			s.poke()
		}
	}
}

// Prompt sends a user message to the session.
func (s *Supervisor) Prompt(sess *Session, text string) error {
	ag := sess.agentSession()
	if ag == nil {
		return errors.New("session is still starting")
	}
	sess.appendEntry(EntryUser, text)
	sess.addHistory(text)
	sess.setStatus(StatusWorking)
	s.poke()
	err := ag.Send(context.Background(), text)
	if err != nil {
		sess.setError(fmt.Sprintf("send failed: %v", err))
		s.poke()
	}
	return err
}

// Abort cancels the session's current turn.
func (s *Supervisor) Abort(sess *Session) {
	if ag := sess.agentSession(); ag != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = ag.Abort(ctx)
	}
	// Unblock a waiting permission handler, if any.
	sess.Respond(agent.Cancel, "")
}

// Kill tears a session down, forgets it in the resume store, and
// optionally removes its worktree.
func (s *Supervisor) Kill(sess *Session, removeWorktree bool) {
	sess.Respond(agent.Cancel, "")
	if ag := sess.agentSession(); ag != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = ag.Abort(ctx)
		cancel()
		_ = ag.Close()
	}
	sess.setStatus(StatusClosed)
	sess.mu.Lock()
	repo, worktree, branch, id := sess.Repo, sess.Worktree, sess.Branch, sess.id
	sess.mu.Unlock()
	if id != "" {
		s.mu.Lock()
		s.killed[id] = true
		s.mu.Unlock()
	}
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
		sess.Respond(agent.Cancel, "")
		if ag := sess.agentSession(); ag != nil {
			_ = ag.Close()
		}
	}
	for _, b := range s.backends {
		_ = b.Stop()
	}
}

func (s *Supervisor) publish(typ string, sess *Session, data map[string]any) {
	sess.mu.Lock()
	id := sess.id
	sess.mu.Unlock()
	s.bus.Publish(bus.Event{Type: typ, SessionID: id, SessionName: sess.Name, Data: data})
}

func (s *Supervisor) handleEvent(sess *Session, e agent.Event) {
	sess.touch()
	switch e.Type {
	case agent.EventTurnStart:
		sess.setStatus(StatusWorking)
	case agent.EventIntent:
		sess.setIntent(e.Text)
	case agent.EventTextDelta:
		sess.appendStream(e.Text)
	case agent.EventMessage:
		sess.finishMessage(e.Text)
	case agent.EventUserMessage:
		sess.appendEntry(EntryUser, e.Text)
	case agent.EventToolStart:
		sess.appendEntry(EntryTool, e.Text)
		s.publish(bus.ToolCall, sess, map[string]any{"tool": e.Text})
	case agent.EventToolFailed:
		sess.appendEntry(EntryError, e.Text)
	case agent.EventIdle:
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
	case agent.EventError:
		sess.setError(fmt.Sprintf("%s: %s", e.ErrType, e.Text))
		s.publish(bus.Error, sess, map[string]any{"errorType": e.ErrType, "message": e.Text})
	case agent.EventContext:
		sess.updateContext(e.CurrentTokens, e.TokenLimit)
	case agent.EventUsage:
		sess.addUsage(e)
		s.ledger.Add(spend.Record{
			Session: sess.Name, Backend: sess.Backend, Model: e.Model,
			In: e.InputTokens, Out: e.OutputTokens,
			NanoAiu: e.NanoAiu, CostUSD: e.CostUSD,
		})
	}
	s.poke()
}

// Diff shows what the session's worktree changed relative to its base.
func (s *Supervisor) Diff(sess *Session) (string, error) {
	sess.mu.Lock()
	dir, base := sess.Worktree, sess.BaseCommit
	sess.mu.Unlock()
	if dir == "" {
		return "", errors.New("session has no worktree — it works directly in the repo")
	}
	return s.trees.Diff(dir, base)
}

// Merge commits the worktree's changes and merges its branch back into
// the branch it was created from.
func (s *Supervisor) Merge(sess *Session) error {
	sess.mu.Lock()
	repo, dir, branch, baseBranch := sess.Repo, sess.Worktree, sess.Branch, sess.BaseBranch
	sess.mu.Unlock()
	if dir == "" {
		return errors.New("session has no worktree")
	}
	err := s.trees.Merge(repo, dir, branch, baseBranch, "atc: changes from session "+sess.Name)
	if err != nil {
		sess.appendEntry(EntryError, "merge: "+err.Error())
	} else {
		sess.appendEntry(EntrySystem, "merged "+branch+" into "+baseBranch)
	}
	s.poke()
	return err
}

// permissionFunc runs on a backend goroutine for each permission
// request: deny-list first, then auto-approval, then block until the
// human answers through the UI.
func (s *Supervisor) permissionFunc(sess *Session) agent.PermissionFunc {
	return func(req agent.PermissionRequest) (agent.Decision, string) {
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
			sess.appendEntry(EntrySystem, "⛔ denied ("+reason+"): "+req.Summary)
			s.poke()
			return agent.Deny, "blocked by atc deny-list: " + reason
		case policy.Allow:
			sess.appendEntry(EntrySystem, "auto-approved: "+req.Summary)
			s.poke()
			return agent.ApproveOnce, ""
		}
		if sess.approvedByRule(req) {
			sess.appendEntry(EntrySystem, "auto-approved (session rule): "+req.Summary)
			s.poke()
			return agent.ApproveOnce, ""
		}

		if s.headless {
			sess.appendEntry(EntrySystem, "⛔ denied (headless run): "+req.Summary)
			s.poke()
			return agent.Deny, "headless run (atc run): not pre-approved by the preset; use an allow-all preset for unattended runs"
		}

		p := &Permission{Kind: req.Kind, Summary: req.Summary, Detail: req.Detail, respond: make(chan permissionAnswer, 1)}
		sess.setPending(p)
		sess.appendEntry(EntrySystem, "permission requested: "+req.Summary)
		s.publish(bus.WaitingOnPermission, sess, map[string]any{"kind": req.Kind, "summary": req.Summary})
		s.poke()

		ans := <-p.respond
		sess.clearPending()
		if ans.decision == agent.ApproveSession {
			rule := ruleFor(req)
			sess.addApproval(rule)
			sess.appendEntry(EntrySystem, "session rule added: always allow "+rule.label())
		}
		s.publish(bus.PermissionResolved, sess, map[string]any{"kind": req.Kind, "summary": req.Summary, "decision": fmt.Sprintf("%d", ans.decision)})
		s.poke()
		if ans.decision == agent.Deny && ans.feedback == "" {
			ans.feedback = "denied by user in atc"
		}
		return ans.decision, ans.feedback
	}
}
