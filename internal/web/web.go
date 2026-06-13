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
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

//go:embed index.html
var indexHTML []byte

const (
	maxUploadBytes = 32 << 20 // whole multipart form
	maxFileBytes   = 10 << 20 // per attachment
	maxFiles       = 6
)

type Server struct {
	sup   *supervisor.Supervisor
	cfg   *config.Config
	token string
	mux   *http.ServeMux
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
	api := func(pattern string, h http.HandlerFunc) {
		s.mux.Handle(pattern, s.auth(h))
	}
	api("GET /api/meta", s.handleMeta)
	api("GET /api/sessions", s.handleList)
	api("POST /api/sessions", s.handleCreate)
	api("GET /api/sessions/{name}", s.handleGet)
	api("POST /api/sessions/{name}/prompt", s.handlePrompt)
	api("POST /api/sessions/{name}/respond", s.handleRespond)
	api("POST /api/sessions/{name}/abort", s.handleAbort)
	api("POST /api/sessions/{name}/kill", s.handleKill)
	api("POST /api/sessions/{name}/auto", s.handleAuto)
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
	Name       string    `json:"name"`
	Repo       string    `json:"repo"`
	Dir        string    `json:"dir"`
	Branch     string    `json:"branch,omitempty"`
	Worktree   bool      `json:"worktree"`
	Backend    string    `json:"backend"`
	Preset     string    `json:"preset,omitempty"`
	Model      string    `json:"model,omitempty"`
	Status     string    `json:"status"`
	Intent     string    `json:"intent,omitempty"`
	Err        string    `json:"err,omitempty"`
	LastLine   string    `json:"lastLine,omitempty"`
	ReadOnly   bool      `json:"readOnly"`
	AutoOK     bool      `json:"autoApprove"`
	Created    time.Time `json:"created"`
	SinceEvent float64   `json:"sinceEventSec,omitempty"`

	InTokens      int64   `json:"inTokens"`
	OutTokens     int64   `json:"outTokens"`
	CostUSD       float64 `json:"costUsd"`
	NanoAiu       float64 `json:"nanoAiu"`
	CurrentTokens int64   `json:"currentTokens"`
	TokenLimit    int64   `json:"tokenLimit"`

	Pending      *permissionJSON `json:"pending,omitempty"`
	PendingCount int             `json:"pendingCount,omitempty"`
}

type permissionJSON struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
	Detail  []string `json:"detail,omitempty"`
}

type entryJSON struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Partial bool   `json:"partial,omitempty"`
}

func toSessionJSON(v supervisor.SessionView) sessionJSON {
	out := sessionJSON{
		Name: v.Name, Repo: v.Repo, Dir: v.Dir, Branch: v.Branch,
		Worktree: v.Worktree != "", Backend: v.Backend, Preset: v.Preset,
		Model: v.Model, Status: string(v.Status), Intent: v.Intent,
		Err: v.Err, LastLine: v.LastLine, ReadOnly: v.ReadOnly,
		AutoOK: v.AutoApprove, Created: v.Created,
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
	defaultBackend := s.cfg.DefaultBackend
	if defaultBackend == "" {
		defaultBackend = supervisor.DefaultBackend
	}
	writeJSON(w, map[string]any{
		"repos":              s.cfg.Repos,
		"backends":           s.sup.Backends(),
		"presets":            presets,
		"defaultRepo":        defaultRepo,
		"defaultBackend":     defaultBackend,
		"defaultModel":       s.cfg.Model,
		"defaultAutoApprove": s.cfg.DefaultAutoApprove,
		"spend": map[string]any{
			"todayUsd": today.CostUSD, "todayAiu": today.NanoAiu / 1e9,
			"monthUsd": month.CostUSD, "monthAiu": month.NanoAiu / 1e9,
		},
	})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	sessions := s.sup.Sessions()
	out := make([]sessionJSON, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, toSessionJSON(sess.View()))
	}
	writeJSON(w, out)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Repo        string `json:"repo"`
		Backend     string `json:"backend"`
		Preset      string `json:"preset"`
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
	sess, err := s.sup.NewSession(supervisor.NewSessionOptions{
		Name: req.Name, Repo: req.Repo, Backend: req.Backend,
		Preset: req.Preset, Model: req.Model, Prompt: req.Prompt,
		UseWorktree: req.Worktree, ReadOnly: req.ReadOnly, AutoApprove: req.AutoApprove,
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, toSessionJSON(sess.View()))
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
	entries := sess.Transcript()
	transcript := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		transcript = append(transcript, entryJSON{Kind: kindString(e.Kind), Text: e.Text, Partial: e.Partial})
	}
	writeJSON(w, map[string]any{
		"session":    toSessionJSON(sess.View()),
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
