package copilotagent

import (
	"os"
	"strings"
	"sync"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/rodolfojsv/atc/internal/agent"
)

// TestCopilotTrace verifies the opt-in ATC_COPILOT_TRACE log captures the
// raw SDK event stream — type, exact content (whitespace quoted), and the
// agent.Event each translated into — so a transcript-duplication repro can
// be diagnosed from the file.
func TestCopilotTrace(t *testing.T) {
	// Reset the once-initialised tracer so this test controls the file.
	traceOnce = sync.Once{}
	traceFile = nil
	path := t.TempDir() + "/trace.log"
	t.Setenv("ATC_COPILOT_TRACE", path)

	var got []agent.Event
	handle := eventTranslator("sess-1234-abcd", func(e agent.Event) { got = append(got, e) })

	// A streamed delta (with a newline, to prove whitespace is visible),
	// the authoritative message, and a tool start.
	handle(copilot.SessionEvent{Data: &rpc.AssistantMessageDeltaData{DeltaContent: "Let me check.\n"}})
	handle(copilot.SessionEvent{Data: &rpc.AssistantMessageData{Content: "Let me check.\nDone."}})
	handle(copilot.SessionEvent{Data: &rpc.ToolExecutionStartData{ToolName: "Read"}})

	if len(got) != 3 {
		t.Fatalf("expected 3 translated events, got %d", len(got))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"sess-123",                  // truncated session id
		"assistant.message_delta",   // raw type
		"→ text_delta",              // translation
		`len=14 "Let me check.\n"`,  // length + whitespace-visible content
		"assistant.message",         // raw type
		"→ message",                 // translation
		`"Let me check.\nDone."`,    // final content, newline visible
		"tool.execution_start",      // raw type (best-effort: see below)
		"Read",                      // tool name
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace missing %q\n--- trace ---\n%s", want, out)
		}
	}
}

// limitsFromQuota powers Copilot's account-usage badge: quota reports the
// percentage remaining, the badge shows percentage used, unlimited/empty
// entitlements are dropped, and windows come out label-sorted (Go map order
// is random) so the snapshot doesn't reshuffle on every poll.
func TestLimitsFromQuota(t *testing.T) {
	if _, ok := limitsFromQuota(nil); ok {
		t.Fatal("no snapshots should yield no limits event")
	}

	snaps := map[string]rpc.AssistantUsageQuotaSnapshot{
		"premium_interactions": {RemainingPercentage: 25},
		"chat":                 {RemainingPercentage: 90},
		"completions":          {IsUnlimitedEntitlement: true, RemainingPercentage: 0},
	}
	e, ok := limitsFromQuota(snaps)
	if !ok {
		t.Fatal("want a limits event")
	}
	if e.Type != agent.EventLimits {
		t.Fatalf("want EventLimits, got %v", e.Type)
	}
	// Unlimited entitlement dropped; remaining two sorted by label.
	want := []agent.LimitWindow{
		{Label: "AIC", Pct: 75},
		{Label: "chat", Pct: 10},
	}
	if len(e.LimitWindows) != len(want) {
		t.Fatalf("want %d windows, got %d: %+v", len(want), len(e.LimitWindows), e.LimitWindows)
	}
	for i, w := range want {
		if e.LimitWindows[i].Label != w.Label || e.LimitWindows[i].Pct != w.Pct {
			t.Errorf("window %d = %+v, want %+v", i, e.LimitWindows[i], w)
		}
	}
}
