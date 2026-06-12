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
- **Two backends** — each session runs on **GitHub Copilot** (default, via the Copilot SDK) or **Claude Code** (via `claude` in headless stream-JSON mode); pick per session in the form or per preset (`"backend": "claude"`). Caveat: Claude's CLI has no runtime permission callback, so atc's interactive approval flow applies to Copilot sessions only — for Claude, `prompt` maps to Claude Code's `acceptEdits` permission mode and `allow-all` to `bypassPermissions`, and Claude Code's own settings.json rules still apply.
- **Session resume & adoption** — open sessions are recorded in `~/.atc/sessions.json`; the next `atc` run reattaches to them with transcripts restored, and a running board adopts sessions finished by other atc processes (scheduled `atc run` jobs) live. Killed sessions are forgotten. Agents don't keep *running* while atc is closed — for that, run atc inside tmux (e.g. under WSL).
- **Attach / detach** — focus any session to watch its stream and send prompts; detach back to the board without interrupting it. Assistant replies render as **markdown** (headings, bold, code blocks — like Copilot CLI); your prompts are highlighted; tool calls and atc notices are dimmed one-liners (`⚙ bash · go test ./...`) so the analysis stays readable.
- **Worktree-per-session** — one keypress starts an agent in a fresh git worktree; cleanup on close. Parallel agents never collide in the same checkout.
- **Diff review & merge** — `d` shows everything a session changed (vs the commit it branched from, untracked files included); `m` commits and merges it back into the branch it came from, aborting cleanly on conflicts. Agents propose, you dispose.
- **Read-only sessions** — the Mode toggle on the form runs the agent in the backend's plan mode (Copilot `plan` agent-mode / Claude Code `--permission-mode plan`): it can inspect but structurally cannot modify. Shown as 🔒 on the board. Ideal for triage/review schedules.
- **Obsidian/markdown export** — `e` exports a session transcript as a markdown note with YAML frontmatter (tokens, cost, repo, branch); `atc run --export` (or `"autoExport": true`) does it for scheduled runs. Point `exportDir` inside your Obsidian vault and notes land in the vault. To push them to other devices immediately, pair with `hooks/obsidian-sync.ps1`, which triggers LiveSync's replicate command via an `obsidian://` Advanced URI on every `finished` event.
- **Spend tracking** — every usage event is appended to `~/.atc/spend.jsonl`; the board footer shows today's and this month's cumulative AIC/$ across all runs, including headless ones.
- **Approval policy** — per-preset `prompt` (default) or `allow-all`, where allow-all is still gated by a deterministic deny-list (destructive commands, credential exfiltration, pipe-to-shell) checked *before* any auto-approval. The permission modal's `s` adds a session-scoped rule ("always allow `go` commands here") for anything between approve-once and full auto-approve.
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

**Board** — `↑/↓`/wheel select · `enter` attach · `n` new session · `a` review permission · `d` diff (then `m` merge) · `e` export to markdown · `A` toggle auto-approve (⚡, deny-list still applies) · `x` abort turn · `K` kill · `q` quit

**Focus** — type + `enter` send prompt · `ctrl+j` newline · `↑/↓` prompt history · `ctrl+y`/`ctrl+n` approve/deny pending permission · `ctrl+x` abort · `pgup/pgdn` scroll · `esc` back to board

**Permission modal** — `y` approve once · `s` always allow this kind for the session · `a` approve + auto-approve session · `n` deny · `esc` back

## Requirements

- A GitHub Copilot subscription (any tier — `atc` uses your existing seat and meters exactly like Copilot CLI)
- [Copilot CLI](https://github.com/github/copilot-cli) installed, on PATH, and logged in (`atc` drives it via the SDK; your stored login is reused)
- Optional: [Claude Code](https://claude.com/claude-code) installed and logged in, for `backend: claude` sessions
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
  "exportDir": "C:/Users/me/Vault/atc",      // inside your Obsidian vault → exports land in the vault
  "autoExport": true,                         // atc run exports every completed session
  "repos": ["C:/dev/app", "C:/dev/infra"],   // repo picker in the new-session form
  "presets": {
    "default": { "approval": "prompt" },
    "scratch": { "approval": "allow-all" },  // deny-list still applies
    "claude":  { "backend": "claude" }        // Claude Code sessions
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

Each entry in `schedules` launches a **new session** with that prompt. There are two engines for the timing — pick per your setup:

**1. Windows Task Scheduler (recommended on Windows — fires even when atc is closed):**

```powershell
atc schedule install     # registers each named schedule as task atc\<name>
atc schedule list        # shows the cron → schtasks translation
atc schedule uninstall   # removes the tasks
```

Each task runs `atc run --schedule <name>` — a **one-shot headless session**: the transcript streams to stdout, your hooks fire (toasts work — tasks run in your desktop session), and the session is recorded in `~/.atc/sessions.json`. **A running atc board adopts it live within seconds of it finishing; otherwise it appears on the next atc start — either way with the full transcript, ready to continue interactively.** Repo/prompt/preset are read from config at fire time, so editing a schedule never requires re-registering. Translatable crons: daily (`0 9 * * *`), weekly (`0 9 * * 1-5`), monthly (`0 8 1 * *`), every-N-minutes (`*/30 * * * *`).

`atc run` also works standalone: `atc run --repo C:/dev/app --preset unattended --worktree --prompt "..."`. Exit codes: 0 done, 1 error, 2 timeout (`--timeout`, default 60m).

**2. In-process scheduler (atc must be open at the firing minute):** the same `schedules` entries fire inside a running atc — useful when atc lives in a tmux session anyway (e.g. under WSL). Checked once per minute; missed minutes are skipped; config is read at startup.

Notes for both:
- **Headless/scheduled sessions can't answer permission prompts.** In `atc run`, anything the preset doesn't pre-approve is denied with an explanatory message; in-process scheduled sessions block at ⚠ WAITING instead. For unattended work use an `allow-all` preset (deny-list still applies), ideally with `"worktree": true` so it can't touch your main checkout. For look-don't-touch jobs (PR triage that only reports), say so in the prompt — and the run still lands as a session you can continue interactively to apply anything it suggested.
- **Every firing spends credits** — sanity-check the cadence before leaving an aggressive schedule running.

### Setting up your first scheduled task (step by step)

1. **Add a preset for unattended runs** to `%APPDATA%\atc\config.json` (plain JSON, no comments):

   ```json
   "presets": {
     "default":    { "approval": "prompt" },
     "unattended": { "approval": "allow-all" }
   }
   ```

2. **Add the schedule.** `name` is required (it becomes the task name) and the prompt should state the ground rules:

   ```json
   "schedules": [
     { "name": "pr-triage", "cron": "0 9 * * 1-5", "preset": "unattended",
       "repo": "C:/dev/app", "worktree": false,
       "prompt": "Review new comments on my open PRs. Summarize what was said and suggest how I should respond or what to change. Do NOT modify any files." }
   ]
   ```

3. **Preview the translation** — confirms the cron maps onto Task Scheduler terms:

   ```powershell
   atc schedule list
   # pr-triage   0 9 * * 1-5  → /SC WEEKLY /D MON,TUE,WED,THU,FRI /ST 09:00  C:/dev/app
   ```

4. **Dry-run it once by hand** before trusting the schedule — same code path the task will use:

   ```powershell
   atc run --schedule pr-triage
   ```

   Read the output: did it finish (`— done`), did anything get denied that the job actually needed (switch the preset if so), does the report look right?

5. **Register it:**

   ```powershell
   atc schedule install
   ```

   Verify with `schtasks /Query /TN atc\pr-triage` (or Task Scheduler's UI, folder `atc`). To test the full pipeline without waiting for 09:00: `schtasks /Run /TN atc\pr-triage`.

6. **That's it.** Each weekday at 09:00 the run fires, your hooks fire (add a `finished` toast so you notice), and the session lands on the board — adopted live if atc is open, on next start otherwise — ready to attach and continue.

Maintenance: editing a schedule's **prompt/repo/preset** needs nothing (config is read at fire time); changing its **cron** or **name** needs `atc schedule install` again (it overwrites); removing one from config needs `atc schedule uninstall` *before* you delete it (or `schtasks /Delete /TN atc\<name>` after). If you move `atc.exe`, re-run `install` — the task records the absolute path.

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
