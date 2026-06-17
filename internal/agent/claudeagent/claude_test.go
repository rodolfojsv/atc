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
	s := &session{id: "abc-123", spec: agent.SessionSpec{WorkingDir: "/home/u/my proj"}}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	p, err := s.transcriptPath()
	if err != nil {
		t.Fatal(err)
	}
	want := "/cfg/projects/-home-u-my-proj/abc-123.jsonl"
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

func TestInitCommandsParsing(t *testing.T) {
	const initLine = `{"type":"system","subtype":"init","slash_commands":["compact","init","deep-research"],"skills":["deep-research","code-review"]}`
	var line streamLine
	if err := json.Unmarshal([]byte(initLine), &line); err != nil {
		t.Fatal(err)
	}
	s := &session{}
	if line.Type == "system" && line.Subtype == "init" {
		s.setCommands(line.SlashCommands, line.Skills)
	}
	got := s.ListCommands(context.Background())
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	// Union of both arrays, de-duplicated (deep-research appears in both).
	want := []string{"compact", "init", "deep-research", "code-review"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("commands = %v, want %v", names, want)
	}
}

// TestLiveSmoke drives a real `claude` subprocess end to end. Opt-in
// (spends a small amount of usage): ATC_CLAUDE_SMOKE=1 go test ./internal/agent/claudeagent/
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("ATC_CLAUDE_SMOKE") != "1" {
		t.Skip("set ATC_CLAUDE_SMOKE=1 to run the live claude smoke test")
	}
	done := make(chan agent.Event, 1)
	var text strings.Builder
	spec := agent.SessionSpec{
		WorkingDir: t.TempDir(),
		Model:      "haiku",
		Approval:   config.ApprovalPrompt,
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
	case <-time.After(120 * time.Second):
		t.Fatal("timed out waiting for result")
	}
	if !strings.Contains(text.String(), "OK") {
		t.Errorf("expected OK in response, got %q", text.String())
	}
	t.Logf("response: %q  session: %s", text.String(), sess.ID())

	// Second turn exercises process reuse on the same conversation.
	text.Reset()
	if err := sess.Send(context.Background(), "Now reply with exactly the word SECOND and nothing else."); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if e.Type == agent.EventError {
			t.Fatalf("second turn error: %s %s", e.ErrType, e.Text)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("timed out on second turn")
	}
	if !strings.Contains(text.String(), "SECOND") {
		t.Errorf("expected SECOND, got %q", text.String())
	}

	// History should replay both prompts from the on-disk transcript.
	users := 0
	for _, e := range sess.History(context.Background()) {
		if e.Type == agent.EventUserMessage {
			users++
		}
	}
	if users < 2 {
		t.Errorf("expected ≥2 user messages in history, got %d", users)
	}
}

// userContent must produce the API content-block shape Claude Code's
// stream-JSON input expects: image blocks first, then the text block.
func TestUserContentWithImages(t *testing.T) {
	got := userContent("what is this?", []agent.Attachment{
		{Name: "a.png", MediaType: "image/png", Data: []byte{1, 2, 3}},
	})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("content is not a block array: %s", raw)
	}
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d: %s", len(blocks), raw)
	}
	if blocks[0]["type"] != "image" {
		t.Fatalf("first block %v, want image", blocks[0]["type"])
	}
	src := blocks[0]["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AQID" {
		t.Fatalf("bad image source: %v", src)
	}
	if blocks[1]["type"] != "text" || blocks[1]["text"] != "what is this?" {
		t.Fatalf("bad text block: %v", blocks[1])
	}
}

// Without attachments the content stays a plain string — the shape
// every Claude Code version accepts.
func TestUserContentPlain(t *testing.T) {
	if got := userContent("hi", nil); got != "hi" {
		t.Fatalf("got %v, want plain string", got)
	}
}

// AskUserQuestion must render as a readable question (headless Claude
// can't be answered, so the user replies in prose).
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
	// It must NOT have produced a generic tool-start line.
	for _, e := range events {
		if e.Type == agent.EventToolStart {
			t.Errorf("AskUserQuestion should not render as a tool line")
		}
	}
}

func TestHistoryRestoresUsage(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	workDir := "/home/u/proj"
	s := &session{id: "hist-1", spec: agent.SessionSpec{WorkingDir: workDir}}

	path, err := s.transcriptPath()
	if err != nil {
		t.Fatal(err)
	}
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
