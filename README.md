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
- **Attach / detach** — focus any session to watch its stream and send prompts; detach back to the board without interrupting it.
- **Worktree-per-session** — one keypress starts an agent in a fresh git worktree; cleanup on close. Parallel agents never collide in the same checkout.
- **Approval policy** — per-preset `prompt` (default) or `allow-all`, where allow-all is still gated by a deterministic deny-list (destructive commands, credential exfiltration, pipe-to-shell) checked *before* any auto-approval.
- **Hooks** — map events (`session-started`, `waiting-on-permission`, `finished`, `error`, `tool-call`) to commands in config. Built-in Windows toast notifications; sample hooks for Teams webhooks and tool-call audit logs.
- **Usage panel** — per-session input/output token totals and cost (Copilot AI Credits era), context-fill gauge, powered by the SDK's `AssistantUsage` / `SessionUsageInfo` events.
- **Scheduled prompts** — cron-style schedules that launch a session with a canned prompt and preset: nightly dependency audit, morning PR triage. Results flow through the same board, notifications, and hooks.

## Status

**Alpha — first working build.** Implemented and compiling; awaiting validation against a real Copilot seat (the Phase 0 smoke test happens on first run):

- [ ] **Phase 0** — validate: SDK hello-world against a work Copilot seat; observe permission + usage events
- [x] **Phase 1** — MVP: session board, spawn/kill, attach/detach, prompts
- [x] **Phase 2** — worktree-per-session, permission surfacing, approval policy, config *(session persistence across restarts: not yet)*
- [x] **Phase 3** — hooks, usage panel, scheduled prompts; toasts via the sample `hooks/toast.ps1` *(quota panel: not yet)*
- Later: remote-control API (deliberately deferred until the security story is settled — when it lands it will be tailnet-bound with bearer auth, never a public tunnel)

## Keys

**Board** — `↑/↓` select · `enter` attach · `n` new session · `a` review permission · `A` toggle auto-approve (⚡, deny-list still applies) · `x` abort turn · `K` kill · `q` quit

**Focus** — type + `enter` send prompt · `ctrl+y`/`ctrl+n` approve/deny pending permission · `ctrl+x` abort · `pgup/pgdn` scroll · `esc` back to board

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

```jsonc
{
  "worktreeRoot": "C:/dev/worktrees",
  "presets": {
    "default": { "approval": "prompt" },
    "scratch": { "approval": "allow-all" }   // deny-list still applies
  },
  "hooks": {
    "waiting-on-permission": ["powershell", "-File", "hooks/toast.ps1"],
    "finished": ["powershell", "-File", "hooks/teams-webhook.ps1"]
  },
  "schedules": [
    { "cron": "0 9 * * 1-5", "preset": "default", "repo": "C:/dev/app",
      "prompt": "Triage open PRs assigned to me and summarize what needs my attention." }
  ]
}
```

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
