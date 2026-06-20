// Package web serves atc's optional browser UI: the same supervisor
// the TUI drives, exposed over HTTP for use from a phone or another
// machine. It binds to localhost; reaching it from elsewhere is meant
// to go through `tailscale serve` (tailnet-only HTTPS), never a public
// listener. Every API call requires a bearer token.
//
// The browser UI adds the one thing the terminal can't do well:
// attaching images to a prompt (file picker or clipboard paste).
package web

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

//go:embed index.html
var indexHTML []byte

//go:embed sw.js
var serviceWorkerJS []byte

const (
	maxUploadBytes = 32 << 20 // whole multipart form
	maxFileBytes   = 10 << 20 // per attachment
	maxFiles       = 6
)

type Server struct {
	sup      *supervisor.Supervisor
	cfg      *config.Config
	token    string
	mux      *http.ServeMux
	apkCache apkHashCache

	fileCacheMu sync.Mutex
	fileCache   map[string]fileCacheEntry // working dir -> cached file walk
}

// New builds the server. An empty token gets a random per-run one;
// read it back with Token() to print the access URL.
func New(sup *supervisor.Supervisor, cfg *config.Config, token string) *Server {
	if token == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		token = hex.EncodeToString(b)
	}
	s := &Server{sup: sup, cfg: cfg, token: token, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Token() string { return s.token }

// Start listens on addr and serves in the background, returning the
// browseable URL (including the token, so it can be opened directly).
func (s *Server) Start(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	host := ln.Addr().String()
	if h, p, err := net.SplitHostPort(host); err == nil && (h == "0.0.0.0" || h == "::") {
		host = net.JoinHostPort("127.0.0.1", p)
	}
	return fmt.Sprintf("http://%s/?token=%s", host, s.token), nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	// The service worker is served unauthenticated (it carries no data and
	// no token); it must live at the origin root so its scope covers "/".
	s.mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Service-Worker-Allowed", "/")
		_, _ = w.Write(serviceWorkerJS)
	})
	api := func(pattern string, h http.HandlerFunc) {
		s.mux.Handle(pattern, s.auth(h))
	}
	api("GET /api/meta", s.handleMeta)
	api("GET /api/schedules", s.handleSchedules)
	api("GET /api/complete", s.handleCompleteDir)
	api("GET /api/sessions", s.handleList)
	api("POST /api/sessions", s.handleCreate)
	api("GET /api/sessions/{name}", s.handleGet)
	api("POST /api/sessions/{name}/prompt", s.handlePrompt)
	api("POST /api/sessions/{name}/respond", s.handleRespond)
	api("POST /api/sessions/{name}/abort", s.handleAbort)
	api("POST /api/sessions/{name}/kill", s.handleKill)
	api("POST /api/sessions/{name}/auto", s.handleAuto)
	api("POST /api/sessions/{name}/pin", s.handlePin)
	api("POST /api/sessions/{name}/category", s.handleCategory)
	api("POST /api/sessions/{name}/rename", s.handleRename)
	api("GET /api/sessions/{name}/diff", s.handleDiff)
	api("POST /api/sessions/{name}/merge", s.handleMerge)
	api("POST /api/sessions/{name}/model", s.handleModel)
	api("GET /api/sessions/{name}/file", s.handleFile)
	api("GET /api/sessions/{name}/complete", s.handleComplete)
	api("GET /api/sessions/{name}/attachment", s.handleAttachment)
	api("GET /api/app/latest", s.handleAppLatest)
	api("GET /api/app/download", s.handleAppDownload)
	api("GET /api/app/qr", s.handleAppQR)
}

// auth accepts the token as a bearer header (fetch calls) or query
// parameter (the initial page link).
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || got == r.Header.Get("Authorization") {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			jsonError(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next(w, r)
	})
}

// --- DTOs ----------------------------------------------------------

type sessionJSON struct {
	Name         string    `json:"name"`
	Repo         string    `json:"repo"`
	Dir          string    `json:"dir"`
	Branch       string    `json:"branch,omitempty"`
	BaseBranch   string    `json:"baseBranch,omitempty"`
	Worktree     bool      `json:"worktree"`
	Backend      string    `json:"backend"`
	Preset       string    `json:"preset,omitempty"`
	Model        string    `json:"model,omitempty"`
	Status       string    `json:"status"`
	Intent       string    `json:"intent,omitempty"`
	Err          string    `json:"err,omitempty"`
	LastLine     string    `json:"lastLine,omitempty"`
	ReadOnly     bool      `json:"readOnly"`
	AutoOK       bool      `json:"autoApprove"`
	Pinned       bool      `json:"pinned,omitempty"`
	Category     string    `json:"category,omitempty"`
	CreatedBy    string    `json:"createdBy,omitempty"`
	ScheduleName string    `json:"scheduleName,omitempty"`
	Created      time.Time `json:"created"`
	SinceEvent   float64   `json:"sinceEventSec,omitempty"`

	InTokens      int64   `json:"inTokens"`
	OutTokens     int64   `json:"outTokens"`
	CostUSD       float64 `json:"costUsd"`
	NanoAiu       float64 `json:"nanoAiu"`
	CurrentTokens int64   `json:"currentTokens"`
	TokenLimit    int64   `json:"tokenLimit"`

	Pending      *permissionJSON `json:"pending,omitempty"`
	PendingCount int             `json:"pendingCount,omitempty"`
	Question     *questionJSON   `json:"question,omitempty"`
	Limits       *limitsJSON     `json:"limits,omitempty"`
}

type limitsJSON struct {
	Windows []limitWindowJSON `json:"windows,omitempty"`
	Text    string            `json:"text,omitempty"`
	AgeSec  float64           `json:"ageSec"`
}

type limitWindowJSON struct {
	Label  string  `json:"label"`
	Pct    float64 `json:"pct"`
	Resets string  `json:"resets,omitempty"`
	Used   int64   `json:"used,omitempty"`
	Max    int64   `json:"max,omitempty"`
}

type permissionJSON struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
	Detail  []string `json:"detail,omitempty"`
}

type questionJSON struct {
	Prompt        string   `json:"prompt"`
	Options       []string `json:"options,omitempty"`
	OptionDetails []string `json:"optionDetails,omitempty"`
	AllowFreeform bool     `json:"allowFreeform"`
	MultiSelect   bool     `json:"multiSelect,omitempty"`
}

type entryJSON struct {
	Kind        string           `json:"kind"`
	Text        string           `json:"text"`
	Partial     bool             `json:"partial,omitempty"`
	Attachments []attachmentJSON `json:"attachments,omitempty"`
}

type attachmentJSON struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // media type, e.g. "image/png"
	URL     string `json:"url"`  // fetch the saved bytes back
	IsImage bool   `json:"isImage"`
}

func (s *Server) toSessionJSON(v supervisor.SessionView) sessionJSON {
	out := sessionJSON{
		Name: v.Name, Repo: v.Repo, Dir: v.Dir, Branch: v.Branch, BaseBranch: v.BaseBranch,
		Worktree: v.Worktree != "", Backend: v.Backend, Preset: v.Preset,
		Model: v.Model, Status: string(v.Status), Intent: v.Intent,
		Err: v.Err, LastLine: v.LastLine, ReadOnly: v.ReadOnly,
		AutoOK: v.AutoApprove, Pinned: v.Pinned, Category: v.Category,
		CreatedBy: v.CreatedBy, ScheduleName: v.ScheduleName, Created: v.Created,
		SinceEvent:    v.SinceEvent.Seconds(),
		InTokens:      v.Usage.InputTokens,
		OutTokens:     v.Usage.OutputTokens,
		CostUSD:       v.Usage.CostUSD,
		NanoAiu:       v.Usage.NanoAiu,
		CurrentTokens: v.Usage.CurrentTokens,
		TokenLimit:    v.Usage.TokenLimit,
	}
	if v.Pending != nil {
		out.Pending = &permissionJSON{Kind: v.Pending.Kind, Summary: v.Pending.Summary, Detail: v.Pending.Detail}
		out.PendingCount = v.PendingCount
	}
	if v.Question != nil {
		out.Question = &questionJSON{Prompt: v.Question.Prompt, Options: v.Question.Options, OptionDetails: v.Question.OptionDetails, AllowFreeform: v.Question.AllowFreeform, MultiSelect: v.Question.MultiSelect}
	}
	// Account usage is global, not per-session: every session reports the same
	// most-recent snapshot, so the badge stays put when switching chats.
	if lim := s.sup.Limits(); !lim.AsOf.IsZero() {
		lj := &limitsJSON{Text: lim.Text, AgeSec: time.Since(lim.AsOf).Seconds()}
		for _, w := range lim.Windows {
			lj.Windows = append(lj.Windows, limitWindowJSON{Label: w.Label, Pct: w.Pct, Resets: w.Resets, Used: w.Used, Max: w.Max})
		}
		out.Limits = lj
	}
	return out
}

func kindString(k supervisor.EntryKind) string {
	switch k {
	case supervisor.EntryUser:
		return "user"
	case supervisor.EntryAssistant:
		return "assistant"
	case supervisor.EntryTool:
		return "tool"
	case supervisor.EntrySystem:
		return "system"
	case supervisor.EntryError:
		return "error"
	}
	return "other"
}

// --- handlers ------------------------------------------------------

func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	today, month := s.sup.Spend()
	presets := make([]string, 0, len(s.cfg.Presets))
	for name := range s.cfg.Presets {
		presets = append(presets, name)
	}
	defaultRepo := s.cfg.DefaultRepo
	if defaultRepo == "" && len(s.cfg.Repos) > 0 {
		defaultRepo = s.cfg.Repos[0]
	}
	defaultBackend := s.sup.PreferredBackend()
	writeJSON(w, map[string]any{
		"repos":              s.cfg.Repos,
		"backends":           s.sup.Backends(),
		"presets":            presets,
		"agents":             s.cfg.AgentNames(),
		"categories":         s.sup.Categories(),
		"defaultRepo":        defaultRepo,
		"defaultBackend":     defaultBackend,
		"defaultModel":       s.cfg.Model,
		"defaultAutoApprove": s.cfg.DefaultAutoApprove,
		"spend": map[string]any{
			"todayUsd": today.CostUSD, "todayAiu": today.NanoAiu / 1e9,
			"monthUsd": month.CostUSD, "monthAiu": month.NanoAiu / 1e9,
		},
		"ntfy": map[string]any{
			"enabled": s.cfg.Ntfy.Enabled,
			"server":  ntfyServerURL(s.cfg.Ntfy),
			"topic":   s.cfg.Ntfy.Topic, // server default topic (stable, catches all sessions)
		},
	})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	sessions := s.sup.Sessions()
	out := make([]sessionJSON, 0, len(sessions))
	for _, sess := range sessions {
		v := sess.View()
		// Schedule-originated sessions drop off the board once they settle —
		// they're reachable through the Scheduled section (each schedule's
		// run timeline links to them). Still-running or attention-needing
		// ones stay visible so they aren't missed. Mirrors the TUI board.
		if v.ScheduleName != "" && boardSettled(v.Status) {
			continue
		}
		out = append(out, s.toSessionJSON(v))
	}
	writeJSON(w, out)
}

// boardSettled reports whether a session has reached a terminal state and
// so may be hidden from the live board.
func boardSettled(st supervisor.Status) bool {
	return st == supervisor.StatusDone || st == supervisor.StatusError || st == supervisor.StatusClosed
}

type scheduleJSON struct {
	Name        string            `json:"name"`
	Cron        string            `json:"cron"`
	Repo        string            `json:"repo"`
	Preset      string            `json:"preset,omitempty"`
	Worktree    bool              `json:"worktree,omitempty"`
	Write       bool              `json:"write"`
	HasPrecheck bool              `json:"hasPrecheck"`
	NextFire    *time.Time        `json:"nextFire,omitempty"`
	LastUpdate  *time.Time        `json:"lastUpdate,omitempty"`
	Runs        []scheduleRunJSON `json:"runs"`
}

type scheduleRunJSON struct {
	Time    time.Time `json:"time"`
	Result  string    `json:"result"` // "updated" | "no-update" | "error"
	Session string    `json:"session,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

func (s *Server) handleSchedules(w http.ResponseWriter, _ *http.Request) {
	views := s.sup.Schedules()
	out := make([]scheduleJSON, 0, len(views))
	for _, v := range views {
		j := scheduleJSON{
			Name: v.Name, Cron: v.Cron, Repo: v.Repo, Preset: v.Preset,
			Worktree: v.Worktree, Write: v.Write, HasPrecheck: v.HasPrecheck,
			Runs: make([]scheduleRunJSON, 0, len(v.Runs)),
		}
		if !v.NextFire.IsZero() {
			t := v.NextFire
			j.NextFire = &t
		}
		if !v.LastUpdate.IsZero() {
			t := v.LastUpdate
			j.LastUpdate = &t
		}
		for _, r := range v.Runs {
			j.Runs = append(j.Runs, scheduleRunJSON{Time: r.Time, Result: r.Result, Session: r.Session, Detail: r.Detail})
		}
		out = append(out, j)
	}
	writeJSON(w, out)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		NameHint    string `json:"nameHint"`
		Repo        string `json:"repo"`
		Backend     string `json:"backend"`
		Preset      string `json:"preset"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		Prompt      string `json:"prompt"`
		Worktree    bool   `json:"worktree"`
		ReadOnly    bool   `json:"readOnly"`
		AutoApprove bool   `json:"autoApprove"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	// "__scratch__" is the "no repo" option: run the agent in a throwaway
	// scratch directory (~/.atc/scratch) for tasks that aren't tied to a
	// codebase. It isn't a git repo, so worktree mode doesn't apply.
	repo, scratch := req.Repo, false
	if repo == "" {
		jsonError(w, http.StatusBadRequest, "repo is required")
		return
	}
	if repo == "__scratch__" {
		dir, err := scratchDir()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "scratch dir: "+err.Error())
			return
		}
		repo, scratch = dir, true
	} else if !s.cfg.HasRepo(repo) {
		// A token-holder may only start sessions in the operator's configured
		// repos (or scratch) — never an arbitrary path on the host. The UI
		// offers only these, so this rejects forged/out-of-band requests.
		jsonError(w, http.StatusForbidden, "repo is not in the configured repos list")
		return
	}
	sess, err := s.sup.NewSession(supervisor.NewSessionOptions{
		Name: req.Name, NameHint: req.NameHint, Repo: repo, Backend: req.Backend,
		Preset: req.Preset, Agent: req.Agent, Model: req.Model, Prompt: req.Prompt,
		UseWorktree: req.Worktree && !scratch, ReadOnly: req.ReadOnly, AutoApprove: req.AutoApprove,
		CreatedBy: clientID(r), NotifyTopic: notifyTopic(r),
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, s.toSessionJSON(sess.View()))
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) *supervisor.Session {
	sess := s.sup.SessionByName(r.PathValue("name"))
	if sess == nil {
		jsonError(w, http.StatusNotFound, "no session named "+r.PathValue("name"))
	}
	return sess
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	name := sess.Name
	entries := sess.Transcript()
	transcript := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		ej := entryJSON{Kind: kindString(e.Kind), Text: e.Text, Partial: e.Partial}
		for _, a := range e.Attachments {
			ej.Attachments = append(ej.Attachments, attachmentJSON{
				Name:    a.Name,
				Type:    a.MediaType,
				IsImage: strings.HasPrefix(a.MediaType, "image/"),
				URL: "/api/sessions/" + url.PathEscape(name) +
					"/attachment?path=" + url.QueryEscape(a.Path),
			})
		}
		transcript = append(transcript, ej)
	}
	writeJSON(w, map[string]any{
		"session":    s.toSessionJSON(sess.View()),
		"transcript": transcript,
	})
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	text, atts, err := readPrompt(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(text) == "" && len(atts) == 0 {
		jsonError(w, http.StatusBadRequest, "empty prompt")
		return
	}
	if strings.TrimSpace(text) == "" {
		text = "(see attached)"
	}
	if err := s.sup.PromptWith(sess, text, atts); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "attachments": len(atts)})
}

// readPrompt accepts either a JSON body {"text": …} or a multipart
// form with a "text" field and "files" parts.
func readPrompt(r *http.Request) (string, []agent.Attachment, error) {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/") {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			return "", nil, errors.New("bad JSON: " + err.Error())
		}
		return req.Text, nil, nil
	}
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		return "", nil, errors.New("upload too large or malformed: " + err.Error())
	}
	text := r.FormValue("text")
	files := r.MultipartForm.File["files"]
	if len(files) > maxFiles {
		return "", nil, fmt.Errorf("too many attachments (max %d)", maxFiles)
	}
	var atts []agent.Attachment
	for _, fh := range files {
		if fh.Size > maxFileBytes {
			return "", nil, fmt.Errorf("%s is larger than %dMB", fh.Filename, maxFileBytes>>20)
		}
		f, err := fh.Open()
		if err != nil {
			return "", nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		_ = f.Close()
		if err != nil {
			return "", nil, err
		}
		mt := fh.Header.Get("Content-Type")
		if mt == "" || mt == "application/octet-stream" {
			mt = http.DetectContentType(data)
		}
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
		name := fh.Filename
		if name == "" {
			name = "pasted"
		}
		atts = append(atts, agent.Attachment{Name: name, MediaType: mt, Data: data})
	}
	return text, atts, nil
}

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		Decision string `json:"decision"`
		Feedback string `json:"feedback"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	var d agent.Decision
	switch req.Decision {
	case "approve":
		d = agent.ApproveOnce
	case "approve-session":
		d = agent.ApproveSession
	case "deny":
		d = agent.Deny
	case "cancel":
		d = agent.Cancel
	default:
		jsonError(w, http.StatusBadRequest, "decision must be approve|approve-session|deny|cancel")
		return
	}
	if req.Feedback == "" && d == agent.Deny {
		req.Feedback = "denied by user in atc web"
	}
	sess.Respond(d, req.Feedback)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	s.sup.Abort(sess)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		RemoveWorktree bool `json:"removeWorktree"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	s.sup.Kill(sess, req.RemoveWorktree)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAuto(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	sess.SetAutoApprove(req.On)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	s.sup.SetPinned(sess, req.Pinned)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleCategory(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		Category string `json:"category"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	s.sup.SetCategory(sess, req.Category)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if err := s.sup.Rename(sess, req.Name); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "name": sess.View().Name})
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	diff, err := s.sup.Diff(sess)
	if err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"diff": diff})
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	if err := s.sup.Merge(sess); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		jsonError(w, http.StatusBadRequest, "model is required")
		return
	}
	if err := s.sup.SwitchModel(sess, strings.TrimSpace(req.Model)); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "model": sess.View().Model})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	name, data, err := s.sup.ReadSessionFile(sess, r.URL.Query().Get("path"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"name":     name,
		"content":  string(data),
		"markdown": strings.HasSuffix(strings.ToLower(name), ".md"),
	})
}

// handleAttachment serves the raw bytes of a saved prompt attachment so
// the UI can show the image the user sent. Confined to the session's
// .atc-attachments dir by the supervisor.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if sess == nil {
		return
	}
	name, data, err := s.sup.ReadAttachment(sess, r.URL.Query().Get("path"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// clientID returns the caller's opaque per-device id from the X-Atc-Client
// header (set by the web UI / app), trimmed and length-capped so a rogue
// client can't bloat the session store. Empty when absent (e.g. curl, TUI).
func clientID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-Atc-Client"))
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

// notifyTopic returns the caller's per-device ntfy topic from the
// X-Atc-Notify-Topic header, trimmed and length-capped. Empty when absent.
func notifyTopic(r *http.Request) string {
	t := strings.TrimSpace(r.Header.Get("X-Atc-Notify-Topic"))
	if len(t) > 128 {
		t = t[:128]
	}
	return t
}

// ntfyServerURL is the ntfy base URL the web UI shows for subscribing —
// SubscribeURL if set, else Server, else ntfy.sh — without a trailing slash.
func ntfyServerURL(c config.Ntfy) string {
	s := strings.TrimRight(strings.TrimSpace(c.SubscribeURL), "/")
	if s == "" {
		s = strings.TrimRight(strings.TrimSpace(c.Server), "/")
	}
	if s == "" {
		return "https://ntfy.sh"
	}
	return s
}

// scratchDir is the directory used for "no repo" sessions. Created on demand.
func scratchDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".atc", "scratch")
	return dir, os.MkdirAll(dir, 0o755)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
