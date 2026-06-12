// Package hooks runs user-configured subprocess hooks on bus events:
// config maps an event type to an argv, and the event is delivered as
// JSON on the subprocess's stdin. This is atc's whole plugin model —
// any language, each "plugin" a readable script.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/rodolfojsv/atc/internal/bus"
)

// Timeout bounds a single hook run; a hook that hangs must not pile up
// goroutines forever.
const Timeout = 60 * time.Second

type Dispatcher struct {
	hooks map[string][]string
}

func New(hooks map[string][]string) *Dispatcher {
	return &Dispatcher{hooks: hooks}
}

func (d *Dispatcher) Attach(b *bus.Bus) {
	b.Subscribe(d.handle)
}

func (d *Dispatcher) handle(e bus.Event) {
	argv := d.hooks[e.Type]
	if len(argv) == 0 {
		return
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	// Hooks are fire-and-forget; their failures must never affect sessions.
	_ = cmd.Run()
}
