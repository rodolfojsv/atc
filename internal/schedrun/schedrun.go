// Package schedrun persists the outcome of every scheduled-task fire to an
// append-only log (~/.atc/schedule-runs.jsonl). Each fire records whether
// the task ran (its precheck passed), was skipped (precheck reported no
// change), or errored (the precheck or session failed to start). The UI
// reads this log to show a scheduled task's history and the "no updates
// since X" line without ever spending tokens on a quiet fire.
package schedrun

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Result is the outcome of a single scheduled-task fire.
type Result string

const (
	// Updated means the precheck passed and a session was launched.
	Updated Result = "updated"
	// NoUpdate means the precheck exited non-zero — nothing changed, so
	// the prompt was skipped and no tokens were spent.
	NoUpdate Result = "no-update"
	// Errored means the precheck could not run or the session failed to
	// start; the prompt did not run.
	Errored Result = "error"
)

// Run is one entry in the schedule run log.
type Run struct {
	Schedule string    `json:"schedule"`
	Time     time.Time `json:"time"`
	Result   Result    `json:"result"`
	// Session is the name of the session launched on an Updated fire, so
	// the UI can deep-link the run to its chat. Empty otherwise.
	Session string `json:"session,omitempty"`
	// Detail carries the precheck/error message on an Errored fire.
	Detail string `json:"detail,omitempty"`
}

// Log is an append-only run log backed by a JSONL file. The zero value
// (empty path) is a no-op sink, so callers without a home dir degrade
// gracefully instead of erroring.
type Log struct {
	path string
}

// Default returns the log at ~/.atc/schedule-runs.jsonl, alongside the
// session store. A missing home dir yields a no-op log.
func Default() Log {
	home, err := os.UserHomeDir()
	if err != nil {
		return Log{}
	}
	return Log{path: filepath.Join(home, ".atc", "schedule-runs.jsonl")}
}

// New returns a log backed by an explicit path (used in tests).
func New(path string) Log { return Log{path: path} }

// Append writes one run as a JSON line. It opens the file in append mode
// so the in-process scheduler and a headless `atc run` can both record
// concurrently — each small line lands atomically via O_APPEND.
func (l Log) Append(r Run) error {
	if l.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// All returns every recorded run in file order (oldest first). A missing
// file yields no runs and no error; malformed lines are skipped.
func (l Log) All() ([]Run, error) {
	if l.path == "" {
		return nil, nil
	}
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Run
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Run
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
