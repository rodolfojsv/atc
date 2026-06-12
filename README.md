# atc — agent traffic control

A terminal manager for running and supervising **multiple GitHub Copilot agent sessions in parallel** — one window, many agents, each in its own git worktree, with at-a-glance status, desktop notifications, usage tracking, and scheduled prompts.

Think of it as the control tower: agents take off, fly their missions, and hold for clearance — you watch the board and only step in when one of them is blocked.

```
┌─ atc ────────────────────────────────────────────────────────┐
│  SESSION        REPO/WORKTREE      STATUS          USAGE     │
│  api-refactor   app@wt-refactor    ● working       12.4k tok │
│  test-cleanup   app@wt-tests       ◐ WAITING ⚠     8.1k tok  │
│  deps-audit     infra@wt-audit     ✓ done          21.0k tok │
│  pr-triage      (scheduled 09:00)  ○ queued        —         │
│                                                              │
│  [enter] attach  [n]ew  [w]orktree  [a]pprove  [k]ill  [q]uit│
└──────────────────────────────────────────────────────────────┘
```

## Why

GitHub Copilot CLI has no standing multi-session manager ([copilot-cli#2966](https://github.com/github/copilot-cli/issues/2966) is open with no timeline). `/fleet` fans out subagents inside one session, but there's no control tower for independent, long-running agents across repos and worktrees.

`atc` is built on the [GitHub Copilot SDK](https://github.com/github/copilot-sdk) (Go), which drives the same agent runtime as Copilot CLI through **structured events** — streaming output, tool calls, permission requests, usage metrics — instead of scraping a terminal. Your existing Copilot login and billing apply unchanged.

Design priorities, in order:

1. **Minimal trust surface.** Single static Go binary. Dependencies: Go stdlib, [Bubble Tea](https://github.com/charmbracelet/bubbletea), and GitHub's own SDK. No npm tree, no telemetry, **no network listener** — strictly local.
2. **Never babysit a blocked agent.** Permission requests surface on the board and fire a Windows toast the moment any agent is waiting.
3. **Extensible without plugins.** Every lifecycle event can invoke a user-configured subprocess hook (PowerShell, Python, anything) with the event as JSON on stdin.

## Features

- **Session board** — live status per agent: idle / working / **waiting on permission** / done / error, with per-session token usage and context-window fill.
- **Session resume** — open sessions are recorded in `~/.atc/sessions.json`; the next `atc` run reattaches to them (the Copilot runtime persists the conversations; killed sessions are forgotten). Agents don't keep *running* while atc is closed — for that, run atc inside tmux (e.g. under WSL).
- **Attach / detach** — focus any session to watch its stream and send prompts; detach back to the board without interrupting it. Assistant replies render as **markdown** (headings, bold, code blocks — like Copilot CLI); your prompts are highlighted; tool calls and atc notices are dimmed one-liners (`⚙ bash · go test ./...`) so the analysis stays readable.
- **Worktree-per-session** — one keypress starts an agent in a fresh git worktree; cleanup on close. Parallel agents never collide in the same checkout.
- **Approval policy** — per-preset `prompt` (default) or `allow-all`, where allow-all is still gated by a deterministic deny-list (destructive commands, credential exfiltration, pipe-to-shell) checked *before* any auto-approval.
- **Hooks** — map events (`session-started`, `waiting-on-permission`, `finished`, `error`, `tool-call`) to commands in config. Built-in Windows toast notifications; sample hooks for Teams webhooks and tool-call audit logs.
- **Usage panel** — per-session input/output token totals, **billed AI Credits** (AIC column — actual nano-AIU reported by the runtime per request; there is no fixed tokens→AIC rate, it varies by model multiplier and billing batches), and a context-fill gauge, powered by the SDK's `AssistantUsage` / `SessionUsageInfo` events.
- **Scheduled prompts** — cron-style schedules that launch a session with a canned prompt and preset: nightly dependency audit, morning PR triage. Results flow through the same board, notifications, and hooks.

## Status

**Alpha — first working build.** Implemented and compiling; awaiting validation against a real Copilot seat (the Phase 0 smoke test happens on first run):

- [ ] **Phase 0** — validate: SDK hello-world against a work Copilot seat; observe permission + usage events
- [x] **Phase 1** — MVP: session board, spawn/kill, attach/detach, prompts
- [x] **Phase 2** — worktree-per-session, permission surfacing, approval policy, config, session persistence (sessions are recorded in `~/.atc/sessions.json` and resumed on the next run)
- [x] **Phase 3** — hooks, usage panel, scheduled prompts; toasts via the sample `hooks/toast.ps1` *(quota panel: not yet)*
- Later: remote-control API (deliberately deferred until the security story is settled — when it lands it will be tailnet-bound with bearer auth, never a public tunnel)

## Keys

**Board** — `↑/↓` select · `enter` attach · `n` new session · `a` review permission · `A` toggle auto-approve (⚡, deny-list still applies) · `x` abort turn · `K` kill · `q` quit

**Focus** — type + `enter` send prompt · `ctrl+j` newline · `↑/↓` prompt history · `ctrl+y`/`ctrl+n` approve/deny pending permission · `ctrl+x` abort · `pgup/pgdn` scroll · `esc` back to board

**Permission modal** — `y` approve once · `a` approve + auto-approve session · `n` deny · `esc` back

## Requirements

- A GitHub Copilot subscription (any tier — `atc` uses your existing seat and meters exactly like Copilot CLI)
- [Copilot CLI](https://github.com/github/copilot-cli) installed, on PATH, and logged in (`atc` drives it via the SDK; your stored login is reused)
- Git (for worktree management)
- Windows 10/11 (primary target — Windows Terminal recommended), Linux/macOS expected to work via Bubble Tea but untested for now

## Build

```sh
go build -o atc .        # or atc.exe on Windows
```

No install step, no runtime dependencies. Config lives at `%APPDATA%\atc\config.json` (Windows) or `~/.config/atc/config.json`.

## Configuration sketch

Note: real `config.json` must be plain JSON — the `//` comments below are illustration only.

```jsonc
{
  "worktreeRoot": "C:/dev/worktrees",
  "repos": ["C:/dev/app", "C:/dev/infra"],   // repo picker in the new-session form
  "presets": {
    "default": { "approval": "prompt" },
    "scratch": { "approval": "allow-all" }   // deny-list still applies
  },
  "hooks": {
    "waiting-on-permission": ["powershell", "-File", "hooks/toast.ps1"],
    "finished": ["powershell", "-File", "hooks/teams-webhook.ps1"]
  },
  "schedules": [
    { "name": "pr-triage", "cron": "0 9 * * 1-5", "preset": "default", "repo": "C:/dev/app",
      "worktree": false,
      "prompt": "Triage open PRs assigned to me and summarize what needs my attention." }
  ]
}
```

## Scheduled prompts

Each entry in `schedules` launches a **new session** (named after the schedule, like the `n` form had been filled in) whenever its cron expression matches. The session shows up on the board and flows through the same usage tracking, notifications, and hooks as a manual one.

- **Cron syntax** — standard 5 fields (`minute hour day-of-month month day-of-week`) supporting `*`, steps (`*/15`), ranges (`1-5`), and lists (`8,18`); day-of-week `0` and `7` are both Sunday. `0 9 * * 1-5` = weekdays 09:00 · `*/30 * * * *` = every half hour · `0 8 1 * *` = monthly.
- **atc must be running at the firing minute** — the scheduler is in-process (checked once per minute); there's no OS-level registration. A missed minute is skipped, never run late. To fire with the window closed, keep atc detached in tmux (e.g. under WSL).
- **Config is read at startup** — restart atc after editing schedules.
- **Choose the preset deliberately** — a `prompt`-mode scheduled session blocks at ⚠ WAITING on its first permission until you approve. For unattended runs use an `allow-all` preset (deny-list still applies), ideally with `"worktree": true` so it can't touch your main checkout.
- **Every firing spends Copilot credits** — sanity-check the cadence before leaving an aggressive schedule running.

## Security posture

- Local-only: `atc` opens **zero listening ports** and makes no network calls of its own; all network traffic belongs to the Copilot runtime it supervises.
- `atc` never reads, stores, or forwards credentials — authentication is handled entirely by the Copilot CLI/SDK.
- `allow-all` is per-preset and deny-list-gated, never the global default.
- Hooks run only commands you wrote into your own config.

## Acknowledgments

- [CCManager](https://github.com/kbwo/ccmanager) — prior art for the multi-agent TUI pattern; `atc` exists for the SDK-events-instead-of-PTY-scraping approach and a from-source trust story.
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) / [Lip Gloss](https://github.com/charmbracelet/lipgloss) — the TUI foundation.

## License

MIT
