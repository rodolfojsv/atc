// Package tmux is a thin, generic wrapper over the `tmux` CLI for driving a
// long-lived interactive program from Go without owning a pseudo-terminal.
//
// The tmux server is a daemon: it allocates the real PTY for each pane and
// keeps the program alive independent of this process. We interact through
// stateless client subcommands (new-session, send-keys, capture-pane,
// has-session, kill-session) that connect, do one thing, and exit — so the
// driven program survives a crash or restart of the caller. That durability,
// plus the fact that the program runs under a genuine terminal, is the whole
// reason for routing through tmux instead of a bare PTY.
//
// This package is deliberately free of any application specifics; callers
// supply the command, geometry, and environment.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Client runs tmux subcommands. The zero value is not usable; call New.
type Client struct {
	bin string // resolved path to the tmux binary
}

// New locates the tmux binary on PATH and returns a Client. It errors when
// tmux is not installed, mirroring how the caller already gates on `claude`.
func New() (*Client, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return nil, errors.New("the `tmux` CLI was not found on PATH")
	}
	return &Client{bin: bin}, nil
}

// NewSessionOpts configures a detached session.
type NewSessionOpts struct {
	// Name is the tmux session name and the target for every later call. Keep
	// it simple (alphanumerics and dashes); tmux treats some characters
	// specially in target specs.
	Name string
	// Command is the program (and args) to run as the pane's process. Required.
	Command []string
	// WorkingDir is the pane's starting directory ("" = inherit).
	WorkingDir string
	// Width and Height set the window geometry. Fixed dimensions keep
	// capture-pane output stable for scraping; zero falls back to tmux defaults.
	Width, Height int
	// Env entries ("KEY=VALUE") are set in the session via `-e`. Use this to
	// scope variables to the pane rather than the whole tmux server.
	Env []string
}

// NewSession creates a detached session that immediately runs Command. The
// session (and its program) outlives this process until KillSession or a
// program exit.
func (c *Client) NewSession(ctx context.Context, opts NewSessionOpts) error {
	if opts.Name == "" {
		return errors.New("tmux: session name is required")
	}
	if len(opts.Command) == 0 {
		return errors.New("tmux: command is required")
	}
	args := []string{"new-session", "-d", "-s", opts.Name}
	if opts.WorkingDir != "" {
		args = append(args, "-c", opts.WorkingDir)
	}
	if opts.Width > 0 {
		args = append(args, "-x", strconv.Itoa(opts.Width))
	}
	if opts.Height > 0 {
		args = append(args, "-y", strconv.Itoa(opts.Height))
	}
	for _, e := range opts.Env {
		args = append(args, "-e", e)
	}
	// `--` ends option parsing so a command starting with '-' is safe.
	args = append(args, "--")
	args = append(args, opts.Command...)
	_, err := c.run(ctx, args...)
	return err
}

// HasSession reports whether a session with the given name exists.
func (c *Client) HasSession(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, c.bin, "has-session", "-t", name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// has-session exits non-zero (with a "can't find session" message) when
		// the session is absent — that's a clean false, not an error. A real
		// failure (e.g. tmux missing) won't carry that message.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("tmux has-session: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return true, nil
}

// KillSession terminates a session and its program. Killing a session that
// does not exist is treated as success (idempotent teardown).
func (c *Client) KillSession(ctx context.Context, name string) error {
	ok, err := c.HasSession(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	_, err = c.run(ctx, "kill-session", "-t", name)
	return err
}

// SendText types literal text into the pane (no key-name interpretation), so
// content like "Enter" or text starting with '-' is sent verbatim. It does
// not submit; follow with SendEnter.
func (c *Client) SendText(ctx context.Context, name, text string) error {
	_, err := c.run(ctx, "send-keys", "-t", name, "-l", "--", text)
	return err
}

// SendKeys sends one or more tmux key names (interpreted), e.g. "Enter",
// "Escape", "C-c". Use SendText for literal prompt content.
func (c *Client) SendKeys(ctx context.Context, name string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := append([]string{"send-keys", "-t", name}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// SendEnter submits the current pane input.
func (c *Client) SendEnter(ctx context.Context, name string) error {
	return c.SendKeys(ctx, name, "Enter")
}

// CaptureOpts tunes a capture-pane read.
type CaptureOpts struct {
	// History includes the full scrollback (capture-pane -S -), not just the
	// visible screen. Pair with a high history-limit (see SetOption).
	History bool
	// JoinWrapped joins lines that tmux wrapped at the pane width (-J), so a
	// long logical line reads as one. Useful for scraping prose.
	JoinWrapped bool
	// Escapes preserves ANSI escape sequences (-e). Off by default: plain,
	// already-rendered text is what most scraping wants.
	Escapes bool
}

// Capture returns the pane contents as text. With default opts it returns the
// visible screen as rendered plain text — tmux is the terminal emulator, so
// the caller needs no VT parsing of its own.
func (c *Client) Capture(ctx context.Context, name string, opts CaptureOpts) (string, error) {
	args := []string{"capture-pane", "-p", "-t", name}
	if opts.History {
		args = append(args, "-S", "-")
	}
	if opts.JoinWrapped {
		args = append(args, "-J")
	}
	if opts.Escapes {
		args = append(args, "-e")
	}
	return c.run(ctx, args...)
}

// SetOption sets a session option, e.g. SetOption(ctx, name, "history-limit",
// "50000") so Capture with History can see output that scrolled off-screen.
func (c *Client) SetOption(ctx context.Context, name, option, value string) error {
	_, err := c.run(ctx, "set-option", "-t", name, option, value)
	return err
}

// run executes a tmux subcommand and returns its stdout, wrapping failures
// with the stderr text tmux prints on error.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
