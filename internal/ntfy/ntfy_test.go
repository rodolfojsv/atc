package ntfy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
)

// capture spins up a fake ntfy server and returns the parsed message bodies
// it receives. waitFor blocks until n messages arrive (OnEvent posts async).
type capture struct {
	mu   sync.Mutex
	msgs []ntfyMessage
	got  chan struct{}
}

func newCapture() (*httptest.Server, *capture) {
	c := &capture{got: make(chan struct{}, 16)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m ntfyMessage
		_ = json.Unmarshal(body, &m)
		c.mu.Lock()
		c.msgs = append(c.msgs, m)
		c.mu.Unlock()
		c.got <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	return ts, c
}

func (c *capture) waitFor(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c.got:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for ntfy message %d/%d", i+1, n)
		}
	}
}

func TestRoutesToSessionTopic(t *testing.T) {
	ts, c := newCapture()
	defer ts.Close()
	pub := New(config.Ntfy{Server: ts.URL, ServerName: "myhost"},
		"atctoken", func(name string) string {
			if name == "mine" {
				return "atc-device1"
			}
			return ""
		})

	pub.OnEvent(bus.Event{Type: bus.WaitingOnPermission, SessionName: "mine", Data: map[string]any{"summary": "run go test"}})
	c.waitFor(t, 1)

	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.msgs[0]
	if m.Topic != "atc-device1" {
		t.Fatalf("topic = %q, want atc-device1", m.Topic)
	}
	if m.Priority != 4 || len(m.Tags) == 0 {
		t.Fatalf("waiting priority/tags = %d/%v", m.Priority, m.Tags)
	}
	if m.Message != "run go test" {
		t.Fatalf("message = %q", m.Message)
	}
	if want := "myhost · mine needs approval"; m.Title != want {
		t.Fatalf("title = %q, want %q", m.Title, want)
	}
}

func TestFallbackTopicAndSkip(t *testing.T) {
	ts, c := newCapture()
	defer ts.Close()
	// No per-session topic; a default topic is configured -> falls back.
	pub := New(config.Ntfy{Server: ts.URL, Topic: "atc-default"}, "", func(string) string { return "" })
	pub.OnEvent(bus.Event{Type: bus.Finished, SessionName: "x", Data: map[string]any{"lastLine": "done"}})
	c.waitFor(t, 1)
	c.mu.Lock()
	if c.msgs[0].Topic != "atc-default" {
		t.Fatalf("fallback topic = %q", c.msgs[0].Topic)
	}
	c.mu.Unlock()

	// A non-notify event must not POST anything.
	pub.OnEvent(bus.Event{Type: bus.ToolCall, SessionName: "x"})
	select {
	case <-c.got:
		t.Fatal("tool-call should not produce an ntfy message")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNoTopicNoSend(t *testing.T) {
	ts, c := newCapture()
	defer ts.Close()
	// No per-session topic and no default -> nobody to notify, no POST.
	pub := New(config.Ntfy{Server: ts.URL}, "", func(string) string { return "" })
	pub.OnEvent(bus.Event{Type: bus.Finished, SessionName: "x"})
	select {
	case <-c.got:
		t.Fatal("should not POST when no topic resolves")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestDefaultPlusPerDevice(t *testing.T) {
	ts, c := newCapture()
	defer ts.Close()
	// A session WITH a per-device topic, AND a default configured: both get it,
	// so a phone subscribed to either receives the alert (no duplicate per topic).
	pub := New(config.Ntfy{Server: ts.URL, Topic: "atc-default"}, "", func(string) string { return "atc-dev1" })
	pub.OnEvent(bus.Event{Type: bus.Finished, SessionName: "x", Data: map[string]any{"lastLine": "ok"}})
	c.waitFor(t, 2)
	c.mu.Lock()
	defer c.mu.Unlock()
	got := map[string]bool{c.msgs[0].Topic: true, c.msgs[1].Topic: true}
	if !got["atc-default"] || !got["atc-dev1"] {
		t.Fatalf("topics = %v, want both atc-default and atc-dev1", got)
	}
}

func TestMuteSentinelSilencesEverything(t *testing.T) {
	ts, c := newCapture()
	defer ts.Close()
	// Even with a default topic, the mute sentinel suppresses all push.
	pub := New(config.Ntfy{Server: ts.URL, Topic: "atc-default"}, "", func(string) string { return MuteSentinel })
	pub.OnEvent(bus.Event{Type: bus.Finished, SessionName: "x"})
	select {
	case <-c.got:
		t.Fatal("muted session must not POST anything, even to the default topic")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestActionButtonsOptIn(t *testing.T) {
	// Actions on: a waiting event gets Approve/Deny buttons hitting /respond.
	on := New(config.Ntfy{Actions: true, PublicURL: "https://host.ts.net", Topic: "t"}, "secret", func(string) string { return "" })
	m := on.build("t", "needs approval", "", 4, "warning",
		bus.Event{Type: bus.WaitingOnPermission, SessionName: "api-refactor"})
	if len(m.Actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(m.Actions))
	}
	if m.Actions[0].URL != "https://host.ts.net/api/sessions/api-refactor/respond" {
		t.Fatalf("approve url = %q", m.Actions[0].URL)
	}
	if m.Actions[0].Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("missing bearer header: %v", m.Actions[0].Headers)
	}
	if m.Click != "https://host.ts.net/?focus=api-refactor" {
		t.Fatalf("click = %q", m.Click)
	}

	// Actions off (default): no buttons, still a tokenless click link.
	off := New(config.Ntfy{PublicURL: "https://host.ts.net", Topic: "t"}, "secret", func(string) string { return "" })
	m2 := off.build("t", "needs approval", "", 4, "warning",
		bus.Event{Type: bus.WaitingOnPermission, SessionName: "x"})
	if len(m2.Actions) != 0 {
		t.Fatalf("actions should be empty when disabled, got %d", len(m2.Actions))
	}
	if m2.Click == "" {
		t.Fatal("click link should still be present")
	}
}
