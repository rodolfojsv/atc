// Package claudeagent adapts the Claude Code CLI to atc's
// backend-neutral agent interface by driving `claude` as a subprocess
// in headless stream-JSON mode (NDJSON user messages on stdin,
// structured events on stdout).
//
// Permission model: the CLI's stream-JSON mode has no runtime
// permission callback (that exists only in the TS/Python Agent SDKs),
// so atc's approval presets map onto Claude Code's own permission
// modes: "prompt" → acceptEdits (file edits auto-approved, anything
// else is denied headlessly and reported by the agent), "allow-all" →
// bypassPermissions. Claude Code's settings.json permission rules
// still apply.
package claudeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/google/uuid"

	"github.com/rodolfojsv/atc/internal/agent"
	"github.com/rodolfojsv/atc/internal/config"
)

type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Name() string { return "claude" }

func (b *Backend) Stop() error { return nil } // each session owns its process

func (b *Backend) NewSession(_ context.Context, spec agent.SessionSpec) (agent.Session, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return nil, errors.New("the `claude` CLI was not found on PATH")
	}
	return &session{id: uuid.NewString(), spec: spec}, nil
}

func (b *Backend) ResumeSession(_ context.Context, spec agent.SessionSpec) (agent.Session, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return nil, errors.New("the `claude` CLI was not found on PATH")
	}
	return &session{id: spec.SessionID, spec: spec, started: true}, nil
}

type session struct {
	mu      sync.Mutex
	id      string
	spec    agent.SessionSpec
	started bool // a previous process used this ID: respawn with --resume
	aborted bool

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
}

func (s *session) ID() string { return s.id }

// ensureProc spawns the claude subprocess lazily — on the first Send,
// or again after an abort/crash (resuming the same conversation).
func (s *session) ensureProc() error {
	if s.cmd != nil && s.cmd.ProcessState == nil {
		return nil
	}
	args := []string{
		"--print",
		"--verbose", // required by --print with --output-format=stream-json
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
	}
	if s.started {
		args = append(args, "--resume", s.id)
	} else {
		args = append(args, "--session-id", s.id)
	}
	if s.spec.Model != "" {
		args = append(args, "--model", s.spec.Model)
	}
	switch {
	case s.spec.ReadOnly:
		args = append(args, "--permission-mode", "plan")
	case s.spec.Approval == config.ApprovalAllowAll:
		args = append(args, "--permission-mode", "bypassPermissions")
	default:
		args = append(args, "--permission-mode", "acceptEdits")
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = s.spec.WorkingDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.stdin, s.stderr = cmd, stdin, stderr
	s.started = true
	s.aborted = false
	go s.readLoop(cmd, stdout, stderr)
	return nil
}

func (s *session) Send(_ context.Context, prompt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureProc(); err != nil {
		return err
	}
	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": prompt},
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(line, '\n'))
	return err
}

// Abort kills the subprocess; the CLI has no in-stream interrupt. The
// conversation continues on the next Send via --resume.
func (s *session) Abort(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
	if s.cmd != nil && s.cmd.ProcessState == nil {
		return s.cmd.Process.Kill()
	}
	return nil
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.ProcessState == nil {
		_ = s.cmd.Process.Kill()
	}
	return nil
}

// streamLine is the top-level NDJSON envelope on stdout.
type streamLine struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"session_id"`
	Event        *apiEvent       `json:"event"`   // type == "stream_event"
	Message      *anthropicMsg   `json:"message"` // type == "assistant"
	Result       string          `json:"result"`
	IsError      bool            `json:"is_error"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Usage        *usageBlock     `json:"usage"`
	ModelUsage   json.RawMessage `json:"model_usage"`
}

type apiEvent struct {
	Type  string `json:"type"`
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type anthropicMsg struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type usageBlock struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (s *session) readLoop(cmd *exec.Cmd, stdout io.Reader, stderr *bytes.Buffer) {
	emit := s.spec.OnEvent
	sawResult := false

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var line streamLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Type {
		case "stream_event":
			if line.Event != nil && line.Event.Type == "content_block_delta" &&
				line.Event.Delta != nil && line.Event.Delta.Type == "text_delta" {
				emit(agent.Event{Type: agent.EventTextDelta, Text: line.Event.Delta.Text})
			}
		case "assistant":
			if line.Message == nil {
				continue
			}
			for _, e := range messageEvents(line.Message.Content) {
				emit(e)
			}
		case "result":
			sawResult = true
			e := agent.Event{Type: agent.EventUsage, CostUSD: line.TotalCostUSD, Model: firstModel(line.ModelUsage)}
			if line.Usage != nil {
				e.InputTokens = line.Usage.InputTokens
				e.OutputTokens = line.Usage.OutputTokens
			}
			emit(e)
			if line.IsError || (line.Subtype != "" && line.Subtype != "success") {
				emit(agent.Event{Type: agent.EventError, ErrType: line.Subtype, Text: agent.Truncate(line.Result, 400)})
			} else {
				emit(agent.Event{Type: agent.EventIdle})
			}
		}
	}
	_ = cmd.Wait()

	s.mu.Lock()
	aborted := s.aborted
	s.mu.Unlock()
	if aborted {
		emit(agent.Event{Type: agent.EventIdle})
		return
	}
	if !sawResult {
		msg := "claude process exited unexpectedly"
		if errOut := bytes.TrimSpace(stderr.Bytes()); len(errOut) > 0 {
			msg += ": " + agent.Truncate(string(errOut), 400)
		}
		emit(agent.Event{Type: agent.EventError, ErrType: "process", Text: msg})
	}
}

// messageEvents converts an assistant message's content blocks into
// transcript events. Content is either a plain string or a block array.
func messageEvents(content json.RawMessage) []agent.Event {
	var out []agent.Event
	var text string
	if json.Unmarshal(content, &text) == nil {
		if text != "" {
			out = append(out, agent.Event{Type: agent.EventMessage, Text: text})
		}
		return out
	}
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, agent.Event{Type: agent.EventMessage, Text: b.Text})
			}
		case "tool_use":
			out = append(out, agent.Event{Type: agent.EventToolStart, Text: agent.ToolSummary(b.Name, anyMap(b.Input))})
		}
	}
	return out
}

func anyMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

func firstModel(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for name := range m {
		return name
	}
	return ""
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// transcriptPath is where Claude Code persists this session's history:
// ~/.claude/projects/<cwd with non-alphanumerics dashed>/<id>.jsonl
func (s *session) transcriptPath() (string, error) {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".claude")
	}
	dir, err := filepath.Abs(s.spec.WorkingDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "projects", nonAlnum.ReplaceAllString(dir, "-"), s.id+".jsonl"), nil
}

// History replays the on-disk session transcript. Lines that don't
// match known shapes are skipped — the format is Claude Code internal.
func (s *session) History(_ context.Context) []agent.Event {
	path, err := s.transcriptPath()
	if err != nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	type historyLine struct {
		Type    string `json:"type"`
		IsMeta  bool   `json:"isMeta"`
		Message *struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	var out []agent.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var line historyLine
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.IsMeta || line.Message == nil {
			continue
		}
		switch line.Type {
		case "user":
			// User entries carry either real prompts or tool results;
			// only plain text blocks are prompts.
			var text string
			if json.Unmarshal(line.Message.Content, &text) == nil {
				if text != "" {
					out = append(out, agent.Event{Type: agent.EventUserMessage, Text: text})
				}
				continue
			}
			var blocks []contentBlock
			if json.Unmarshal(line.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					out = append(out, agent.Event{Type: agent.EventUserMessage, Text: b.Text})
				}
			}
		case "assistant":
			out = append(out, messageEvents(line.Message.Content)...)
		}
	}
	return out
}
