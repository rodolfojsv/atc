package web

// Notes and docs: a single free-form markdown scratchpad the web UI autosaves
// (~/.atc/notes.md), plus a read-only browser over the operator-configured
// DocFolders. Both are rendered client-side by the page's existing markdown
// renderer; the server only stores/serves raw text and is strict about never
// serving a file outside a configured folder.

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	maxNoteBytes = 4 << 20 // scratchpad cap
	maxDocBytes  = 2 << 20 // per rendered doc
	maxDocFiles  = 2000    // listing cap per folder
	maxDocDepth  = 6       // directory recursion cap
)

// notesPath is ~/.atc/notes.md. Empty when the home dir can't be resolved, in
// which case notes silently no-op (consistent with the other ~/.atc stores).
func notesPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".atc", "notes.md")
}

func (s *Server) handleNotesGet(w http.ResponseWriter, _ *http.Request) {
	text := ""
	if p := notesPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			text = string(b)
		}
	}
	writeJSON(w, map[string]string{"text": text})
}

func (s *Server) handleNotesPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxNoteBytes)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	p := notesPath()
	if p == "" {
		jsonError(w, http.StatusInternalServerError, "no home dir for notes")
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		jsonError(w, http.StatusInternalServerError, "notes dir: "+err.Error())
		return
	}
	// Atomic temp+rename, like the other ~/.atc stores, so a crash mid-save
	// can't truncate the scratchpad.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(req.Text), 0o644); err != nil {
		jsonError(w, http.StatusInternalServerError, "writing notes: "+err.Error())
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		jsonError(w, http.StatusInternalServerError, "saving notes: "+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// docFolderRoot resolves the ?folder=<index> query into an absolute folder root
// from config. ok is false for an unknown/out-of-range index, so a forged
// request can't reach an arbitrary path.
func (s *Server) docFolderRoot(r *http.Request) (string, bool) {
	idx, err := strconv.Atoi(r.URL.Query().Get("folder"))
	if err != nil || idx < 0 || idx >= len(s.cfg.Web.DocFolders) {
		return "", false
	}
	root, err := filepath.Abs(expandHome(s.cfg.Web.DocFolders[idx].Path))
	if err != nil {
		return "", false
	}
	return root, true
}

func (s *Server) handleDocList(w http.ResponseWriter, r *http.Request) {
	root, ok := s.docFolderRoot(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "unknown doc folder")
		return
	}
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // skip dotdirs (.git, .obsidian, …)
			}
			if rel != "." && strings.Count(rel, string(os.PathSeparator))+1 >= maxDocDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			if rel, err := filepath.Rel(root, p); err == nil {
				files = append(files, rel)
			}
		}
		if len(files) >= maxDocFiles {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	writeJSON(w, map[string]any{"files": files})
}

func (s *Server) handleDocFile(w http.ResponseWriter, r *http.Request) {
	root, ok := s.docFolderRoot(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "unknown doc folder")
		return
	}
	full := filepath.Clean(filepath.Join(root, r.URL.Query().Get("path")))
	// Confine to the folder root: a cleaned join that doesn't sit under root
	// escaped via "..".
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		jsonError(w, http.StatusForbidden, "path escapes the doc folder")
		return
	}
	if !strings.HasSuffix(strings.ToLower(full), ".md") {
		jsonError(w, http.StatusForbidden, "only .md files are served")
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		jsonError(w, http.StatusNotFound, "read: "+err.Error())
		return
	}
	if len(b) > maxDocBytes {
		b = b[:maxDocBytes]
	}
	writeJSON(w, map[string]string{"name": filepath.Base(full), "content": string(b)})
}

// expandHome resolves a leading "~/" against the user's home dir.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
