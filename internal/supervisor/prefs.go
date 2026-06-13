package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// prefs holds small, frontend-agnostic UI preferences that should stick
// across restarts — distinct from user-authored config. Stored at
// ~/.atc/prefs.json. Best-effort: any read/write failure just means a
// preference doesn't persist.
type prefs struct {
	LastBackend string `json:"lastBackend,omitempty"`
}

type prefsStore struct{ path string }

func defaultPrefsStore() prefsStore {
	home, err := os.UserHomeDir()
	if err != nil {
		return prefsStore{}
	}
	return prefsStore{path: filepath.Join(home, ".atc", "prefs.json")}
}

func (ps prefsStore) load() prefs {
	if ps.path == "" {
		return prefs{}
	}
	data, err := os.ReadFile(ps.path)
	if err != nil {
		return prefs{}
	}
	var p prefs
	_ = json.Unmarshal(data, &p)
	return p
}

func (ps prefsStore) save(p prefs) {
	if ps.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(ps.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := ps.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, ps.path)
	}
}
