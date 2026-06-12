package supervisor

import (
	"testing"
	"time"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/bus"
)

// Two permission requests arriving concurrently used to clobber each
// other: the second overwrote the single pending slot, the first's
// handler blocked forever, and the session froze at "working" with
// nothing visible to approve. Both must now resolve via the queue.
func TestConcurrentPermissionsBothResolve(t *testing.T) {
	s := New(testConfig(t), bus.New())
	sess := &Session{Name: "x", Preset: "default", status: StatusWorking}
	pf := s.permissionFunc(sess)

	results := make(chan agent.Decision, 2)
	ask := func(cmd string) {
		d, _ := pf(agent.PermissionRequest{Kind: "shell", Command: cmd, Summary: "run: " + cmd})
		results <- d
	}
	go ask("go test ./...")
	go ask("git status")

	// Wait until both are queued.
	deadline := time.After(3 * time.Second)
	for {
		if sess.View().PendingCount == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("requests not queued: count=%d", sess.View().PendingCount)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if sess.Status() != StatusWaiting {
		t.Fatalf("status = %v, want waiting", sess.Status())
	}

	// Answer them one at a time, as the UI would.
	sess.Respond(agent.ApproveOnce, "")
	sess.Respond(agent.ApproveOnce, "")

	for i := 0; i < 2; i++ {
		select {
		case d := <-results:
			if d != agent.ApproveOnce {
				t.Fatalf("request %d resolved with %v", i, d)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a permission handler is still blocked — the freeze bug")
		}
	}
	if sess.Status() != StatusWorking || sess.View().PendingCount != 0 {
		t.Fatalf("queue not drained: status=%v count=%d", sess.Status(), sess.View().PendingCount)
	}
}

// Toggling auto-approve must release every queued request, not just
// the surfaced one.
func TestAutoApproveReleasesWholeQueue(t *testing.T) {
	s := New(testConfig(t), bus.New())
	sess := &Session{Name: "x", Preset: "default", status: StatusWorking}
	pf := s.permissionFunc(sess)

	results := make(chan agent.Decision, 3)
	for i := 0; i < 3; i++ {
		go func() {
			d, _ := pf(agent.PermissionRequest{Kind: "shell", Command: "ls", Summary: "run: ls"})
			results <- d
		}()
	}
	deadline := time.After(3 * time.Second)
	for sess.View().PendingCount != 3 {
		select {
		case <-deadline:
			t.Fatalf("queue never reached 3: %d", sess.View().PendingCount)
		case <-time.After(10 * time.Millisecond):
		}
	}

	sess.SetAutoApprove(true)
	for i := 0; i < 3; i++ {
		select {
		case <-results:
		case <-time.After(3 * time.Second):
			t.Fatal("auto-approve left a handler blocked")
		}
	}
}
