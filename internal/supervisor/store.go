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
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Repo     string    `json:"repo"`
	Dir      string    `json:"dir"`
	Worktree string    `json:"worktree,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Backend  string    `json:"backend,omitempty"` // empty = copilot (pre-backend files)
	Preset   string    `json:"preset,omitempty"`
	Model    string    `json:"model,omitempty"`
	Created  time.Time `json:"created"`
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
	return os.WriteFile(st.path, data, 0o644)
}
