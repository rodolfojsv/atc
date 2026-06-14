package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/bus"
)

// An agent question blocks until the user's next message answers it; a
// bare option number maps to that option's text.
func TestQuestionAnsweredByNextPrompt(t *testing.T) {
	s := New(testConfig(t), bus.New())
	ag := &fakeAgent{}
	sess := &Session{Name: "x", Dir: t.TempDir(), Preset: "default", status: StatusWorking, ag: ag}
	qf := s.questionFunc(sess)

	answer := make(chan string, 1)
	go func() {
		ans, ok := qf(agent.Question{Prompt: "Tabs or spaces?", Options: []string{"Tabs", "Spaces"}})
		if !ok {
			t.Errorf("question reported cancelled")
		}
		answer <- ans
	}()

	// Wait until the question is pending.
	deadline := time.After(2 * time.Second)
	for !sess.HasQuestion() {
		select {
		case <-deadline:
			t.Fatal("question never became pending")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Answering "2" should resolve to the second option's text.
	if err := s.Prompt(sess, "2"); err != nil {
		t.Fatal(err)
	}
	select {
	case ans := <-answer:
		if ans != "Spaces" {
			t.Fatalf("answer = %q, want Spaces", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("question handler still blocked after answer")
	}
	if sess.HasQuestion() {
		t.Fatal("question still pending after answer")
	}
	// The answer must NOT have been sent as a normal turn to the backend.
	if ag.sentText != "" {
		t.Fatalf("answer leaked to backend as a prompt: %q", ag.sentText)
	}
}

// Aborting a session unblocks a waiting question with ok=false.
func TestQuestionCancelledByAbort(t *testing.T) {
	s := New(testConfig(t), bus.New())
	ag := &fakeAgent{}
	sess := &Session{Name: "x", Dir: t.TempDir(), Preset: "default", status: StatusWorking, ag: ag}
	qf := s.questionFunc(sess)

	done := make(chan bool, 1)
	go func() {
		_, ok := qf(agent.Question{Prompt: "Proceed?"})
		done <- ok
	}()
	deadline := time.After(2 * time.Second)
	for !sess.HasQuestion() {
		select {
		case <-deadline:
			t.Fatal("question never pending")
		case <-time.After(5 * time.Millisecond):
		}
	}
	s.Abort(sess)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("expected cancelled (ok=false) on abort")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not unblock the question")
	}
}

// A session started in auto-approve must spawn the backend in allow-all
// so Claude (whose permission mode is fixed at launch) gets
// bypassPermissions, not just the Copilot runtime path.
func TestSpecAutoApproveBecomesAllowAll(t *testing.T) {
	s := New(testConfig(t), bus.New())
	sess := &Session{Name: "x", Dir: "/tmp", Preset: "default", autoApprove: true}
	if got := s.spec(sess, "").Approval; got != "allow-all" {
		t.Fatalf("auto session approval = %q, want allow-all", got)
	}
	off := &Session{Name: "y", Dir: "/tmp", Preset: "default"}
	if got := s.spec(off, "").Approval; got == "allow-all" {
		t.Fatalf("non-auto session should not be allow-all, got %q", got)
	}
}

// fakeAgent records what was sent. With inline=true it also implements
// agent.AttachmentSender, like the claude adapter.
type fakeAgent struct {
	sentText string
	sentAtts []agent.Attachment
}

func (f *fakeAgent) ID() string                                  { return "fake" }
func (f *fakeAgent) Send(_ context.Context, prompt string) error { f.sentText = prompt; return nil }
func (f *fakeAgent) SetModel(context.Context, string) error      { return nil }
func (f *fakeAgent) History(context.Context) []agent.Event       { return nil }
func (f *fakeAgent) Abort(context.Context) error                 { return nil }
func (f *fakeAgent) Close() error                                { return nil }

type fakeInlineAgent struct{ fakeAgent }

func (f *fakeInlineAgent) SendWithAttachments(_ context.Context, prompt string, atts []agent.Attachment) error {
	f.sentText, f.sentAtts = prompt, atts
	return nil
}

func png() agent.Attachment {
	return agent.Attachment{Name: "shot.png", MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}
}

// A backend without attachment support gets images saved to disk and
// referenced by path in the prompt text.
func TestPromptWithSavesToDiskForPlainBackend(t *testing.T) {
	s := New(testConfig(t), bus.New())
	dir := t.TempDir()
	ag := &fakeAgent{}
	sess := &Session{Name: "x", Dir: dir, Preset: "default", status: StatusIdle, ag: ag}

	if err := s.PromptWith(sess, "what is this?", []agent.Attachment{png()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ag.sentText, "what is this?") || !strings.Contains(ag.sentText, ".atc-attachments") {
		t.Fatalf("prompt missing text or file reference: %q", ag.sentText)
	}
	files, err := os.ReadDir(filepath.Join(dir, ".atc-attachments"))
	if err != nil || len(files) != 1 {
		t.Fatalf("attachment not written: %v (%d files)", err, len(files))
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".atc-attachments", files[0].Name())); string(data) != "\x89PNG" {
		t.Fatalf("attachment content mangled: %q", data)
	}
}

// A backend with AttachmentSender gets images inline and the prompt text
// stays clean (no on-disk file reference), but a copy is still persisted
// so the UI can show it.
func TestPromptWithInlinesImagesWhenSupported(t *testing.T) {
	s := New(testConfig(t), bus.New())
	dir := t.TempDir()
	ag := &fakeInlineAgent{}
	sess := &Session{Name: "x", Dir: dir, Preset: "default", status: StatusIdle, ag: ag}

	if err := s.PromptWith(sess, "what is this?", []agent.Attachment{png()}); err != nil {
		t.Fatal(err)
	}
	if len(ag.sentAtts) != 1 || ag.sentAtts[0].Name != "shot.png" {
		t.Fatalf("image not sent inline: %+v", ag.sentAtts)
	}
	if ag.sentText != "what is this?" {
		t.Fatalf("prompt text altered (should not reference disk path): %q", ag.sentText)
	}
	// The inline image is also saved for the UI and recorded on the entry.
	files, err := os.ReadDir(filepath.Join(dir, ".atc-attachments"))
	if err != nil || len(files) != 1 {
		t.Fatalf("inline image not persisted for viewing: %v (%d files)", err, len(files))
	}
	entries := sess.Transcript()
	last := entries[len(entries)-1]
	if len(last.Attachments) != 1 || last.Attachments[0].Name != "shot.png" ||
		!strings.HasPrefix(last.Attachments[0].Path, ".atc-attachments") {
		t.Fatalf("entry attachment metadata missing: %+v", last.Attachments)
	}
}

// Killing a session removes its saved attachments from disk.
func TestKillRemovesAttachments(t *testing.T) {
	s := New(testConfig(t), bus.New())
	dir := t.TempDir()
	ag := &fakeInlineAgent{}
	sess := &Session{Name: "x", Dir: dir, Preset: "default", status: StatusIdle, ag: ag}
	s.sessions = append(s.sessions, sess)

	if err := s.PromptWith(sess, "look", []agent.Attachment{png()}); err != nil {
		t.Fatal(err)
	}
	attDir := filepath.Join(dir, ".atc-attachments")
	if _, err := os.Stat(attDir); err != nil {
		t.Fatalf("attachments not written: %v", err)
	}
	s.Kill(sess, false)
	if _, err := os.Stat(attDir); !os.IsNotExist(err) {
		t.Fatalf("attachments dir survived kill: %v", err)
	}
}

// Non-image files go to disk even on an inline-capable backend (image
// blocks only accept images); the transcript shows the attachment names.
func TestPromptWithMixedAttachments(t *testing.T) {
	s := New(testConfig(t), bus.New())
	dir := t.TempDir()
	ag := &fakeInlineAgent{}
	sess := &Session{Name: "x", Dir: dir, Preset: "default", status: StatusIdle, ag: ag}

	atts := []agent.Attachment{png(), {Name: "log.txt", MediaType: "text/plain", Data: []byte("boom")}}
	if err := s.PromptWith(sess, "debug this", atts); err != nil {
		t.Fatal(err)
	}
	if len(ag.sentAtts) != 1 {
		t.Fatalf("expected 1 inline image, got %d", len(ag.sentAtts))
	}
	if !strings.Contains(ag.sentText, "log.txt") {
		t.Fatalf("text attachment not referenced in prompt: %q", ag.sentText)
	}
	entries := sess.Transcript()
	last := entries[len(entries)-1]
	if last.Kind != EntryUser || !strings.Contains(last.Text, "shot.png") || !strings.Contains(last.Text, "log.txt") {
		t.Fatalf("transcript entry missing attachment names: %+v", last)
	}
	// History recalls the typed text only, not the attachment plumbing.
	if h := sess.History(); len(h) != 1 || h[0] != "debug this" {
		t.Fatalf("history = %v", h)
	}
}
