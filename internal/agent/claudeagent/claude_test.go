package claudeagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
)

// The badge must headline the binding "Current …" window and ignore the
// "What's contributing…" stats below, whose percentages ("80% of your usage
// came from…") are not limits — reading the max over the whole overlay is what
// produced a bogus 74%.
func TestParseUsageLimits(t *testing.T) {
	overlay := strings.Join([]string{
		"You are currently using your subscription to power your Claude Code usage",
		"",
		"Current session: 0% used · resets Jun 17, 7:49pm (America/Chicago)",
		"Current week (all models): 36% used · resets Jun 20, 2:59pm (America/Chicago)",
		"Current week (Sonnet only): 0% used · resets Jun 20, 3pm (America/Chicago)",
		"",
		"What's contributing to your limits usage?",
		"Last 7d · 9952 requests · 125 sessions",
		"  80% of your usage came from subagent-heavy sessions",
		"  67% of your usage was at >150k context",
	}, "\n")
	windows := parseUsageLimits(overlay)
	if len(windows) != 3 {
		t.Fatalf("want 3 windows (session, all models, Sonnet), got %d: %+v", len(windows), windows)
	}
	want := []struct {
		label string
		pct   float64
	}{{"session", 0}, {"week (all models)", 36}, {"week (Sonnet only)", 0}}
	for i, w := range want {
		if windows[i].Label != w.label || windows[i].Pct != w.pct {
			t.Errorf("window %d = %q %v%%, want %q %v%%", i, windows[i].Label, windows[i].Pct, w.label, w.pct)
		}
	}
	if !strings.Contains(windows[1].Resets, "Jun 20") {
		t.Errorf("all-models resets = %q, want a Jun 20 hint", windows[1].Resets)
	}
}

func TestMessageEventsStringContent(t *testing.T) {
	events := messageEvents(json.RawMessage(`"plain answer"`))
	if len(events) != 1 || events[0].Type != agent.EventMessage || events[0].Text != "plain answer" {
		t.Fatalf("unexpected: %+v", events)
	}
}

func TestMessageEventsBlocks(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"I'll check the file."},
		{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}},
		{"type":"text","text":"Done."}
	]`)
	events := messageEvents(raw)
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %+v", events)
	}
	if events[1].Type != agent.EventToolStart || !strings.Contains(events[1].Text, "go test ./...") {
		t.Errorf("tool event malformed: %+v", events[1])
	}
}

func TestTranscriptPathEncoding(t *testing.T) {
	s := &session{id: "abc-123", claudeID: "abc-123", spec: agent.SessionSpec{WorkingDir: "/home/u/my proj"}}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	want := "/cfg/projects/-home-u-my-proj/abc-123.jsonl"
	if p := s.transcriptPath(); p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

// eventsFromLine drives both History (includeUser=true) and live tailing
// (includeUser=false). The live path must skip the user's own prompt so it
// isn't echoed back into the transcript the user just typed into.
func TestEventsFromLineUserVisibility(t *testing.T) {
	userLine := []byte(`{"type":"user","message":{"role":"user","content":"hello there"}}`)

	if evs := eventsFromLine(userLine, false); len(evs) != 0 {
		t.Errorf("live tail should skip user lines, got %+v", evs)
	}
	evs := eventsFromLine(userLine, true)
	if len(evs) != 1 || evs[0].Type != agent.EventUserMessage || evs[0].Text != "hello there" {
		t.Errorf("history replay should include the user message, got %+v", evs)
	}
}

// AskUserQuestion must render as a readable question rather than a tool line.
func TestFormatAskUserQuestion(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_use","name":"AskUserQuestion","input":{
		"questions":[{"header":"Indentation","question":"Tabs or spaces?","options":[
			{"label":"Tabs","description":"tab chars"},
			{"label":"Spaces","description":"space chars"}]}]}}]`)
	events := messageEvents(raw)
	if len(events) != 1 || events[0].Type != agent.EventMessage {
		t.Fatalf("expected one message event, got %+v", events)
	}
	text := events[0].Text
	for _, want := range []string{"Indentation", "Tabs or spaces?", "Tabs", "Spaces", "Reply with your choice"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered question missing %q:\n%s", want, text)
		}
	}
	for _, e := range events {
		if e.Type == agent.EventToolStart {
			t.Errorf("AskUserQuestion should not render as a tool line")
		}
	}
}

func TestHistoryRestoresUsage(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	s := &session{id: "hist-1", claudeID: "hist-1", spec: agent.SessionSpec{WorkingDir: "/home/u/proj"}}

	path := s.transcriptPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","costUSD":0.012,"message":{"role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":120,"output_tokens":40}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"more"}],"usage":{"input_tokens":200,"output_tokens":60}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var in, out int64
	var cost float64
	users, msgs := 0, 0
	for _, e := range s.History(context.Background()) {
		switch e.Type {
		case agent.EventUserMessage:
			users++
		case agent.EventMessage:
			msgs++
		case agent.EventUsage:
			in += e.InputTokens
			out += e.OutputTokens
			cost += e.CostUSD
		}
	}
	if users != 1 || msgs != 2 {
		t.Errorf("chat replay wrong: %d users, %d msgs", users, msgs)
	}
	if in != 320 || out != 100 {
		t.Errorf("token totals not restored: %d in, %d out", in, out)
	}
	if cost != 0.012 {
		t.Errorf("cost not restored: %v", cost)
	}
}

// drainTranscript must emit only lines written after the recorded offset, so a
// new turn doesn't re-emit the whole prior conversation.
func TestDrainTranscriptFromOffset(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	s := &session{id: "drain-1", claudeID: "drain-1", spec: agent.SessionSpec{WorkingDir: "/w"}}

	path := s.transcriptPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old turn"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start this turn at end-of-file, as Send does.
	s.offset = transcriptSize(path)

	// Nothing new yet.
	if evs := s.drainTranscript(); len(evs) != 0 {
		t.Fatalf("expected no events before new output, got %+v", evs)
	}

	// Append a new assistant line; only it should be emitted.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"new turn"}]}}` + "\n")
	_ = f.Close()

	evs := s.drainTranscript()
	if len(evs) != 1 || evs[0].Text != "new turn" {
		t.Fatalf("expected only the new line, got %+v", evs)
	}
	// A second drain with no new bytes yields nothing (offset advanced).
	if evs := s.drainTranscript(); len(evs) != 0 {
		t.Fatalf("offset not advanced; re-emitted %+v", evs)
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"claude", "--model", "haiku", "--resume", "a'b"})
	want := `'claude' '--model' 'haiku' '--resume' 'a'\''b'`
	if got != want {
		t.Errorf("shellJoin:\n got %s\nwant %s", got, want)
	}
}

func TestIsShellAndWorkingMarkers(t *testing.T) {
	for _, sh := range []string{"sh", "bash", "zsh", "fish"} {
		if !isShell(sh) {
			t.Errorf("%q should be a shell", sh)
		}
	}
	for _, notSh := range []string{"claude", "node", "vim"} {
		if isShell(notSh) {
			t.Errorf("%q should not be a shell", notSh)
		}
	}
	if !containsAny("…thinking (esc to interrupt)", workingMarkers) {
		t.Error("expected working marker to match the busy status line")
	}
	if containsAny("> ", workingMarkers) {
		t.Error("idle prompt should not match a working marker")
	}
	// Live-observed busy line (the spinner word rotates; the "(<n>s" counter
	// is the stable signal).
	if !isWorking("✢ Noodling… (49s · ↓ 2.7k tokens)") {
		t.Error("isWorking should match the live busy spinner line")
	}
	if isWorking("❯ type a message\n  ? for shortcuts") {
		t.Error("isWorking should be false on an idle input box")
	}
}

// The first-run trust dialog must be recognized (so waitReady can auto-accept
// it) and must not be confused with the ready splash screen.
func TestIsTrustPrompt(t *testing.T) {
	trust := strings.Join([]string{
		" Accessing workspace:",
		" /tmp/ccexp.t4Lp5i",
		" Quick safety check: Is this a project you created or one you trust?",
		" ❯ 1. Yes, I trust this folder",
		"   2. No, exit",
		" Enter to confirm · Esc to cancel",
	}, "\n")
	if !isTrustPrompt(trust) {
		t.Error("expected the trust dialog to be recognized")
	}
	splash := "Welcome back Mauricio!\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	if isTrustPrompt(splash) {
		t.Error("the ready splash should not be detected as a trust dialog")
	}
	if !containsAny(splash, readyMarkers) {
		t.Error("the splash should match a ready marker")
	}
}

// TestLiveSmoke drives a real `claude` through tmux end to end. Opt-in (needs
// tmux + a logged-in claude, and spends a little subscription usage):
// ATC_CLAUDE_SMOKE=1 go test ./internal/agent/claudeagent/
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("ATC_CLAUDE_SMOKE") != "1" {
		t.Skip("set ATC_CLAUDE_SMOKE=1 to run the live claude+tmux smoke test")
	}
	done := make(chan agent.Event, 1)
	var text strings.Builder
	spec := agent.SessionSpec{
		WorkingDir: t.TempDir(),
		Model:      "haiku",
		Approval:   config.ApprovalAllowAll,
		OnEvent: func(e agent.Event) {
			switch e.Type {
			case agent.EventMessage:
				text.WriteString(e.Text)
			case agent.EventIdle, agent.EventError:
				select {
				case done <- e:
				default:
				}
			}
		},
	}
	sess, err := New().NewSession(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.Send(context.Background(), "Reply with exactly the word OK and nothing else."); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if e.Type == agent.EventError {
			t.Fatalf("session error: %s %s", e.ErrType, e.Text)
		}
	case <-time.After(180 * time.Second):
		t.Fatal("timed out waiting for turn-end")
	}
	if !strings.Contains(text.String(), "OK") {
		t.Errorf("expected OK in response, got %q", text.String())
	}
	t.Logf("response: %q  session: %s", text.String(), sess.ID())
}
