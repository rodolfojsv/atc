package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/rodolfojsv/atc/internal/config"
)

// cmdSchedule registers the config's schedules with Windows Task
// Scheduler: each becomes a task `atc\<name>` whose action is
// `atc.exe run --schedule <name>`. The task carries only the name —
// repo/prompt/preset are read from config at fire time, so editing a
// schedule's prompt never requires re-registering.
func cmdSchedule(argv []string) int {
	if len(argv) == 0 || (argv[0] != "install" && argv[0] != "uninstall" && argv[0] != "list") {
		fmt.Fprintln(os.Stderr, "usage: atc schedule <install|uninstall|list> [--config path]")
		return 1
	}
	sub := argv[0]
	fs := flag.NewFlagSet("atc schedule", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: OS config dir /atc/config.json)")
	_ = fs.Parse(argv[1:])

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc schedule:", err)
		return 1
	}
	if len(cfg.Schedules) == 0 {
		fmt.Println("no schedules in config")
		return 0
	}

	if sub == "list" {
		for _, s := range cfg.Schedules {
			timing, err := cronToSchtasks(s.Cron)
			translation := strings.Join(timing, " ")
			if err != nil {
				translation = "(not translatable: " + err.Error() + ")"
			}
			fmt.Printf("%-16s %-14s → %-32s %s\n", s.Name, s.Cron, translation, s.Repo)
		}
		return 0
	}

	if runtime.GOOS != "windows" {
		fmt.Fprintln(os.Stderr, "atc schedule "+sub+" drives schtasks.exe and only works on Windows.")
		fmt.Fprintln(os.Stderr, "On Linux/macOS use the in-process scheduler (keep atc open, e.g. in tmux) or system cron with `atc run --schedule <name>`.")
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "atc schedule:", err)
		return 1
	}

	rc := 0
	for _, s := range cfg.Schedules {
		if s.Name == "" {
			fmt.Fprintf(os.Stderr, "✗ schedule with cron %q has no name — names are required for Task Scheduler registration\n", s.Cron)
			rc = 1
			continue
		}
		taskName := `atc\` + s.Name

		if sub == "uninstall" {
			out, err := exec.Command("schtasks", "/Delete", "/F", "/TN", taskName).CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "✗ %s: %s\n", taskName, strings.TrimSpace(string(out)))
				rc = 1
			} else {
				fmt.Printf("✓ removed %s\n", taskName)
			}
			continue
		}

		timing, err := cronToSchtasks(s.Cron)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: cron %q: %v — register it manually with schtasks against: %s run --schedule %s\n", s.Name, s.Cron, err, exe, s.Name)
			rc = 1
			continue
		}
		action := fmt.Sprintf(`"%s" run --schedule %s`, exe, s.Name)
		if *configPath != "" {
			action += fmt.Sprintf(` --config "%s"`, *configPath)
		}
		args := append([]string{"/Create", "/F", "/TN", taskName, "/TR", action}, timing...)
		out, err := exec.Command("schtasks", args...).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %s\n", taskName, strings.TrimSpace(string(out)))
			rc = 1
		} else {
			fmt.Printf("✓ registered %s (%s)\n", taskName, strings.Join(timing, " "))
		}
	}
	return rc
}

// cronToSchtasks translates the cron subset that maps cleanly onto
// schtasks timing flags: daily, weekly (day-of-week lists/ranges),
// monthly (single day-of-month), and every-N-minutes.
func cronToSchtasks(expr string) ([]string, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("want 5 cron fields, got %d", len(f))
	}
	min, hour, dom, mon, dow := f[0], f[1], f[2], f[3], f[4]
	if mon != "*" {
		return nil, fmt.Errorf("month field is not supported")
	}

	if strings.HasPrefix(min, "*/") && hour == "*" && dom == "*" && dow == "*" {
		n, err := strconv.Atoi(min[2:])
		if err != nil || n < 1 || n > 1439 {
			return nil, fmt.Errorf("bad minute step %q", min)
		}
		return []string{"/SC", "MINUTE", "/MO", strconv.Itoa(n)}, nil
	}

	m, err1 := strconv.Atoi(min)
	h, err2 := strconv.Atoi(hour)
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return nil, fmt.Errorf("only fixed minute+hour are supported (got %q %q)", min, hour)
	}
	st := fmt.Sprintf("%02d:%02d", h, m)

	switch {
	case dom == "*" && dow == "*":
		return []string{"/SC", "DAILY", "/ST", st}, nil
	case dom == "*":
		days, err := dowToSchtasks(dow)
		if err != nil {
			return nil, err
		}
		return []string{"/SC", "WEEKLY", "/D", days, "/ST", st}, nil
	case dow == "*":
		d, err := strconv.Atoi(dom)
		if err != nil || d < 1 || d > 31 {
			return nil, fmt.Errorf("bad day-of-month %q", dom)
		}
		return []string{"/SC", "MONTHLY", "/D", strconv.Itoa(d), "/ST", st}, nil
	}
	return nil, fmt.Errorf("combining day-of-month and day-of-week is not supported")
}

func dowToSchtasks(dow string) (string, error) {
	names := []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}
	seen := map[string]bool{}
	var out []string
	add := func(i int) {
		n := names[i%7]
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, part := range strings.Split(dow, ",") {
		if a, b, ok := strings.Cut(part, "-"); ok {
			ai, err1 := strconv.Atoi(a)
			bi, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil || ai > bi || ai < 0 || bi > 7 {
				return "", fmt.Errorf("bad day-of-week range %q", part)
			}
			for i := ai; i <= bi; i++ {
				add(i)
			}
			continue
		}
		i, err := strconv.Atoi(part)
		if err != nil || i < 0 || i > 7 {
			return "", fmt.Errorf("bad day-of-week %q", part)
		}
		add(i)
	}
	return strings.Join(out, ","), nil
}
