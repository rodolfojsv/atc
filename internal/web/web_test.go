package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []string{"/tmp/repo-a"}
	s := New(supervisor.New(cfg, bus.New()), cfg, "secret")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func get(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	_, ts := testServer(t)
	for _, token := range []string{"", "wrong"} {
		resp := get(t, ts.URL+"/api/sessions", token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
	// Query-parameter form works too (the printed URL).
	resp := get(t, ts.URL+"/api/sessions?token=secret", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query token: status %d, want 200", resp.StatusCode)
	}
}

func TestIndexServedWithoutAuth(t *testing.T) {
	_, ts := testServer(t)
	resp := get(t, ts.URL+"/", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestMetaAndEmptySessions(t *testing.T) {
	_, ts := testServer(t)
	resp := get(t, ts.URL+"/api/meta", "secret")
	defer resp.Body.Close()
	var meta struct {
		Repos    []string `json:"repos"`
		Backends []string `json:"backends"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Repos) != 1 || len(meta.Backends) == 0 {
		t.Fatalf("meta = %+v", meta)
	}

	resp2 := get(t, ts.URL+"/api/sessions", "secret")
	defer resp2.Body.Close()
	var list []sessionJSON
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty board, got %d", len(list))
	}
}

func TestCreateValidation(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/api/sessions", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing repo: status %d, want 400", resp.StatusCode)
	}
}

func TestUnknownSession404(t *testing.T) {
	_, ts := testServer(t)
	resp := get(t, ts.URL+"/api/sessions/nope", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

// readPrompt must pull text and typed attachments out of a multipart
// form, sniffing the media type when the part doesn't declare one.
func TestReadPromptMultipart(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", "look at this")
	fw, _ := mw.CreateFormFile("files", "shot.png")
	// Real PNG magic so DetectContentType identifies it.
	_, _ = fw.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0})
	mw.Close()

	req := httptest.NewRequest("POST", "/api/sessions/x/prompt", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	text, atts, err := readPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	if text != "look at this" || len(atts) != 1 {
		t.Fatalf("text=%q atts=%d", text, len(atts))
	}
	if atts[0].MediaType != "image/png" || !atts[0].IsImage() {
		t.Fatalf("media type %q, want image/png", atts[0].MediaType)
	}
}
