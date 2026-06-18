package supervisor

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Optional diagnostics shared with the claude backend: when ATC_CLAUDE_TRACE
// names a writable file, strace appends a timestamped, "[sup]"-tagged line so
// supervisor-side routing decisions interleave with the backend's own trace.
// No-op (one env lookup) when unset.
var (
	straceOnce sync.Once
	straceFile *os.File
	straceMu   sync.Mutex
)

func strace(format string, args ...any) {
	straceOnce.Do(func() {
		if p := strings.TrimSpace(os.Getenv("ATC_CLAUDE_TRACE")); p != "" {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				straceFile = f
			}
		}
	})
	if straceFile == nil {
		return
	}
	straceMu.Lock()
	defer straceMu.Unlock()
	fmt.Fprintf(straceFile, time.Now().Format("15:04:05.000")+" [sup] "+format+"\n", args...)
}
