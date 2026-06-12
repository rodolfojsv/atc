package logx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLevelsAndShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atc.log")
	l := Open(path, Info)
	defer l.Close()

	l.Log(Info, "permission.enqueued", map[string]any{"session": "x", "queued": 2})
	l.Log(Debug, "event.idle", map[string]any{"session": "x"}) // below level: dropped

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d: %q", len(lines), data)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["event"] != "permission.enqueued" || rec["session"] != "x" || rec["level"] != "info" {
		t.Errorf("bad record: %v", rec)
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	var l *Logger // Off level returns nil
	l.Log(Info, "x", nil)
	l.Close()
	if l.Enabled(Info) {
		t.Error("nil logger must report disabled")
	}
	if Open("", Off) != nil {
		t.Error("Off level must return nil logger")
	}
}

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atc.log")
	if err := os.WriteFile(path, make([]byte, maxSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	l := Open(path, Info)
	defer l.Close()
	l.Log(Info, "atc.start", nil)
	if fi, _ := os.Stat(path); fi.Size() > 1024 {
		t.Error("log not rotated on open")
	}
	if _, err := os.Stat(path + ".old"); err != nil {
		t.Error("previous log not kept as .old")
	}
}

func TestParseLevel(t *testing.T) {
	for s, want := range map[string]Level{"": Off, "off": Off, "info": Info, "true": Info, "debug": Debug, "nonsense": Off} {
		if got := ParseLevel(s); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", s, got, want)
		}
	}
}
