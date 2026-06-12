// atc — agent traffic control: a terminal manager for parallel GitHub
// Copilot agent sessions. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/hooks"
	"github.com/rodolfojsv/atc/internal/sched"
	"github.com/rodolfojsv/atc/internal/supervisor"
	"github.com/rodolfojsv/atc/internal/tui"
)

var version = "0.1.0-dev"

func main() {
	configPath := flag.String("config", "", "config file (default: OS config dir /atc/config.json)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("atc", version)
		return
	}

	if _, err := exec.LookPath("copilot"); err != nil {
		fmt.Fprintln(os.Stderr, "atc: the `copilot` CLI was not found on PATH.")
		fmt.Fprintln(os.Stderr, "Install it and log in first: https://github.com/github/copilot-cli")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		os.Exit(1)
	}

	b := bus.New()
	hooks.New(cfg.Hooks).Attach(b)

	sup := supervisor.New(cfg, b)
	defer sup.Stop()

	m := tui.New(sup, cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	sup.SetNotify(func() { p.Send(tui.RefreshMsg{}) })

	// Reattach to sessions from the previous run (recorded in
	// ~/.atc/sessions.json); they appear on the board as they resume.
	sup.ResumeAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startSchedules(ctx, cfg, sup); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		os.Exit(1)
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		os.Exit(1)
	}
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
