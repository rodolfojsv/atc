package supervisor

import (
	"strings"
	"testing"
)

// assistantEntries returns the text of every committed assistant entry
// (the in-flight Partial trailing entry is excluded by reading only the
// finalized transcript via a finished stream).
func assistantTexts(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		if e.Kind == EntryAssistant {
			out = append(out, e.Text)
		}
	}
	return out
}

// The canonical ordering for both backends is: text streams, the
// authoritative message arrives, THEN any tool runs. finishMessage clears
// the buffer before the tool flush, so the message appears exactly once.
func TestTranscriptCanonicalOrderNoDuplicate(t *testing.T) {
	sess := &Session{Name: "x"}
	sess.appendStream("Let me look at this.")
	sess.finishMessage("Let me look at this.") // message before the tool
	sess.appendEntry(EntryTool, "Read(main.go)")

	got := assistantTexts(sess.Transcript())
	want := []string{"Let me look at this."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant entries = %v, want %v", got, want)
	}
}

// Regression: when a tool start arrives mid-stream (before the
// authoritative message), the partial text is flushed as a permanent
// entry. If the backend's full message then repeats that prefix, the
// pre-tool text must not appear twice.
func TestTranscriptToolInterleavedNoDuplicate(t *testing.T) {
	sess := &Session{Name: "x"}
	sess.appendStream("Let me look at this.")  // deltas
	sess.appendEntry(EntryTool, "Read(main.go)") // tool flushes the partial
	sess.appendStream("Now I'll fix it.")        // more deltas
	// Authoritative message carries the full turn text, including the
	// already-flushed prefix.
	sess.finishMessage("Let me look at this.\nNow I'll fix it.")

	tr := sess.Transcript()
	got := assistantTexts(tr)
	want := []string{"Let me look at this.", "Now I'll fix it."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant entries = %v, want %v (full transcript %+v)", got, want, tr)
	}
	// Ordering: the pre-tool text stays before the tool, the remainder after.
	if len(tr) != 3 || tr[0].Kind != EntryAssistant || tr[1].Kind != EntryTool || tr[2].Kind != EntryAssistant {
		t.Fatalf("unexpected ordering: %+v", tr)
	}
}

// When the authoritative message exactly equals what was already flushed
// (no new text after the tool), nothing extra is appended.
func TestTranscriptFullyShownMessageNotReappended(t *testing.T) {
	sess := &Session{Name: "x"}
	sess.appendStream("Reading the file.")
	sess.appendEntry(EntryTool, "Read(main.go)") // flushes "Reading the file."
	sess.finishMessage("Reading the file.")      // identical authoritative content

	got := assistantTexts(sess.Transcript())
	want := []string{"Reading the file."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant entries = %v, want %v", got, want)
	}
}

// A fresh user turn must not let stale flushed text suppress the next
// message even if it happens to share a prefix.
func TestTranscriptUserTurnResetsDedup(t *testing.T) {
	sess := &Session{Name: "x"}
	sess.appendStream("Done.")
	sess.appendEntry(EntryUser, "do it again") // flushes "Done.", opens new turn
	sess.finishMessage("Done.")                 // a genuine new identical reply

	got := assistantTexts(sess.Transcript())
	want := []string{"Done.", "Done."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant entries = %v, want %v", got, want)
	}
}

// Multiple mid-stream tool flushes accumulate; the final message strips
// the whole shown prefix.
func TestTranscriptMultipleFlushesNoDuplicate(t *testing.T) {
	sess := &Session{Name: "x"}
	sess.appendStream("First.\n")
	sess.appendEntry(EntryTool, "Read(a.go)") // flush "First."
	sess.appendStream("Second.\n")
	sess.appendEntry(EntryTool, "Read(b.go)") // flush "Second."
	sess.appendStream("Third.")
	sess.finishMessage("First.\nSecond.\nThird.")

	got := assistantTexts(sess.Transcript())
	want := []string{"First.", "Second.", "Third."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant entries = %v, want %v", got, want)
	}
}
