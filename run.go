package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/export"
	"github.com/rodolfojsv/atc/internal/hooks"
	"github.com/rodolfojsv/atc/internal/supervisor"
)

// cmdRun executes one headless session and exits — the unit Windows
// Task Scheduler (or any cron) invokes. The transcript streams to
// stdout as plain text, hooks fire as usual, and the session lands in
// the resume store so the TUI can pick it up later.
//
// Exit codes: 0 done, 1 error, 2 timeout.
func cmdRun(argv []string) int {
	fs := flag.NewFlagSet("atc run", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: OS config dir /atc/config.json)")
	scheduleName := fs.String("schedule", "", "load repo/prompt/preset from this named schedule in config")
	repo := fs.String("repo", "", "repository or directory to run in")
	prompt := fs.String("prompt", "", "the prompt to run")
	preset := fs.String("preset", "", "preset name (use an allow-all preset for unattended runs)")
	backend := fs.String("backend", "", "copilot (default) or claude")
	model := fs.String("model", "", "model override")
	name := fs.String("name", "", "session name")
	worktree := fs.Bool("worktree", false, "run in a fresh git worktree")
	doExport := fs.Bool("export", false, "export the transcript as markdown on completion (or set autoExport in config)")
	debugFlag := fs.Bool("debug", false, "diagnostic log at debug level (overrides config logLevel)")
	timeout := fs.Duration("timeout", 60*time.Minute, "abort the run after this long")
	_ = fs.Parse(argv)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc run:", err)
		return 1
	}
	if *debugFlag {
		cfg.LogLevel = "debug"
	}

	opts := supervisor.NewSessionOptions{
		Name: *name, Repo: *repo, Prompt: *prompt, Preset: *preset,
		Backend: *backend, Model: *model, UseWorktree: *worktree,
	}
	if *scheduleName != "" {
		found := false
		for _, s := range cfg.Schedules {
			if s.Name == *scheduleName {
				if opts.Repo == "" {
					opts.Repo = s.Repo
				}
				if opts.Prompt == "" {
					opts.Prompt = s.Prompt
				}
				if opts.Preset == "" {
					opts.Preset = s.Preset
				}
				if opts.Name == "" {
					opts.Name = s.Name
				}
				if !opts.UseWorktree {
					opts.UseWorktree = s.Worktree
				}
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "atc run: schedule %q not found in config\n", *scheduleName)
			return 1
		}
	}
	if opts.Repo == "" || opts.Prompt == "" {
		fmt.Fprintln(os.Stderr, "atc run: --repo and --prompt are required (or --schedule naming a config entry)")
		return 1
	}

	b := bus.New()
	hooks.New(cfg.Hooks).Attach(b)
	sup := supervisor.New(cfg, b)
	sup.SetHeadless(true)
	defer sup.Stop()

	sess, err := sup.NewSession(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc run:", err)
		return 1
	}
	fmt.Printf("atc run: session %q in %s\n", sess.Name, opts.Repo)

	deadline := time.Now().Add(*timeout)
	printed := 0
	flush := func() {
		for _, e := range sess.Transcript()[printed:] {
			if e.Partial {
				break
			}
			fmt.Println(formatEntry(e))
			printed++
		}
	}
	for {
		flush()
		st := sess.Status()
		if st == supervisor.StatusDone || st == supervisor.StatusError {
			break
		}
		if time.Now().After(deadline) {
			flush()
			fmt.Println("✗ timeout reached, aborting")
			sup.Abort(sess)
			return 2
		}
		time.Sleep(300 * time.Millisecond)
	}
	flush()

	v := sess.View()
	fmt.Printf("\n— %s · %s↑ %s↓", v.Status, tokens(v.Usage.InputTokens), tokens(v.Usage.OutputTokens))
	if v.Usage.NanoAiu > 0 {
		fmt.Printf(" · %.2f AIC", v.Usage.NanoAiu/1e9)
	}
	if v.Usage.CostUSD > 0 {
		fmt.Printf(" · $%.2f", v.Usage.CostUSD)
	}
	fmt.Println()
	if *doExport || cfg.AutoExport {
		if path, err := export.Write(cfg.ExportDir, v, sess.Transcript()); err == nil {
			fmt.Println("exported →", path)
		} else {
			fmt.Fprintln(os.Stderr, "atc run: export:", err)
		}
	}
	if v.Status == supervisor.StatusError {
		fmt.Fprintln(os.Stderr, "atc run:", v.Err)
		return 1
	}
	return 0
}

func formatEntry(e supervisor.Entry) string {
	switch e.Kind {
	case supervisor.EntryUser:
		return "\n❯ " + e.Text
	case supervisor.EntryAssistant:
		return "\n" + e.Text
	case supervisor.EntryTool:
		return "  ⚙ " + e.Text
	case supervisor.EntrySystem:
		return "  · " + e.Text
	case supervisor.EntryError:
		return "  ✗ " + e.Text
	}
	return e.Text
}

func tokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
