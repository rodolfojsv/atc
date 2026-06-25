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
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
	"github.com/rodolfojsv/atc/internal/hooks"
	"github.com/rodolfojsv/atc/internal/ntfy"
	"github.com/rodolfojsv/atc/internal/sched"
	"github.com/rodolfojsv/atc/internal/schedrun"
	"github.com/rodolfojsv/atc/internal/supervisor"
	"github.com/rodolfojsv/atc/internal/tui"
	"github.com/rodolfojsv/atc/internal/web"
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
		case "serve":
			os.Exit(cmdServe(args[1:]))
		}
	}
	os.Exit(cmdTUI(args))
}

func cmdTUI(argv []string) int {
	fs := flag.NewFlagSet("atc", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: OS config dir /atc/config.json)")
	showVersion := fs.Bool("version", false, "print version and exit")
	debugFlag := fs.Bool("debug", false, "diagnostic log at debug level (overrides config logLevel)")
	serveFlag := fs.Bool("serve", false, "also serve the web UI (config web.addr, default 127.0.0.1:8787)")
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
	attachNtfy(b, sup, cfg, cfg.Web.Token)

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
	// Auto-clean finished scheduled sessions per config.scheduledRetentionDays
	// (no-op when unset). Hourly is plenty for a day-grained retention.
	go sup.PruneScheduledLoop(ctx, time.Hour)
	if err := startSchedules(ctx, *configPath, sup); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}

	if *serveFlag {
		srv := web.New(sup, cfg, cfg.Web.Token)
		url, err := srv.Start(cfg.Web.Addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "atc: web:", err)
			return 1
		}
		// Visible in the TUI footer flash; the full tokenized URL also
		// lands in scrollback above the alt screen.
		fmt.Fprintln(os.Stderr, "atc: web UI at", url)
		go func() { p.Send(tui.NewFlash("web UI: " + url)) }()
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}
	return 0
}

// cmdServe runs the supervisor with the web UI as its only frontend —
// no TUI. Sessions resume, schedules fire, and permission requests wait
// for an answer from the browser, exactly like the TUI flow.
func cmdServe(argv []string) int {
	fs := flag.NewFlagSet("atc serve", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: OS config dir /atc/config.json)")
	addr := fs.String("addr", "", "listen address (default: config web.addr, else 127.0.0.1:8787)")
	tokenFlag := fs.String("token", "", "access token (default: config web.token, else random per run)")
	debugFlag := fs.Bool("debug", false, "diagnostic log at debug level")
	_ = fs.Parse(argv)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}
	if *debugFlag {
		cfg.LogLevel = "debug"
	}
	if *addr == "" {
		*addr = cfg.Web.Addr
	}
	token := *tokenFlag
	if token == "" {
		token = cfg.Web.Token
	}

	b := bus.New()
	hooks.New(cfg.Hooks).Attach(b)
	sup := supervisor.New(cfg, b)
	defer sup.Stop()
	attachNtfy(b, sup, cfg, token)

	srv := web.New(sup, cfg, token)
	url, err := srv.Start(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc: web:", err)
		return 1
	}
	fmt.Println("atc web UI:", url)
	fmt.Println("tailnet access: `tailscale serve --bg " + portOf(url) + "` (and `tailscale serve off` when done)")

	sup.ResumeAll()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.WatchStore(ctx, 3*time.Second)
	go sup.PruneScheduledLoop(ctx, time.Hour)
	if err := startSchedules(ctx, *configPath, sup); err != nil {
		fmt.Fprintln(os.Stderr, "atc:", err)
		return 1
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("atc: shutting down (sessions stay resumable)")
	return 0
}

// attachNtfy subscribes an ntfy push publisher to the bus when it's
// enabled in config. atcToken is the effective web token (used only for
// Approve/Deny action buttons). No-op when ntfy is disabled.
func attachNtfy(b *bus.Bus, sup *supervisor.Supervisor, cfg *config.Config, atcToken string) {
	if !cfg.Ntfy.Enabled {
		return
	}
	pub := ntfy.New(cfg.Ntfy, atcToken, func(name string) string {
		if s := sup.SessionByName(name); s != nil {
			return s.View().NotifyTopic
		}
		return ""
	})
	b.Subscribe(pub.OnEvent)
}

// portOf extracts the port from a URL like http://127.0.0.1:8787/?token=…
func portOf(url string) string {
	rest := strings.TrimPrefix(url, "http://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		return rest[i+1:]
	}
	return "8787"
}

// startSchedules launches the in-process scheduler. It re-reads the config
// file whenever its mtime changes, so schedules added, edited, or removed
// take effect without restarting the process; a config that fails to parse
// is ignored and the previous good schedule set keeps firing. Only the
// schedule list is hot-reloaded — presets, defaultAutoApprove and other
// config the supervisor captured at startup still require a restart.
func startSchedules(ctx context.Context, configPath string, sup *supervisor.Supervisor) error {
	runLog := schedrun.Default()

	path := configPath
	if path == "" {
		p, err := config.Path()
		if err != nil {
			return err
		}
		path = p
	}

	var (
		lastMod time.Time
		cached  []sched.Job
	)
	// build returns the current job set, reparsing only when the config
	// file's mtime moved. A missing file keeps the current set (treated as
	// "no change"); a parse error is surfaced so the caller can keep the
	// last good set rather than dropping every schedule.
	build := func() ([]sched.Job, error) {
		info, err := os.Stat(path)
		if err != nil {
			return cached, nil
		}
		if mod := info.ModTime(); !mod.Equal(lastMod) {
			lastMod = mod
			cfg, err := config.Load(path)
			if err != nil {
				return nil, err
			}
			jobs, err := buildScheduleJobs(cfg, sup, runLog)
			if err != nil {
				return nil, err
			}
			cached = jobs
		}
		return cached, nil
	}

	// Build once up front so a bad config still fails fast at startup.
	if _, err := build(); err != nil {
		return err
	}
	go sched.RunReloadable(ctx, build, func(err error) {
		fmt.Fprintln(os.Stderr, "atc: schedule reload skipped, keeping previous schedules:", err)
	})
	return nil
}

func buildScheduleJobs(cfg *config.Config, sup *supervisor.Supervisor, runLog schedrun.Log) ([]sched.Job, error) {
	var jobs []sched.Job
	for _, s := range cfg.Schedules {
		// A disabled schedule stays in config (and on the board) but never
		// fires: skip building a job for it. Its cron is then not validated, so
		// a disabled entry with a stale cron can't fail the whole reload.
		if s.Disabled {
			continue
		}
		entry, err := sched.Parse(s.Cron)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", s.Name, err)
		}
		s := s
		jobs = append(jobs, sched.Job{Entry: entry, Fire: func() {
			fireSchedule(s, sup, runLog)
		}})
	}
	return jobs, nil
}

// fireSchedule runs one scheduled task: it consults the precheck (if any),
// launches a session only when something changed, and records the outcome
// in the run log so the UI can show "no updates since X" without spending
// tokens on a quiet fire.
func fireSchedule(s config.Schedule, sup *supervisor.Supervisor, runLog schedrun.Log) {
	name := s.Name
	if name == "" {
		name = "sched"
	}
	rec := func(r schedrun.Run) {
		r.Schedule, r.Time = name, time.Now()
		_ = runLog.Append(r)
	}

	if s.Precheck != "" {
		run, err := runPrecheck(s.Precheck, s.Repo)
		if err != nil {
			rec(schedrun.Run{Result: schedrun.Errored, Detail: "precheck: " + err.Error()})
			return
		}
		if !run {
			rec(schedrun.Run{Result: schedrun.NoUpdate})
			return
		}
	}

	sess, err := sup.NewSession(supervisor.NewSessionOptions{
		Name:         name,
		Repo:         s.Repo,
		Preset:       s.Preset,
		Agent:        s.Agent,
		UseWorktree:  s.Worktree,
		Prompt:       s.Prompt,
		ReadOnly:     !s.Write, // scheduled tasks are read-only unless write:true
		ScheduleName: name,
	})
	if err != nil {
		rec(schedrun.Run{Result: schedrun.Errored, Detail: err.Error()})
		return
	}
	rec(schedrun.Run{Result: schedrun.Updated, Session: sess.Name})
}
