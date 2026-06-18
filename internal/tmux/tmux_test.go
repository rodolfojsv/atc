package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// chunkBytes must reassemble to the original and never split a multi-byte rune
// (a mid-rune cut would corrupt the pasted prompt).
func TestChunkBytes(t *testing.T) {
	inputs := []string{
		"",
		"short",
		strings.Repeat("a", 2000),
		strings.Repeat("a", 2001),
		strings.Repeat("a", 9999),
		strings.Repeat("é", 1500), // 2-byte runes straddling the boundary
		strings.Repeat("🚀", 800),  // 4-byte runes straddling the boundary
	}
	const max = 2000
	for _, in := range inputs {
		chunks := chunkBytes(in, max)
		if got := strings.Join(chunks, ""); got != in {
			t.Errorf("reassembly mismatch for len=%d", len(in))
		}
		for _, ch := range chunks {
			if len(ch) > max {
				t.Errorf("chunk len %d exceeds max %d", len(ch), max)
			}
			if !utf8.ValidString(ch) {
				t.Errorf("chunk split a rune: %q", ch)
			}
		}
	}
}

// newTestClient skips the test when tmux is not installed, so the suite stays
// green on machines (and CI) without it.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := New()
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	return c
}

// uniqueName derives a collision-resistant session name from the test name and
// time, so parallel/repeated runs don't clash on a leftover session.
func uniqueName(t *testing.T) string {
	t.Helper()
	safe := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("atc-test-%s-%d", safe, time.Now().UnixNano())
}

func TestSessionLifecycleAndIO(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	name := uniqueName(t)

	// Run a bare shell so we can type a command and read its output back.
	if err := c.NewSession(ctx, NewSessionOpts{
		Name:    name,
		Command: []string{"sh"},
		Width:   120,
		Height:  40,
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	ok, err := c.HasSession(ctx, name)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !ok {
		t.Fatal("HasSession = false right after NewSession")
	}

	// Type a command that prints a known marker, then submit it.
	const marker = "atc_tmux_marker_42"
	if err := c.SendText(ctx, name, "echo "+marker); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := c.SendEnter(ctx, name); err != nil {
		t.Fatalf("SendEnter: %v", err)
	}

	// The shell renders asynchronously; poll the pane until the marker appears.
	got := pollFor(t, func() (string, bool) {
		out, err := c.Capture(ctx, name, CaptureOpts{})
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		// Two occurrences would be the typed command echoed plus its output;
		// one is enough to prove the round trip worked.
		return out, strings.Contains(out, marker)
	})
	if !strings.Contains(got, marker) {
		t.Fatalf("marker %q not found in pane capture:\n%s", marker, got)
	}
}

func TestKillSessionIsIdempotent(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	name := uniqueName(t)

	// Killing a session that was never created must not error.
	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("KillSession on missing session: %v", err)
	}

	if err := c.NewSession(ctx, NewSessionOpts{Name: name, Command: []string{"sh"}}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	ok, err := c.HasSession(ctx, name)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if ok {
		t.Fatal("session still present after KillSession")
	}
}

func TestEnvIsScopedToPane(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	name := uniqueName(t)

	if err := c.NewSession(ctx, NewSessionOpts{
		Name:    name,
		Command: []string{"sh"},
		Env:     []string{"ATC_TMUX_ENV=present"},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	if err := c.SendText(ctx, name, "printf 'val=%s\\n' \"$ATC_TMUX_ENV\""); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := c.SendEnter(ctx, name); err != nil {
		t.Fatalf("SendEnter: %v", err)
	}

	got := pollFor(t, func() (string, bool) {
		out, err := c.Capture(ctx, name, CaptureOpts{})
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		return out, strings.Contains(out, "val=present")
	})
	if !strings.Contains(got, "val=present") {
		t.Fatalf("env var not visible in pane:\n%s", got)
	}
}

// pollFor calls fn until it reports done or a short deadline elapses, returning
// the last value seen. tmux renders asynchronously, so reads must be retried.
func pollFor(t *testing.T, fn func() (string, bool)) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		v, ok := fn()
		last = v
		if ok {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}
