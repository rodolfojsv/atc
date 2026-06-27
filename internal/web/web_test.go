package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	s := New(supervisor.New(cfg, bus.New()), cfg, "secret", "")
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

func TestDocsConfinement(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "vault")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.md"), []byte("# Hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret outside the configured root must never be reachable.
	if err := os.WriteFile(filepath.Join(dir, "secret.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Web.DocFolders = []config.DocFolder{{Label: "Vault", Path: root}}
	s := New(supervisor.New(cfg, bus.New()), cfg, "secret", "")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// Listing finds the nested .md by relative path.
	resp := get(t, ts.URL+"/api/docs/list?folder=0&token=secret", "")
	var list struct{ Files []string }
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Files) != 1 || list.Files[0] != filepath.Join("sub", "a.md") {
		t.Fatalf("list = %v", list.Files)
	}

	// In-root file renders.
	resp = get(t, ts.URL+"/api/docs/file?folder=0&path="+url.QueryEscape("sub/a.md")+"&token=secret", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in-root file status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Traversal out of the root is refused.
	resp = get(t, ts.URL+"/api/docs/file?folder=0&path="+url.QueryEscape("../secret.md")+"&token=secret", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("traversal status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown folder index is rejected.
	resp = get(t, ts.URL+"/api/docs/list?folder=9&token=secret", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad folder status %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestScheduleCRUD(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := config.Load(cfgPath) // missing file → defaults
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []string{"/tmp/repo-a"}
	s := New(supervisor.New(cfg, bus.New()), cfg, "secret", cfgPath)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	do := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	mustStatus := func(method, path, body string, want int) {
		t.Helper()
		resp := do(method, path, body)
		defer resp.Body.Close()
		if resp.StatusCode != want {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s: status %d, want %d (%s)", method, path, resp.StatusCode, want, b)
		}
	}

	// Create, then reject the predictable bad inputs.
	mustStatus("POST", "/api/schedules",
		`{"name":"nightly","cron":"0 3 * * *","repo":"/tmp/repo-a","prompt":"go","model":"claude-sonnet-4-6","write":true}`, 200)
	mustStatus("POST", "/api/schedules", `{"name":"bad","cron":"not a cron","repo":"/tmp/repo-a","prompt":"x"}`, 400)
	mustStatus("POST", "/api/schedules", `{"name":"r","cron":"0 3 * * *","repo":"/tmp/other","prompt":"x"}`, 400)
	mustStatus("POST", "/api/schedules", `{"name":"nightly","cron":"0 3 * * *","repo":"/tmp/repo-a","prompt":"x"}`, 409)

	if got := s.sup.ScheduleConfigs(); len(got) != 1 || got[0].Model != "claude-sonnet-4-6" || !got[0].Write {
		t.Fatalf("in-memory schedule wrong: %+v", got)
	}
	// The change must be on disk so the scheduler's next config reload sees it.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Schedules) != 1 || reloaded.Schedules[0].Name != "nightly" {
		t.Fatalf("not persisted: %+v", reloaded.Schedules)
	}

	// Disable, rename+edit, delete.
	mustStatus("POST", "/api/schedules/nightly/disable", `{"disabled":true}`, 200)
	if got := s.sup.ScheduleConfigs(); !got[0].Disabled {
		t.Error("toggle did not disable")
	}
	mustStatus("PUT", "/api/schedules/nightly", `{"name":"daily","cron":"0 9 * * *","repo":"/tmp/repo-a","prompt":"go2"}`, 200)
	if got := s.sup.ScheduleConfigs(); len(got) != 1 || got[0].Name != "daily" || got[0].Cron != "0 9 * * *" {
		t.Fatalf("update wrong: %+v", got)
	}
	mustStatus("DELETE", "/api/schedules/daily", "", 200)
	if got := s.sup.ScheduleConfigs(); len(got) != 0 {
		t.Fatalf("not deleted: %+v", got)
	}
}

func TestCompleteDirListsSkills(t *testing.T) {
	_, ts := testServer(t)
	repo := t.TempDir()
	skill := filepath.Join(repo, ".claude", "skills", "deploy-app")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\ndescription: ship it\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := get(t, ts.URL+"/api/complete?token=secret&backend=claude&dir="+url.QueryEscape(repo), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out completeJSON
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	var found *cmdInfo
	for i := range out.Commands {
		if out.Commands[i].Name == "/deploy-app" {
			found = &out.Commands[i]
		}
	}
	if found == nil {
		t.Fatalf("/deploy-app not in %+v", out.Commands)
	}
	if found.Desc != "ship it" {
		t.Errorf("desc = %q, want %q", found.Desc, "ship it")
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

func TestServiceWorkerServedWithoutAuth(t *testing.T) {
	_, ts := testServer(t)
	resp := get(t, ts.URL+"/sw.js", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sw.js: status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("sw.js content-type %q", resp.Header.Get("Content-Type"))
	}
	// The worker must be allowed to claim the whole origin, else
	// registration with scope "/" is rejected by the browser.
	if resp.Header.Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("Service-Worker-Allowed = %q, want /", resp.Header.Get("Service-Worker-Allowed"))
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

// TestCreateRepoConfinement verifies a token-holder can only start a session
// in a configured repo (or scratch) — an arbitrary host path is rejected, so
// a forged request can't point an agent outside the operator's repos.
func TestCreateRepoConfinement(t *testing.T) {
	_, ts := testServer(t) // cfg.Repos = {"/tmp/repo-a"}
	post := func(body string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/api/sessions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(`{"repo":"/etc"}`); got != http.StatusForbidden {
		t.Fatalf("arbitrary path: status %d, want 403", got)
	}
	if got := post(`{"repo":"/tmp/repo-a/../repo-b"}`); got != http.StatusForbidden {
		t.Fatalf("traversal outside configured repo: status %d, want 403", got)
	}
}

func TestClientID(t *testing.T) {
	mk := func(v string, set bool) *http.Request {
		r := httptest.NewRequest("POST", "/api/sessions", nil)
		if set {
			r.Header.Set("X-Atc-Client", v)
		}
		return r
	}
	if got := clientID(mk("  dev-abc  ", true)); got != "dev-abc" {
		t.Fatalf("trim: got %q, want dev-abc", got)
	}
	if got := clientID(mk("", false)); got != "" {
		t.Fatalf("absent header: got %q, want empty", got)
	}
	if got := clientID(mk(strings.Repeat("x", 200), true)); len(got) != 64 {
		t.Fatalf("cap: len %d, want 64", len(got))
	}
}

func TestAppLatest404WhenNoAPK(t *testing.T) {
	_, ts := testServer(t)
	resp := get(t, ts.URL+"/api/app/latest?token=secret", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no APK configured: status %d, want 404", resp.StatusCode)
	}
}

func TestAppLatestAndDownload(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	apk := filepath.Join(t.TempDir(), "atc.apk")
	if err := os.WriteFile(apk, []byte("PK\x03\x04 fake apk bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Web.APKPath = apk
	cfg.Web.APKVersion = "1.2.3"
	s := New(supervisor.New(cfg, bus.New()), cfg, "secret", "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp := get(t, ts.URL+"/api/app/latest?token=secret", "")
	defer resp.Body.Close()
	var info appInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "1.2.3" || info.Size == 0 || len(info.SHA256) != 64 {
		t.Fatalf("latest = %+v", info)
	}

	dl := get(t, ts.URL+"/api/app/download?token=secret", "")
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status %d", dl.StatusCode)
	}
	if ct := dl.Header.Get("Content-Type"); ct != "application/vnd.android.package-archive" {
		t.Fatalf("download content-type %q", ct)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "atc-1.2.3.apk") {
		t.Fatalf("download disposition %q", cd)
	}
}

func TestAppQR(t *testing.T) {
	_, ts := testServer(t)
	// Missing data -> 400.
	bad := get(t, ts.URL+"/api/app/qr?token=secret", "")
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("no data: status %d, want 400", bad.StatusCode)
	}
	// Valid data -> a PNG image.
	resp := get(t, ts.URL+"/api/app/qr?token=secret&data=https%3A%2F%2Fexample.ts.net%2F%3Ftoken%3Dx", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr: status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 8 || string(body[1:4]) != "PNG" {
		t.Fatalf("qr body not a PNG (len %d)", len(body))
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
