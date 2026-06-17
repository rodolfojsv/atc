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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/agent/claudeagent"
	"github.com/rodolfojsv/atc/internal/agent/copilotagent"
	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/logx"
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
	log      *logx.Logger

	prefsMu    sync.Mutex
	prefsStore prefsStore
	prefs      prefs

	notifyMu      sync.Mutex
	notify        func()
	notifyPending bool

	headless bool
}

func New(cfg *config.Config, b *bus.Bus) *Supervisor {
	level := logx.ParseLevel(cfg.LogLevel)
	log := logx.Open(cfg.LogFile, level)
	// Pass the runtime's own diagnostics through at debug level; they
	// land in the Copilot CLI's log location, not ours.
	sdkLogLevel := ""
	if level >= logx.Debug {
		sdkLogLevel = "debug"
	}
	log.Log(logx.Info, "atc.start", map[string]any{"logLevel": cfg.LogLevel})
	ps := defaultPrefsStore()
	return &Supervisor{
		cfg: cfg,
		backends: map[string]agent.Backend{
			"copilot": copilotagent.New(sdkLogLevel),
			"claude":  claudeagent.New(),
		},
		killed:     map[string]bool{},
		trees:      wt.Manager{Root: cfg.WorktreeRoot},
		bus:        b,
		store:      defaultStore(),
		ledger:     spend.Open(spendPath()),
		log:        log,
		prefsStore: ps,
		prefs:      ps.load(),
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// PreferredBackend is what a new-session form should default to: the
// last backend the user actually launched, else the configured default,
// else the built-in default. Lets the choice stick across restarts
// without editing config.
func (s *Supervisor) PreferredBackend() string {
	s.prefsMu.Lock()
	last := s.prefs.LastBackend
	s.prefsMu.Unlock()
	if _, ok := s.backends[last]; ok {
		return last
	}
	if _, ok := s.backends[s.cfg.DefaultBackend]; ok {
		return s.cfg.DefaultBackend
	}
	return DefaultBackend
}

// rememberBackend records the backend just launched so the next new
// session defaults to it.
func (s *Supervisor) rememberBackend(name string) {
	s.prefsMu.Lock()
	if s.prefs.LastBackend == name {
		s.prefsMu.Unlock()
		return
	}
	s.prefs.LastBackend = name
	p := s.prefs
	s.prefsMu.Unlock()
	s.prefsStore.save(p)
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
	NameHint    string // derives an auto-name when Name is empty; never sent to the agent (the web form passes its first prompt here)
	Category    string // board category; empty defaults to the repo (see defaultCategory)
	Repo        string // repo or plain directory the agent runs in
	UseWorktree bool
	Backend     string // "copilot" (default) or "claude"
	Preset      string
	Model       string // overrides preset model, then config model
	Prompt      string // optional first prompt
	ReadOnly    bool   // plan mode: the agent inspects but never modifies
	AutoApprove bool   // start in allow-all (deny-list still gates Copilot)
	CreatedBy   string // opaque per-device clientId of the creator (web/app); "" for TUI
	NotifyTopic string // ntfy topic of the creator's device; "" for TUI/scheduler
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
		backendName = s.cfg.DefaultBackend
	}
	if backendName == "" {
		backendName = DefaultBackend
	}
	if _, err := s.backend(backendName); err != nil {
		return nil, err
	}
	s.rememberBackend(backendName)

	name := wt.CleanName(opts.Name)
	if name == "" {
		name = autoName(firstNonEmpty(opts.Prompt, opts.NameHint), repo)
	}
	name = s.uniqueName(name)

	model := opts.Model
	if model == "" {
		model = preset.Model
	}
	if model == "" {
		model = s.cfg.Model
	}

	category := opts.Category
	if category == "" {
		category = s.defaultCategory(repo)
	}

	autoApprove := opts.AutoApprove || s.cfg.DefaultAutoApprove
	sess := &Session{
		Name: name, Repo: repo, Dir: repo, Backend: backendName,
		Preset: presetName, ReadOnly: opts.ReadOnly, Model: model,
		Created: time.Now(), status: StatusStarting, autoApprove: autoApprove,
		category: category, createdBy: opts.CreatedBy, notifyTopic: opts.NotifyTopic,
	}
	s.mu.Lock()
	s.sessions = append(s.sessions, sess)
	s.mu.Unlock()
	s.poke()

	go s.launch(sess, model, opts.Prompt, opts.UseWorktree)
	return sess, nil
}

// defaultCategory picks the board category for a new session when the
// caller didn't set one: a config override keyed by the repo's absolute
// path or base name, else the repo's base directory name. The user can
// re-categorize afterward with SetCategory.
func (s *Supervisor) defaultCategory(repo string) string {
	if c, ok := s.cfg.CategoryByRepo[repo]; ok {
		return c
	}
	base := filepath.Base(repo)
	if c, ok := s.cfg.CategoryByRepo[base]; ok {
		return c
	}
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// autoName derives a friendly session name when the user didn't supply
// one: the first few words of the first prompt, else the repo's base
// name, else a short timestamp. The caller still de-dupes the result.
func autoName(prompt, repo string) string {
	if words := strings.Fields(prompt); len(words) > 0 {
		if len(words) > 6 {
			words = words[:6]
		}
		if s := capLen(wt.CleanName(strings.Join(words, " ")), 40); s != "" {
			return s
		}
	}
	if repo != "" {
		if s := wt.CleanName(filepath.Base(repo)); s != "" {
			return s
		}
	}
	return fmt.Sprintf("session-%s", time.Now().Format("1504-05"))
}

// capLen trims a name to at most n runes, preferring to break at the
// last word boundary (space, else dash) so it doesn't cut mid-word.
func capLen(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " -"); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(strings.TrimSpace(cut), "-.")
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
	auto := sess.autoApprove
	sess.mu.Unlock()
	approval := s.cfg.Preset(sess.Preset).Approval
	// A session started in auto-approve runs unattended: surface that to
	// the backend's own permission mechanism too, so Claude (whose mode
	// is fixed at launch) spawns in bypassPermissions rather than only
	// flipping the Copilot runtime path. Deny-list still gates Copilot.
	if auto {
		approval = config.ApprovalAllowAll
	}
	return agent.SessionSpec{
		WorkingDir:   dir,
		Model:        model,
		Approval:     approval,
		ReadOnly:     sess.ReadOnly,
		OnEvent:      func(e agent.Event) { s.handleEvent(sess, e) },
		OnPermission: s.permissionFunc(sess),
		OnQuestion:   s.questionFunc(sess),
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
	s.log.Log(logx.Info, "session.launch", map[string]any{
		"session": sess.Name, "backend": sess.Backend, "model": model,
		"dir": spec.WorkingDir, "approval": spec.Approval, "readOnly": spec.ReadOnly,
		"worktree": useWorktree,
	})
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
		s.log.Log(logx.Info, "session.launch_failed", map[string]any{"session": sess.Name, "error": err.Error()})
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
	s.log.Log(logx.Info, "session.started", map[string]any{"session": sess.Name, "id": ag.ID()})

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
			Name: s.uniqueName(wt.CleanName(sv.Name)), Repo: sv.Repo, Dir: sv.Dir,
			Worktree: sv.Worktree, Branch: sv.Branch, Backend: backendName,
			Preset: sv.Preset, ReadOnly: sv.ReadOnly, Model: sv.Model,
			Created: sv.Created, BaseBranch: sv.BaseBranch, BaseCommit: sv.BaseCommit,
			autoApprove: sv.AutoApprove, pinned: sv.Pinned, category: sv.Category,
			createdBy: sv.CreatedBy, notifyTopic: sv.NotifyTopic,
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
		s.log.Log(logx.Info, "session.resume_failed", map[string]any{"session": sess.Name, "id": sv.ID, "error": err.Error()})
		sess.setError(fmt.Sprintf("resume failed: %v (K to discard)", err))
		s.poke()
		return
	}
	sess.mu.Lock()
	sess.ag = ag
	sess.id = ag.ID()
	sess.status = StatusDone
	sess.everWorked = true
	// Restore the usage snapshot atc persisted; the runtimes' own logs
	// don't reliably keep usage events (often marked ephemeral), so
	// history replay can't be trusted for the numbers.
	sess.usage.InputTokens = sv.InTokens
	sess.usage.OutputTokens = sv.OutTokens
	sess.usage.NanoAiu = sv.NanoAiu
	sess.usage.CostUSD = sv.CostUSD
	sess.usage.CurrentTokens = sv.CurrentTokens
	sess.usage.TokenLimit = sv.TokenLimit
	if sess.usage.Model == "" {
		sess.usage.Model = sv.Model
	}
	snapshotHasUsage := sv.InTokens+sv.OutTokens > 0 || sv.NanoAiu > 0 || sv.CostUSD > 0
	sess.mu.Unlock()

	restored := s.replayHistory(sess, ag.History(context.Background()), !snapshotHasUsage)
	s.log.Log(logx.Info, "session.resumed", map[string]any{"session": sess.Name, "id": sv.ID, "restored": restored})
	if restored > 0 {
		sess.appendEntry(EntrySystem, fmt.Sprintf("— resumed; %d earlier events restored —", restored))
	} else {
		sess.appendEntry(EntrySystem, "resumed from previous run (no earlier transcript available)")
	}
	s.persist()
	s.poke()
}

// replayHistory feeds persisted events back into the transcript,
// restoring chat text and ↑-recall history. Usage events apply only
// when applyUsage is set (no snapshot existed — e.g. pre-snapshot
// store files); otherwise the snapshot wins and replaying would
// double-count.
func (s *Supervisor) replayHistory(sess *Session, events []agent.Event, applyUsage bool) int {
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
			if applyUsage {
				sess.updateContext(e.CurrentTokens, e.TokenLimit)
			}
		case agent.EventUsage:
			if applyUsage {
				sess.addUsage(e)
			}
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
				Preset: sess.Preset, Model: firstNonEmpty(sess.usage.Model, sess.Model), ReadOnly: sess.ReadOnly,
				AutoApprove: sess.autoApprove,
				Pinned:      sess.pinned, Category: sess.category, CreatedBy: sess.createdBy,
				NotifyTopic: sess.notifyTopic,
				BaseBranch:  sess.BaseBranch, BaseCommit: sess.BaseCommit,
				Status: string(sess.status), Created: sess.Created,
				InTokens: sess.usage.InputTokens, OutTokens: sess.usage.OutputTokens,
				NanoAiu: sess.usage.NanoAiu, CostUSD: sess.usage.CostUSD,
				CurrentTokens: sess.usage.CurrentTokens, TokenLimit: sess.usage.TokenLimit,
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
				Name: s.uniqueName(wt.CleanName(sv.Name)), Repo: sv.Repo, Dir: sv.Dir,
				Worktree: sv.Worktree, Branch: sv.Branch, Backend: backendName,
				Preset: sv.Preset, ReadOnly: sv.ReadOnly, Model: sv.Model,
				Created: sv.Created, BaseBranch: sv.BaseBranch, BaseCommit: sv.BaseCommit,
				pinned: sv.Pinned, category: sv.Category, createdBy: sv.CreatedBy,
				notifyTopic: sv.NotifyTopic,
				status:      StatusStarting, id: sv.ID,
			}
			s.mu.Lock()
			s.sessions = append(s.sessions, sess)
			s.mu.Unlock()
			s.log.Log(logx.Info, "session.adopted", map[string]any{"session": sess.Name, "id": sv.ID, "status": sv.Status})
			s.publish(bus.SessionAdopted, sess, map[string]any{"status": sv.Status})
			go s.resume(sess, sv)
			s.poke()
		}
	}
}

// Prompt sends a user message to the session. When the agent is waiting
// on a question (Copilot's ask_user), the message answers that question
// instead of starting a new turn.
func (s *Supervisor) Prompt(sess *Session, text string) error {
	strace("Prompt sess=%q backend=%s status=%s hasQuestion=%t textlen=%d",
		sess.Name, sess.Backend, sess.Status(), sess.HasQuestion(), len(text))
	if sess.HasQuestion() {
		strace("Prompt->answerQuestion sess=%q (consumed as question answer, not sent)", sess.Name)
		sess.appendEntry(EntryUser, text)
		sess.addHistory(text)
		sess.answerQuestion(text)
		sess.setStatus(StatusWorking)
		s.poke()
		return nil
	}
	ag := sess.agentSession()
	if ag == nil {
		strace("Prompt->ag-nil sess=%q (session is still starting)", sess.Name)
		return errors.New("session is still starting")
	}
	sess.appendEntry(EntryUser, text)
	sess.addHistory(text)
	sess.setStatus(StatusWorking)
	s.poke()
	strace("Prompt->ag.Send sess=%q", sess.Name)
	err := ag.Send(context.Background(), text)
	if err != nil {
		strace("Prompt->ag.Send err sess=%q err=%v", sess.Name, err)
		sess.setError(fmt.Sprintf("send failed: %v", err))
		s.poke()
	}
	return err
}

// questionFunc surfaces an agent's ask-the-user request and blocks until
// the user's next message answers it (or the session is cancelled). Only
// backends that can take an answer (Copilot) call this; Claude headless
// renders the question instead.
func (s *Supervisor) questionFunc(sess *Session) agent.QuestionFunc {
	return func(q agent.Question) (string, bool) {
		ch := sess.askQuestion(q)
		sess.appendEntry(EntrySystem, formatQuestion(q))
		sess.setStatus(StatusWaiting)
		s.log.Log(logx.Info, "session.question", map[string]any{
			"session": sess.Name, "options": len(q.Options), "freeform": q.AllowFreeform,
		})
		s.publish(bus.WaitingOnPermission, sess, map[string]any{"kind": "question", "summary": agent.Truncate(q.Prompt, 120)})
		s.poke()
		ans, ok := <-ch
		s.poke()
		return ans, ok
	}
}

// formatQuestion renders an agent question for the transcript so the user
// knows what to answer with their next message.
func formatQuestion(q agent.Question) string {
	var b strings.Builder
	b.WriteString("❓ " + q.Prompt)
	for i, opt := range q.Options {
		b.WriteString(fmt.Sprintf("\n   %d) %s", i+1, opt))
	}
	b.WriteString("\n→ reply with your answer")
	if len(q.Options) > 0 {
		b.WriteString(" (the option text, or its number)")
	}
	return b.String()
}

// SessionByName finds a session by its (unique) board name.
func (s *Supervisor) SessionByName(name string) *Session {
	for _, sess := range s.Sessions() {
		if sess.Name == name {
			return sess
		}
	}
	return nil
}

// PromptWith sends a user message with file attachments. Images go
// inline (base64 content blocks) when the backend supports it; anything
// else is saved under <session dir>/.atc-attachments and referenced by
// path in the prompt, which every backend can read with its file tools.
func (s *Supervisor) PromptWith(sess *Session, text string, atts []agent.Attachment) error {
	if len(atts) == 0 {
		return s.Prompt(sess, text)
	}
	ag := sess.agentSession()
	if ag == nil {
		return errors.New("session is still starting")
	}

	// Persist every attachment under the session dir so the UI can show
	// it for the life of the session (cleaned up on Kill). The saved
	// paths line up with atts by index.
	saved, err := s.saveAttachments(sess, atts)
	if err != nil {
		return err
	}

	var inline []agent.Attachment
	var diskRefs []string
	sender, canInline := ag.(agent.AttachmentSender)
	for i, a := range atts {
		if canInline && a.IsImage() {
			inline = append(inline, a) // also sent inline so the model sees it
		} else {
			diskRefs = append(diskRefs, saved[i].Path) // referenced by path for non-inline backends
		}
	}

	prompt := text
	if len(diskRefs) > 0 {
		prompt += "\n\nAttached files (read them from disk):\n- " + strings.Join(diskRefs, "\n- ")
	}

	names := make([]string, len(atts))
	for i, a := range atts {
		names[i] = a.Name
	}
	sess.appendEntryWith(Entry{Kind: EntryUser, Text: text + "\n📎 " + strings.Join(names, ", "), Attachments: saved})
	sess.addHistory(text)
	sess.setStatus(StatusWorking)
	s.log.Log(logx.Info, "session.prompt_attachments", map[string]any{
		"session": sess.Name, "inline": len(inline), "onDisk": len(diskRefs),
	})
	s.poke()

	if len(inline) > 0 {
		err = sender.SendWithAttachments(context.Background(), prompt, inline)
	} else {
		err = ag.Send(context.Background(), prompt)
	}
	if err != nil {
		sess.setError(fmt.Sprintf("send failed: %v", err))
		s.poke()
	}
	return err
}

// saveAttachments writes attachments into the session's working dir so
// the agent can read them and the UI can show them; returns one
// EntryAttachment per input (same order), with a path relative to the
// session dir.
func (s *Supervisor) saveAttachments(sess *Session, atts []agent.Attachment) ([]EntryAttachment, error) {
	sess.mu.Lock()
	base := sess.Dir
	sess.mu.Unlock()
	dir := filepath.Join(base, ".atc-attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Keep these atc scratch files out of git status and any `git add -A`
	// (e.g. on merge) so a session's screenshots aren't committed.
	if err := s.trees.IgnoreLocally(base, ".atc-attachments/"); err != nil {
		s.log.Log(logx.Info, "session.attachments_ignore_failed", map[string]any{"session": sess.Name, "err": err.Error()})
	}
	saved := make([]EntryAttachment, len(atts))
	stamp := time.Now().Format("150405")
	for i, a := range atts {
		name := fmt.Sprintf("%s-%d-%s", stamp, i+1, filepath.Base(a.Name))
		if err := os.WriteFile(filepath.Join(dir, name), a.Data, 0o644); err != nil {
			return nil, err
		}
		saved[i] = EntryAttachment{
			Name:      a.Name,
			MediaType: a.MediaType,
			Path:      filepath.Join(".atc-attachments", name),
		}
	}
	return saved, nil
}

// Rename changes a session's board name. The worktree, branch and
// resume ID are physical artifacts created at launch and are left
// untouched — only the display name changes. Returns an error if the
// new name (after cleaning) collides with another live session.
func (s *Supervisor) Rename(sess *Session, newName string) error {
	clean := wt.CleanName(newName)
	if clean == "" {
		return errors.New("name cannot be empty")
	}
	s.mu.Lock()
	for _, other := range s.sessions {
		if other != sess && other.Name == clean {
			s.mu.Unlock()
			return fmt.Errorf("a session named %q already exists", clean)
		}
	}
	s.mu.Unlock()
	sess.mu.Lock()
	old := sess.Name
	sess.Name = clean
	sess.mu.Unlock()
	if clean != old {
		s.log.Log(logx.Info, "session.rename", map[string]any{"from": old, "to": clean})
	}
	s.persist()
	s.poke()
	return nil
}

// SetPinned flips a session's pinned flag — board organization only,
// no effect on the agent — and persists it so it survives a restart.
func (s *Supervisor) SetPinned(sess *Session, on bool) {
	sess.setPinned(on)
	s.persist()
	s.poke()
}

// SetCategory assigns a session's category (empty clears it to
// uncategorized), trimming surrounding whitespace, and persists it.
func (s *Supervisor) SetCategory(sess *Session, category string) {
	sess.setCategory(strings.TrimSpace(category))
	s.persist()
	s.poke()
}

// Categories returns the distinct non-empty categories currently in use,
// sorted — for the new-session form, the TUI picker and the web sidebar.
func (s *Supervisor) Categories() []string {
	seen := map[string]bool{}
	var out []string
	for _, sess := range s.Sessions() {
		if c := sess.View().Category; c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// Note drops an informational line into the session transcript.
func (s *Supervisor) Note(sess *Session, text string) {
	sess.appendEntry(EntrySystem, text)
	s.poke()
}

// SwitchModel changes the model for the session's subsequent turns.
func (s *Supervisor) SwitchModel(sess *Session, model string) error {
	ag := sess.agentSession()
	if ag == nil {
		return errors.New("session is still starting")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ag.SetModel(ctx, model); err != nil {
		sess.appendEntry(EntryError, "model switch: "+err.Error())
		s.poke()
		return err
	}
	sess.mu.Lock()
	sess.Model = model
	sess.usage.Model = model
	sess.mu.Unlock()
	s.log.Log(logx.Info, "session.model_switch", map[string]any{"session": sess.Name, "model": model})
	sess.appendEntry(EntrySystem, "model switched to "+model)
	s.poke()
	return nil
}

// Abort cancels the session's current turn.
func (s *Supervisor) Abort(sess *Session) {
	s.log.Log(logx.Info, "session.abort", map[string]any{"session": sess.Name})
	if ag := sess.agentSession(); ag != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = ag.Abort(ctx)
	}
	// Unblock a waiting permission handler or question, if any.
	sess.RespondAll(agent.Cancel, "")
	sess.cancelQuestion()
}

// Kill tears a session down, forgets it in the resume store, and
// optionally removes its worktree.
func (s *Supervisor) Kill(sess *Session, removeWorktree bool) {
	s.log.Log(logx.Info, "session.kill", map[string]any{"session": sess.Name, "removeWorktree": removeWorktree})
	sess.RespondAll(agent.Cancel, "")
	sess.cancelQuestion()
	if ag := sess.agentSession(); ag != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = ag.Abort(ctx)
		cancel()
		_ = ag.Close()
	}
	sess.setStatus(StatusClosed)
	sess.mu.Lock()
	repo, worktree, branch, id, dir := sess.Repo, sess.Worktree, sess.Branch, sess.id, sess.Dir
	sess.mu.Unlock()
	if id != "" {
		s.mu.Lock()
		s.killed[id] = true
		s.mu.Unlock()
	}
	// Drop saved attachments. For a worktree session this lives inside the
	// worktree removed below, but direct/scratch sessions keep their dir,
	// so clean it up explicitly either way.
	if dir != "" {
		_ = os.RemoveAll(filepath.Join(dir, ".atc-attachments"))
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
		sess.RespondAll(agent.Cancel, "")
		sess.cancelQuestion()
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
	if e.Type == agent.EventError {
		s.log.Log(logx.Info, "session.error", map[string]any{"session": sess.Name, "errType": e.ErrType, "message": e.Text})
	} else if s.log.Enabled(logx.Debug) && e.Type != agent.EventTextDelta {
		// Deltas would flood the log; everything else is one line each.
		s.log.Log(logx.Debug, "event."+e.Type.String(), map[string]any{"session": sess.Name})
	}
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

// maxFileReadBytes caps the size of a file the web UI can fetch for
// preview (clickable file mentions).
const maxFileReadBytes = 2 << 20

// ReadSessionFile reads a file for preview, resolved relative to the
// session's working directory and confined to it: relative paths join
// the dir, absolute paths must already live under it, and any result
// that escapes the dir (via "..") is refused. Returns the base name and
// contents. This is the file equivalent of what the agent can already
// see, exposed read-only to the (token-gated, tailnet-bound) web UI.
func (s *Supervisor) ReadSessionFile(sess *Session, rel string) (string, []byte, error) {
	sess.mu.Lock()
	base := sess.Dir
	sess.mu.Unlock()
	if base == "" {
		return "", nil, errors.New("session has no directory yet")
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", nil, errors.New("path is required")
	}
	target := rel
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	target = filepath.Clean(target)
	if r, err := filepath.Rel(base, target); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", nil, errors.New("path is outside the session directory")
	}
	fi, err := os.Stat(target)
	if err != nil {
		return "", nil, err
	}
	if !fi.Mode().IsRegular() {
		return "", nil, errors.New("not a regular file")
	}
	if fi.Size() > maxFileReadBytes {
		return "", nil, fmt.Errorf("file is larger than %dMB", maxFileReadBytes>>20)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", nil, err
	}
	return filepath.Base(target), data, nil
}

// maxAttachmentBytes caps an attachment the web UI can fetch back. It
// matches the per-file upload limit so anything that was accepted can be
// served again.
const maxAttachmentBytes = 10 << 20

// ReadAttachment serves the bytes of a previously saved attachment,
// confined to the session's .atc-attachments dir (rel must point inside
// it; "../" escapes are refused). Returns the base name and raw bytes;
// the caller decides the content type.
func (s *Supervisor) ReadAttachment(sess *Session, rel string) (string, []byte, error) {
	sess.mu.Lock()
	base := sess.Dir
	sess.mu.Unlock()
	if base == "" {
		return "", nil, errors.New("session has no directory yet")
	}
	root := filepath.Join(base, ".atc-attachments")
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", nil, errors.New("path is required")
	}
	target := rel
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	target = filepath.Clean(target)
	if r, err := filepath.Rel(root, target); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", nil, errors.New("path is outside the attachments directory")
	}
	fi, err := os.Stat(target)
	if err != nil {
		return "", nil, err
	}
	if !fi.Mode().IsRegular() {
		return "", nil, errors.New("not a regular file")
	}
	if fi.Size() > maxAttachmentBytes {
		return "", nil, fmt.Errorf("file is larger than %dMB", maxAttachmentBytes>>20)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", nil, err
	}
	return filepath.Base(target), data, nil
}

// Diff shows what changed: for a worktree session, relative to the
// commit it branched from; for a session running directly in the repo,
// the repo's current uncommitted changes (vs HEAD) plus untracked files.
// The latter isn't scoped to this session — it's the working tree as it
// stands — but it's the "git diff" view of the work.
func (s *Supervisor) Diff(sess *Session) (string, error) {
	sess.mu.Lock()
	dir, base := sess.Worktree, sess.BaseCommit
	if dir == "" {
		dir, base = sess.Dir, "" // direct-in-repo: empty base → diff vs HEAD
	}
	sess.mu.Unlock()
	if dir == "" {
		return "", errors.New("session has no directory yet")
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

		plog := func(answer, via string) {
			s.log.Log(logx.Info, "permission.answered", map[string]any{
				"session": sess.Name, "kind": req.Kind, "summary": agent.Truncate(req.Summary, 80),
				"answer": answer, "via": via,
			})
		}
		verdict, reason := policy.Evaluate(approval, req)
		switch verdict {
		case policy.Deny:
			plog("deny", "deny-list: "+reason)
			sess.appendEntry(EntrySystem, "⛔ denied ("+reason+"): "+req.Summary)
			s.poke()
			return agent.Deny, "blocked by atc deny-list: " + reason
		case policy.Allow:
			plog("approve", "allow-all")
			sess.appendEntry(EntrySystem, "auto-approved: "+req.Summary)
			s.poke()
			return agent.ApproveOnce, ""
		}
		if sess.approvedByRule(req) {
			plog("approve", "session-rule")
			sess.appendEntry(EntrySystem, "auto-approved (session rule): "+req.Summary)
			s.poke()
			return agent.ApproveOnce, ""
		}

		if s.headless {
			plog("deny", "headless")
			sess.appendEntry(EntrySystem, "⛔ denied (headless run): "+req.Summary)
			s.poke()
			return agent.Deny, "headless run (atc run): not pre-approved by the preset; use an allow-all preset for unattended runs"
		}

		p := &Permission{Kind: req.Kind, Summary: req.Summary, Detail: req.Detail, respond: make(chan permissionAnswer, 1)}
		sess.enqueuePending(p)
		s.log.Log(logx.Info, "permission.enqueued", map[string]any{
			"session": sess.Name, "kind": req.Kind, "summary": agent.Truncate(req.Summary, 80),
			"queued": sess.View().PendingCount,
		})
		sess.appendEntry(EntrySystem, "permission requested: "+req.Summary)
		s.publish(bus.WaitingOnPermission, sess, map[string]any{"kind": req.Kind, "summary": req.Summary})
		s.poke()

		ans := <-p.respond
		plog([...]string{"deny", "approve", "approve-session", "cancel"}[ans.decision], "user")
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
