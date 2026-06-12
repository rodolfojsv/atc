// Package bus is atc's in-process event bus. The supervisor publishes
// session lifecycle events; the TUI, hook dispatcher, and (later) the
// remote API subscribe to them.
package bus

import (
	"slices"
	"sync"
	"time"
)

// Event types. These names double as the keys of the "hooks" map in
// config.json, so renaming one is a breaking config change.
const (
	SessionStarted      = "session-started"
	WaitingOnPermission = "waiting-on-permission"
	PermissionResolved  = "permission-resolved"
	Finished            = "finished"
	Error               = "error"
	ToolCall            = "tool-call"
	SessionClosed       = "session-closed"
)

type Event struct {
	Type        string         `json:"type"`
	SessionID   string         `json:"sessionId"`
	SessionName string         `json:"sessionName"`
	Time        time.Time      `json:"time"`
	Data        map[string]any `json:"data,omitempty"`
}

type Bus struct {
	mu   sync.RWMutex
	subs []func(Event)
}

func New() *Bus { return &Bus{} }

func (b *Bus) Subscribe(fn func(Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, fn)
}

// Publish dispatches the event to all subscribers on a separate
// goroutine (one per event, subscribers in order), so publishers —
// including SDK event handlers — never block on slow hooks.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.RLock()
	subs := slices.Clone(b.subs)
	b.mu.RUnlock()
	go func() {
		for _, fn := range subs {
			fn(e)
		}
	}()
}
