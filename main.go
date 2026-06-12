// atc — agent traffic control: a terminal manager for parallel AI
// agent sessions (GitHub Copilot, Claude Code). See README.md.
//
// Usage:
//
//	atc                  the TUI (default)
//	atc run …            one headless session, for Task Scheduler/cron
//	atc schedule …       register config schedules with Windows Task Scheduler
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/hooks"
	"github.com/rodolfojsv/atc/internal/sched"
	"github.com/rodolfojsv/atc/internal/supervisor"
	"github.com/rodolfojsv/atc/internal/tui"
)

var version = "0.1.0-dev"

// buildRevision reports the VCS commit Go embedded at build time, so
// `atc --version` can prove which build is actually on PATH.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev == "" {
		return "(no build info)"
	}
	return rev + dirty
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "run":
			os.Exit(cmdRun(args[1:]))
		case "schedule":
			os.Exit(cmdSchedule(args[1:]))
		}
	}
	os.Exit(cmdTUI(args))
}

func cmdTUI(argv []string) int {
	fs := flag.NewFlagSet("atc", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: OS config dir /atc/config.json)")
	showVersion := fs.Bool("version", false, "print version and exit")
	debugFlag := fs.Bool("debug", false, "diagnostic log at debug level (overrides config logLevel)")
	_ = fs.Parse(argv)

	if *showVersion {
		fmt.Println("atc", version, buildRevision())
		return 0
	}

	_, copilotErr := exec.LookPath("copilot")
	_, claudeErr := exec.LookPath("claude")
	if copilotErr != nil && claudeErr != nil {
		fmt.Fprintln(os.Stderr, "atc: neither `copilot` nor `claude` was found on PATH; install at least one backend CLI.")
		fmt.Fprintln(os.Stderr, "Copilot: https://github.com/github/copilot-cli · Claude Code: https://claude.com/claude-code")
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}
	if *debugFlag {
		cfg.LogLevel = "debug"
	}

	b := bus.New()
	hooks.New(cfg.Hooks).Attach(b)

	sup := supervisor.New(cfg, b)
	defer sup.Stop()

	m := tui.New(sup, cfg)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	sup.SetNotify(func() { p.Send(tui.RefreshMsg{}) })

	// Reattach to sessions from the previous run (recorded in
	// ~/.atc/sessions.json); they appear on the board as they resume.
	sup.ResumeAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Adopt sessions other atc processes finish while we're open —
	// e.g. Task Scheduler `atc run` jobs land on the board live.
	go sup.WatchStore(ctx, 3*time.Second)
	if err := startSchedules(ctx, cfg, sup); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}
	return 0
}

func startSchedules(ctx context.Context, cfg *config.Config, sup *supervisor.Supervisor) error {
	var jobs []sched.Job
	for _, s := range cfg.Schedules {
		entry, err := sched.Parse(s.Cron)
		if err != nil {
			return fmt.Errorf("schedule %q: %w", s.Name, err)
		}
		s := s
		jobs = append(jobs, sched.Job{Entry: entry, Fire: func() {
			name := s.Name
			if name == "" {
				name = "sched"
			}
			_, _ = sup.NewSession(supervisor.NewSessionOptions{
				Name:        name,
				Repo:        s.Repo,
				Preset:      s.Preset,
				UseWorktree: s.Worktree,
				Prompt:      s.Prompt,
			})
		}})
	}
	go sched.Run(ctx, jobs)
	return nil
}
