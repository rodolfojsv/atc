// Package logx is atc's diagnostic log: JSONL, leveled, capped, and
// silent unless enabled. It records metadata and shapes — never
// transcript content or prompts — so it stays safe on a work machine.
package logx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Level int

const (
	Off   Level = iota
	Info        // lifecycle: sessions, permissions, store, scheduler
	Debug       // + every backend event at the supervisor boundary
)

// ParseLevel accepts the config/flag spellings; unknown means Off.
func ParseLevel(s string) Level {
	switch s {
	case "info", "true", "on":
		return Info
	case "debug", "verbose":
		return Debug
	}
	return Off
}

// maxSize rotates the log on open so unattended setups can't grow an
// unbounded file: the previous log is kept once as <path>.old.
const maxSize = 5 * 1024 * 1024

type Logger struct {
	mu    sync.Mutex
	w     *os.File
	level Level
}

// Open creates a logger at path; a nil-receiver-safe no-op logger is
// returned when level is Off or the file can't be opened. DefaultPath
// is used when path is empty.
func Open(path string, level Level) *Logger {
	if level == Off {
		return nil
	}
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxSize {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return &Logger{w: f, level: level}
}

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "atc.log"
	}
	return filepath.Join(home, ".atc", "atc.log")
}

// Enabled reports whether lv would be written; usable on a nil logger.
func (l *Logger) Enabled(lv Level) bool {
	return l != nil && lv <= l.level
}

// Log writes one JSONL line: {"time":…,"level":…,"event":…, fields…}.
// Safe on a nil logger.
func (l *Logger) Log(lv Level, event string, fields map[string]any) {
	if !l.Enabled(lv) {
		return
	}
	line := map[string]any{
		"time":  time.Now().Format(time.RFC3339Nano),
		"level": [...]string{"off", "info", "debug"}[lv],
		"event": event,
	}
	for k, v := range fields {
		line[k] = v
	}
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(data, '\n'))
}

func (l *Logger) Close() {
	if l != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		_ = l.w.Close()
	}
}
