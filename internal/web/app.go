package web

// "App" tab support: serve a built Android APK and render QR codes for
// pairing a device / downloading the APK. Mirrors the approach in the
// sibling linuxservermanager project, adapted to atc's single-binary,
// no-npm posture — the QR is generated server-side with the tiny
// dependency-free rsc.io/qr rather than a client-side JS library.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"rsc.io/qr"
)

// appInfo is the JSON returned by GET /api/app/latest.
type appInfo struct {
	Version    string `json:"version"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	ReleasedAt string `json:"releasedAt"`
}

// apkHashCache caches the APK's SHA-256 keyed on size+mtime so a multi-MB
// file isn't rehashed on every poll. The first call after a replacement
// pays the hashing cost; the rest are O(1).
type apkHashCache struct {
	mu      sync.Mutex
	size    int64
	modTime time.Time
	hash    string
}

func (c *apkHashCache) get(path string, size int64, modTime time.Time) (string, error) {
	c.mu.Lock()
	if c.hash != "" && c.size == size && c.modTime.Equal(modTime) {
		h := c.hash
		c.mu.Unlock()
		return h, nil
	}
	c.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	c.mu.Lock()
	c.hash, c.size, c.modTime = sum, size, modTime
	c.mu.Unlock()
	return sum, nil
}

// statAPK returns the configured APK's file info, or ok=false when it's
// unset or missing on disk — the routes treat that as a 404 so the tab can
// show "no build yet".
func (s *Server) statAPK() (os.FileInfo, string, bool) {
	path := s.cfg.Web.APKPath
	if path == "" {
		return nil, "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, "", false
	}
	return info, path, true
}

func (s *Server) handleAppLatest(w http.ResponseWriter, _ *http.Request) {
	info, path, ok := s.statAPK()
	if !ok {
		jsonError(w, http.StatusNotFound, "no APK published")
		return
	}
	sum, err := s.apkCache.get(path, info.Size(), info.ModTime())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to hash APK")
		return
	}
	version := s.cfg.Web.APKVersion
	if version == "" {
		version = "unknown"
	}
	writeJSON(w, appInfo{
		Version:    version,
		Size:       info.Size(),
		SHA256:     sum,
		ReleasedAt: info.ModTime().UTC().Format(time.RFC3339),
	})
}

// handleAppDownload streams the APK with the android package-archive
// content-type. http.ServeFile gives Range support for free.
func (s *Server) handleAppDownload(w http.ResponseWriter, r *http.Request) {
	_, path, ok := s.statAPK()
	if !ok {
		jsonError(w, http.StatusNotFound, "no APK published")
		return
	}
	version := s.cfg.Web.APKVersion
	if version == "" {
		version = "latest"
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="atc-`+version+`.apk"`)
	http.ServeFile(w, r, path)
}

// handleAppQR renders the `data` query value as a QR PNG. It's loaded as an
// <img>, so auth arrives via ?token= (the middleware accepts that) rather
// than a header. The route is a pure function of its input — it encodes
// whatever it's handed and exposes nothing the caller didn't already supply.
func (s *Server) handleAppQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" {
		jsonError(w, http.StatusBadRequest, "data is required")
		return
	}
	if len(data) > 2048 {
		jsonError(w, http.StatusBadRequest, "data too long")
		return
	}
	code, err := qr.Encode(data, qr.M)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "cannot encode: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(code.PNG())
}
