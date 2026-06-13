package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/bus"
)

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

// A backend with AttachmentSender gets images inline; nothing lands on
// disk and the prompt text stays clean.
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
		t.Fatalf("prompt text altered: %q", ag.sentText)
	}
	if _, err := os.Stat(filepath.Join(dir, ".atc-attachments")); !os.IsNotExist(err) {
		t.Fatal("inline path should not write files")
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
