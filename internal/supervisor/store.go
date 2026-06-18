package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// store persists minimal session metadata (~/.atc/sessions.json) so atc
// can resume sessions on the next run. The Copilot runtime keeps the
// conversation state itself; atc only remembers the IDs and where each
// session was running.
type store struct {
	path string
}

func defaultStore() store {
	home, err := os.UserHomeDir()
	if err != nil {
		return store{} // persistence disabled
	}
	return store{path: filepath.Join(home, ".atc", "sessions.json")}
}

type savedSession struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Repo         string    `json:"repo"`
	Dir          string    `json:"dir"`
	Worktree     string    `json:"worktree,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	Backend      string    `json:"backend,omitempty"` // empty = copilot (pre-backend files)
	Preset       string    `json:"preset,omitempty"`
	Model        string    `json:"model,omitempty"`
	ReadOnly     bool      `json:"readOnly,omitempty"`
	AutoApprove  bool      `json:"autoApprove,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	Category     string    `json:"category,omitempty"`
	CreatedBy    string    `json:"createdBy,omitempty"`    // per-device clientId of the creator
	NotifyTopic  string    `json:"notifyTopic,omitempty"`  // ntfy topic of the creator's device
	ScheduleName string    `json:"scheduleName,omitempty"` // schedule that launched it; "" if manual
	BaseBranch   string    `json:"baseBranch,omitempty"`
	BaseCommit   string    `json:"baseCommit,omitempty"`
	Status       string    `json:"status,omitempty"` // session status at last persist
	Created      time.Time `json:"created"`

	// Usage snapshot at last persist — restored on resume because the
	// runtimes' own logs don't reliably persist usage events.
	InTokens      int64   `json:"inTokens,omitempty"`
	OutTokens     int64   `json:"outTokens,omitempty"`
	NanoAiu       float64 `json:"nanoAiu,omitempty"`
	CostUSD       float64 `json:"costUsd,omitempty"`
	CurrentTokens int64   `json:"currentTokens,omitempty"`
	TokenLimit    int64   `json:"tokenLimit,omitempty"`
}

// settled reports whether the session finished its work — the signal a
// running TUI uses to adopt sessions written by another atc process
// (e.g. a Task Scheduler `atc run`). Empty status means a pre-status
// file; treat as settled.
func (sv savedSession) settled() bool {
	return sv.Status == "" || sv.Status == "done" || sv.Status == "error"
}

// load returns the saved sessions; any read/parse problem just means
// nothing to resume.
func (st store) load() []savedSession {
	if st.path == "" {
		return nil
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return nil
	}
	var out []savedSession
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// save writes atomically (temp file + rename) — a TUI and a headless
// `atc run` may both touch this file.
func (st store) save(sessions []savedSession) error {
	if st.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
