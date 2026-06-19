# atc — agent traffic control

A manager for running and supervising **multiple AI coding-agent sessions in parallel** — **GitHub Copilot and Claude Code, side by side** — one window, many agents, each in its own git worktree, with at-a-glance status, cross-device notifications, usage tracking, and scheduled prompts. Drive it from the **terminal (TUI)**, the **browser**, or an **Android app**.

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

`atc` is built on the [GitHub Copilot SDK](https://github.com/github/copilot-sdk) (Go), which drives the same agent runtime as Copilot CLI through **structured events** — streaming output, tool calls, permission requests, usage metrics — instead of scraping a terminal. Your existing Copilot login and billing apply unchanged. A second backend drives **[Claude Code](https://claude.com/claude-code)** (headless, stream-JSON), so Copilot and Claude agents share the same board, the same approval flow, and the same remote clients.

Design priorities, in order:

1. **Minimal trust surface.** Single static Go binary. Core dependencies: Go stdlib, [Bubble Tea](https://github.com/charmbracelet/bubbletea), and GitHub's own SDK. No npm tree, no telemetry. **No network listener by default** — the optional web UI and phone push are opt-in and **tailnet-only**, never a public bind.
2. **Never babysit a blocked agent.** Permission requests surface on the board and fire a notification the moment any agent is waiting — a desktop toast, a browser notification, or a **phone push** that lands even with everything closed.
3. **Extensible without plugins.** Every lifecycle event can invoke a user-configured subprocess hook (PowerShell, Python, anything) with the event as JSON on stdin.

## Features

- **Session board** — live status per agent: idle / working / **waiting on permission** / done / error, with per-session token usage and context-window fill.
- **Two backends** — each session runs on **GitHub Copilot** (default, via the Copilot SDK) or **Claude Code** (via `claude` in headless stream-JSON mode); pick per session in the form or per preset (`"backend": "claude"`). Caveat: Claude's CLI has no runtime permission callback, so atc's interactive approval flow applies to Copilot sessions only — for Claude, `prompt` maps to Claude Code's `acceptEdits` permission mode and `allow-all` to `bypassPermissions`, and Claude Code's own settings.json rules still apply.
- **Custom agents (tagging)** — define agent personas once in atc's config (`agents`) and **tag** a session with one in the new-session form, a preset (`"agent": "name"`), or a schedule. atc injects them at launch — Copilot via the SDK's custom-agent config, Claude via `--agents`/`--agent` — so a tagged persona drives the session **without committing `.github/agents` or `.claude/agents` to the repo**. Built for locked-down work repos where you can't add those directories. See [Custom agents](#custom-agents).
- **Session resume & adoption** — open sessions are recorded in `~/.atc/sessions.json`; the next `atc` run reattaches to them with transcripts restored, and a running board adopts sessions finished by other atc processes (scheduled `atc run` jobs) live. Killed sessions are forgotten. Agents don't keep *running* while atc is closed — for that, run atc inside tmux (e.g. under WSL).
- **Attach / detach** — focus any session to watch its stream and send prompts; detach back to the board without interrupting it. Assistant replies render as **markdown** (headings, bold, code blocks — like Copilot CLI); your prompts are highlighted; tool calls and atc notices are dimmed one-liners (`⚙ bash · go test ./...`) so the analysis stays readable.
- **Worktree-per-session** — one keypress starts an agent in a fresh git worktree; cleanup on close. Parallel agents never collide in the same checkout.
- **Diff review & merge** — `d` shows everything a session changed (vs the commit it branched from, untracked files included); `m` commits and merges it back into the branch it came from, aborting cleanly on conflicts. Agents propose, you dispose.
- **Read-only sessions** — the Mode toggle on the form runs the agent in the backend's plan mode (Copilot `plan` agent-mode / Claude Code `--permission-mode plan`): it can inspect but structurally cannot modify. Shown as 🔒 on the board. **Scheduled tasks default to this mode** (override per schedule with `"write": true`), so an unattended prompt can't change anything on its own.
- **Obsidian/markdown export** — `e` exports a session transcript as a markdown note with YAML frontmatter (tokens, cost, repo, branch); `atc run --export` (or `"autoExport": true`) does it for scheduled runs. Point `exportDir` inside your Obsidian vault and notes land in the vault. To push them to other devices immediately, pair with `hooks/obsidian-sync.ps1`, which triggers LiveSync's replicate command via an `obsidian://` Advanced URI on every `finished` event.
- **Spend tracking** — every usage event is appended to `~/.atc/spend.jsonl`; the board footer shows today's and this month's cumulative AIC/$ across all runs, including headless ones.
- **Approval policy** — per-preset `prompt` (default) or `allow-all`, where allow-all is still gated by a deterministic deny-list (destructive commands, credential exfiltration, pipe-to-shell) checked *before* any auto-approval. The permission modal's `s` adds a session-scoped rule ("always allow `go` commands here") for anything between approve-once and full auto-approve.
- **Hooks** — map events (`session-started`, `waiting-on-permission`, `finished`, `error`, `tool-call`) to commands in config. Built-in Windows toast notifications; sample hooks for Teams webhooks and tool-call audit logs.
- **Usage panel** — per-session input/output token totals, **billed AI Credits** (AIC column — actual nano-AIU reported by the runtime per request; there is no fixed tokens→AIC rate, it varies by model multiplier and billing batches), and a context-fill gauge, powered by the SDK's `AssistantUsage` / `SessionUsageInfo` events.
- **Scheduled prompts** — cron-style schedules that launch a session with a canned prompt and preset: nightly dependency audit, morning PR triage. **Read-only by default** (plan mode — inspect, don't modify) unless a schedule sets `"write": true`. Each schedule can carry a **`precheck`**: a shell command run first, so a firing where *nothing changed* is skipped — **no session, no tokens spent**. Outcomes (updated / no-update / error) land in a per-task **run timeline** in the **Scheduled** band on the board. Results flow through the same board, notifications, and hooks. See [Scheduled prompts](#scheduled-prompts).
- **Web UI (optional)** — `atc --serve` adds a browser frontend over the *same* live sessions the TUI drives; `atc serve` runs it headless (no terminal). Everything works from a phone: the live board, streaming transcripts, prompting, and the **approve / deny / always-allow** permission flow. Plus **image attachments** (pick or paste a screenshot — inlined for Claude, saved + path-referenced otherwise), **markdown rendering** with GFM tables, **diff + merge** of a worktree session, **mid-session model switch**, **clickable file mentions** (scoped to the session dir), and an in-app link-preview modal. Localhost-bound and bearer-token protected — reach it from elsewhere via `tailscale serve` (tailnet-only HTTPS), never a public listener. See [Web UI](#web-ui).
- **Android app (optional)** — a thin [Capacitor](https://capacitorjs.com) shell that loads the web UI over your tailnet, installable from a QR in the web "App" tab. A **Servers** screen stores multiple atc instances and switches between them; hardware Back maps to in-app navigation. The app reuses the web UI verbatim, so every feature above is on your phone. Build it yourself with `scripts/build-apk.sh` (Dockerized, self-signed). See [Android app & push notifications](#android-app--push-notifications).
- **Phone push (ntfy)** — background notifications via [ntfy](https://ntfy.sh) (self-hosted or ntfy.sh): when an agent **needs approval / finishes / errors**, atc makes an outbound POST and the ntfy app alerts your phone — even with atc and the browser closed. Scoped to the sessions you started (per-device topic) with a server-wide default topic as a catch-all, and **Approve/Deny buttons right on the notification**. Outbound-only — no inbound listener, no public tunnel.

## Status

**Beta — in daily use** on real Copilot and Claude seats (Linux and Windows).

- [x] **Phase 0** — validated: SDK against a real seat; permission + usage events observed in practice
- [x] **Phase 1** — MVP: session board, spawn/kill, attach/detach, prompts
- [x] **Phase 2** — worktree-per-session, permission surfacing, approval policy, config, session persistence (sessions are recorded in `~/.atc/sessions.json` and resumed on the next run)
- [x] **Phase 3** — hooks, usage panel, scheduled prompts; desktop toasts via the sample `hooks/toast.ps1` *(quota panel: not yet)*
- [x] **Phase 4** — web UI: board, transcripts, prompting + images, approvals, markdown/tables, diff/merge, model switch, file mentions — all phone-ready over `tailscale serve`
- [x] **Phase 5** — Android app (Capacitor shell, multi-server) and **phone push via ntfy** (per-session scoping, Approve/Deny from the notification)
- Later: a formal remote-control HTTP API (the web UI already covers remote control; a dedicated API stays deferred until there's a need it doesn't meet)

## Keys

**Board** — `↑/↓`/wheel select · `enter` attach · `n` new session · `a` review permission · `d` diff (then `m` merge) · `e` export to markdown · `A` toggle auto-approve (⚡, deny-list still applies) · `x` abort turn · `K` kill · `q` quit

**Focus** — type + `enter` send prompt · `ctrl+j` newline · `↑/↓` prompt history · `ctrl+y`/`ctrl+n` approve/deny pending permission · `ctrl+x` abort · `pgup/pgdn` scroll · `esc` back to board. The active model shows bottom-right.

**Prompt box extras** — `@` fuzzy-finds a file in the session's directory **or a subagent to hand the turn to** (type to filter, `↑/↓` scroll the matches, `tab`/`enter` insert). A file inserts as `@path` and a subagent as `@agent-<name>` — the leading `@` is kept so the agent eagerly loads the file or delegates to the subagent (a bare path is just prose it may ignore). Subagent candidates are your atc-config agents plus the repo's and `~/.claude`'s `.claude/agents` (Claude sessions); `/` opens atc's command palette: `/model [name]` (show/switch model mid-session), `/diff`, `/export`, `/abort`, `/auto`, `/skills`, `/help`. In **claude sessions**, the repo's own `.claude/commands/*.md` appear in the palette too and pass through to the agent (it expands them itself; verified headless). `/skills` lists the repo's agent assets — `.claude/skills`, `.claude/commands`, instruction files — all of which the agents load as usual; skills are model-invoked when relevant. Backend CLI built-ins (`/fleet`, `/compact`) don't exist over the SDK path.

**Permission modal** — `y` approve once · `s` always allow this kind for the session · `a` approve + auto-approve session · `n` deny · `esc` back

## Requirements

- At least one backend, on PATH and logged in:
  - **[Copilot CLI](https://github.com/github/copilot-cli)** — needs a GitHub Copilot subscription (any tier); `atc` drives it via the SDK and reuses your stored login, metering exactly like Copilot CLI.
  - and/or **[Claude Code](https://claude.com/claude-code)** — for `backend: claude` sessions.
- Git (for worktree management)
- **Linux and Windows 10/11** are both used day-to-day (Windows Terminal recommended on Windows); macOS is expected to work via Bubble Tea but is less exercised.
- *Optional, for phone access:* [Tailscale](https://tailscale.com) (to reach the web UI / app over your tailnet) and an [ntfy](https://ntfy.sh) server + the ntfy app (for background push). Both are opt-in.

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
    "claude":  { "backend": "claude" },       // Claude Code sessions
    "review":  { "agent": "reviewer" }         // tag every session from this preset with the "reviewer" agent
  },
  "agents": {                                  // custom personas, injected at launch (no repo files needed)
    "reviewer": {
      "description": "Careful code reviewer",
      "prompt": "You are a meticulous senior reviewer. Point out bugs and risks; don't rewrite.",
      "model": "claude-sonnet-4-6"             // optional; tools optional too (names are backend-specific)
    }
  },
  "hooks": {
    "waiting-on-permission": ["powershell", "-File", "hooks/toast.ps1"],
    "finished": ["powershell", "-File", "hooks/teams-webhook.ps1"]
  },
  "scheduledRetentionDays": 14,                 // auto-delete finished scheduled sessions older than this (0 = keep forever)
  "schedules": [
    { "name": "pr-triage", "cron": "*/15 9-18 * * 1-5", "preset": "default", "repo": "C:/dev/app",
      "precheck": "~/scripts/check-prs.sh",        // skip (no tokens) unless something changed
      // read-only by default; add "write": true to let a task modify files
      "prompt": "Summarize what's new/unresolved on my open PRs and what needs my attention." }
  ]
}
```

## Custom agents

Both backends let you run a session as a **custom agent** — a named persona with its own system prompt (and optionally tools/model). Normally those definitions live in the repo (`.github/agents/*.md` for Copilot, `.claude/agents/*.md` for Claude), which is a problem on a repo where you **can't or shouldn't commit them** — a corporate checkout, a repo with a locked-down `.github/`, or anything you don't own.

atc sidesteps that: define agents **once in atc's own config**, then **tag** a session with one. atc injects the definition into the backend at launch, so nothing lands in the repo.

```json
{
  "agents": {
    "reviewer": {
      "description": "Careful code reviewer",
      "prompt": "You are a meticulous senior reviewer. Flag bugs and risks; don't rewrite code.",
      "tools": ["Read", "Grep", "Bash"],     // optional — omit for all tools (names are backend-specific)
      "model": "claude-sonnet-4-6"            // optional — overrides the session model for this agent
    },
    "scribe": { "prompt": "You write and tidy docs. Never touch code." }
  }
}
```

Three ways to tag a session with one:

- **New-session form** — an **Agent** picker appears (TUI and web) once any agents are configured; `(none)` leaves the backend's default agent in charge.
- **Preset** — `"agent": "reviewer"` on a preset pins it for every session started from that preset.
- **Schedule** — `"agent": "reviewer"` on a schedule entry, for unattended runs.

How it maps under the hood:

- **Copilot** — all configured agents are passed as the SDK's custom agents (so they're also available for delegation), and the tagged one is activated as the session's agent.
- **Claude** — atc passes the agents inline via `--agents '<json>'` and activates the tagged one with `--agent <name>`. These are session-only; nothing is written to `~/.claude` or the repo.

Notes:

- **Tool names are backend-specific.** A `tools` list written for Claude (`Read`, `Bash`, …) won't match Copilot's tool names. Omit `tools` (the default = all tools) for an agent you want to use on either backend, or keep backend-specific agents.
- A tagged agent that no longer exists in config is silently ignored (the session runs with the default agent) rather than failing to launch.
- This is independent of any `.github/agents` / `.claude/agents` the repo *does* have — those still load as usual; tagging just adds atc-managed personas on top.

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
- **Scheduled tasks are read-only by default** (the backend's plan mode — inspect, summarize, advise, but structurally can't modify), so a look-don't-touch job like PR/Jira triage needs nothing extra. For a task that *should* change files, set `"write": true` on the schedule and pair it with an `allow-all` preset (deny-list still applies), ideally with `"worktree": true` so it can't touch your main checkout.
- **Headless/scheduled sessions can't answer permission prompts.** In `atc run`, anything the preset doesn't pre-approve is denied with an explanatory message; in-process scheduled sessions block at ⚠ WAITING instead. Either way the run still lands as a session you can continue interactively to apply anything it suggested.
- **Un-gated firings spend credits** — but a `precheck` makes the quiet ones free (see below). Still sanity-check the cadence before leaving an aggressive schedule running.

### Precondition gates & token savings

Scheduled prompts get expensive when every firing wakes the agent — yet most of the time *nothing has changed*. A **`precheck`** fixes that: a shell command run **before** the prompt, in the schedule's `repo` directory (`sh -c` on Linux/macOS, `cmd /c` on Windows).

- **exit 0** → something changed → run the prompt as usual.
- **non-zero exit** → nothing new → **skip**: no session is created and **no tokens are spent**.
- **fails to start** (missing script, bad dir, >60s) → recorded as an `error` instead of a silent skip.

Every firing's outcome — `updated` / `no-update` / `error` — is appended to `~/.atc/schedule-runs.jsonl` and shown as a per-task **run timeline** in the **Scheduled** band of the board (web, and the TUI overlay via `s`). So you can see "checked at 12:15, no changes" without it ever costing a token; `updated` rows deep-link to the session that ran.

**Scheduled sessions stay out of the main board.** A schedule's sessions surface on the board only while they're *running* or *need your input* — once a run finishes (done/error) it drops off the board and lives in the **Scheduled** section instead, openable from its schedule's run timeline (TUI: press `s`, select with ↑↓, `enter` to open; web: tap the `↗ session` link). This keeps a recurring task from burying your interactive work. To stop finished runs piling up indefinitely, set **`scheduledRetentionDays`** in config: atc auto-deletes finished scheduled sessions (and their worktrees) older than that — on a timer while it's open, and once after each headless `atc run`, so cron-driven schedules self-clean even with no UI running. `0` (the default) keeps them forever. Manually started sessions are never auto-removed. (Repeated runs of the same schedule are named with a timestamp suffix — `pr-triage-0618-1430` — rather than `pr-triage-2`, so the Scheduled list reads chronologically.)

A minimal precheck is just *fetch a cheap signal → compare to stored state → exit 0/1*:

```bash
#!/usr/bin/env bash
# exit 0 = changed (run the prompt), exit 1 = nothing new (skip)
set -euo pipefail
state="$HOME/.local/state/atc/pr-triage.hash"; mkdir -p "$(dirname "$state")"
sig=$(gh pr list --author '@me' --state open --json number,reviewDecision,updatedAt | jq -S .)
new=$(printf '%s' "$sig" | sha256sum | cut -d' ' -f1)
[ "$new" = "$(cat "$state" 2>/dev/null || true)" ] && exit 1
printf '%s' "$new" > "$state"; exit 0
```

The savings compound with **incremental context**: a good precheck doesn't just gate, it writes a *delta* (only what changed since last run) for the prompt to read — so even when the agent does run, it looks at the new comment, not the whole PR. Two patterns this is built for:

- **PR review triage** — list *unresolved* review threads (via GitHub GraphQL, since REST can't see resolution) across your open PRs, flag ones you've likely already fixed, and skip entirely when nothing's new. Over a quiet day that's dozens of free no-op checks instead of dozens of full agent runs.
- **Jira issue activity** — pull new comments / status changes on an issue (or JQL) and gate the same way.

> Because the delta is text other people wrote, treat it as **untrusted** in the prompt (summarize it, never follow instructions inside it) — which is exactly why scheduled tasks are read-only by default.

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
       "prompt": "Review new comments on my open PRs. Summarize what was said and suggest how I should respond or what to change." }
   ]
   ```

   > Read-only is the default, so a pure triage like this needs no `allow-all` preset (use `default`); `unattended` only matters once you set `"write": true`. Add a `precheck` (see [Precondition gates & token savings](#precondition-gates--token-savings)) to make quiet runs free.

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

## Web UI

A browser frontend over the same supervisor the TUI uses — built for two things the terminal can't do well: driving sessions from your phone, and **attaching images to a prompt**.

Two ways to run it:

```
atc --serve          # the TUI, plus the web server alongside it
atc serve            # headless: web only, no terminal (laptop-lid-closed case)
```

Both print a tokenized URL at startup, e.g. `http://127.0.0.1:8787/?token=ab12…`. Open that once and the browser remembers the token. `atc serve` runs the full machinery — sessions resume, schedules fire, hooks run, and permission requests wait for an answer from the browser, exactly like the TUI.

**Sharing sessions between the terminal and the browser.** This comes down to *one process or two*:

- **`atc --serve`** — one process runs the TUI **and** the web server against the same in-memory supervisor. The terminal and the browser show the **same live sessions in real time** (same transcripts streaming, same permission prompts). This is what you want if you're at the machine and also want the phone view. It needs a terminal (the TUI renders there); the web URL is served alongside.
- **`atc serve` + a separate `atc`** — these are two independent processes. A live agent session is owned by the process that spawned it, so they do **not** stream each other's in-flight sessions. They share only **settled** sessions (done/error) through `~/.atc/sessions.json`: a session finished in one shows up in the other within a few seconds (the same `WatchStore` adoption used for scheduled `atc run` jobs), transcript restored, ready to continue. So a web-created session appears in a later-launched TUI once it settles, and vice versa — just not mid-turn.

Rule of thumb: want both views *live* → run `atc --serve`. Running them as separate processes (e.g. a headless `atc serve` plus an occasional TUI) is fine too; you just get settled-session hand-off rather than live mirroring.

**What you can do from the browser:** see the live board (status, model, cost, pending-permission badges, pin/category grouping), open a session and watch its **markdown-rendered** transcript stream (GFM tables, clickable `.md` file mentions, link-preview modal), send prompts, **approve/deny/always-allow permission requests**, toggle auto-approve, **review the diff and merge** a worktree session, **switch the model mid-session**, abort, kill, rename, and launch new sessions (repo/backend/model/worktree/read-only, or a no-repo *scratch* session). The **📱 App** tab hosts the APK download, device pairing, and ntfy subscribe — see [Android app & push notifications](#android-app--push-notifications).

**Images.** Click 📎 to pick files, or just **paste a screenshot** into the prompt box. For **Claude** sessions the image is inlined into the model's context as a base64 content block — no file written. For backends that can't take inline images (and for non-image files anywhere), the file is saved under `<session dir>/.atc-attachments/` and its path is appended to the prompt so the agent reads it with its file tools. Limits: 6 files/prompt, 10 MB each.

**Reaching it from your phone — tailnet only.** Leave atc bound to localhost and put Tailscale in front:

```
tailscale serve --bg 8787      # tailnet-only HTTPS with a real cert
tailscale serve off            # when you're done for the session
```

This exposes nothing on your LAN and opens no public listener — only devices on your tailnet (your phone, signed into the same account) can reach it, over HTTPS. **Do not** bind atc itself to `0.0.0.0` to "make it reachable"; that defeats the token-over-localhost model. Since this serves work-repo transcripts over your tailnet, treat turning it on like the Teams two-way idea — worth a nod to IT first.

Config (all optional):

```json
{
  "web": {
    "addr": "127.0.0.1:8787",   // listen address; keep it on localhost
    "token": "pick-your-own"    // omit for a fresh random token each run
  }
}
```

Flags override config: `atc serve --addr 127.0.0.1:9000 --token mytoken`. A stable `token` in config keeps the URL constant so a phone bookmark keeps working across restarts; omit it and a new token is minted (and printed) each run.

## Android app & push notifications

Two opt-in pieces turn the phone view into a real app with background alerts. Both are **tailnet-only and outbound-only** — atc never opens an inbound port for them.

### The "App" tab

When the web UI is up, the header's **📱 App** button opens a panel with:
- **Android app** — version, size, SHA-256, a **Download APK** button, a **download QR** to scan from the phone, and a **copy app link** button (the URL you paste into the app on first launch). Appears once you point `web.apkPath` at a built APK.
- **Phone notifications (ntfy)** — a **subscribe QR** + topic URL and a **send-test** button (appears when ntfy is enabled).

The APK download and the QR endpoint sit behind the same bearer token as the rest of the UI; the QR is rendered server-side (tiny `rsc.io/qr`, no client-side JS deps).

### The Android app

The app is a thin Capacitor shell that loads the live web UI over your tailnet — so it's always in sync with the server and needs no rebuild when atc updates. On first launch you paste your atc link (`https://<host>.ts.net/?token=…`); a **Servers** screen stores it and any others, so you can keep several atc instances and switch between them (one visible at a time). Hardware **Back** maps to in-app navigation (session → board → Servers). Background notifications come from ntfy, below.

**Build it** (needs Docker; produces a self-signed `build/atc.apk`):

```sh
ATC_KEYSTORE_PASSWORD=your-pass ./scripts/build-apk.sh
```

First run builds a ~2 GB toolchain image (Node + JDK + Android SDK) once; later builds take a couple of minutes. **Back up the keystore and its password** (`~/.android/atc-release.keystore`) — the same pair is required to ship updates. Then set `web.apkPath` to the APK and `web.apkVersion` to a label, and restart atc; the App tab serves it.

### Push via ntfy

atc publishes session events to an [ntfy](https://ntfy.sh) topic; the **ntfy phone app** (not the website) delivers them as real push notifications, even with atc and the browser fully closed.

```jsonc
{
  "ntfy": {
    "enabled": true,
    "server":  "http://127.0.0.1:2586",                 // where atc POSTs (self-hosted = localhost)
    "subscribeUrl": "https://myhost.tailnet.ts.net:8443", // what the phone subscribes to (shown in the App tab)
    "topic":   "atc-<something-unguessable>",            // stable default topic; catches every session
    "serverName": "myhost",                               // labels the notification title
    "publicUrl":  "https://myhost.tailnet.ts.net",        // for tap-to-open deep links + action buttons
    "actions": true                                        // Approve/Deny buttons (self-hosted only — see below)
  }
}
```

- **Which events:** `waiting-on-permission`, `finished`, `error`. Title is `<serverName> · <session> <event>`; tapping deep-links to the session.
- **Scoping:** each device gets its own topic (sessions you start notify *you*); the configured `topic` is a server-wide catch-all so a single subscribed phone reliably gets everything. A new-session **"Notify me"** checkbox (default on) can mute a session.
- **Approve/Deny from the notification:** with `actions: true`, permission alerts carry buttons that POST back to atc. They embed the atc token in the message, so **only enable `actions` with a self-hosted ntfy you trust** — never on ntfy.sh.
- **Self-hosted ntfy** is the on-ethos choice (single Go binary, no Google): run `ntfy serve`, expose it with `tailscale serve --https=8443 http://127.0.0.1:2586`, and in the phone's ntfy app **subscribe on your server** (not the default ntfy.sh) with **Instant Delivery** enabled — required for background push without Firebase. Or point `server` at `https://ntfy.sh` for battery-friendly FCM delivery at the cost of routing through Google.

## Diagnostics

atc writes no logs by default. When something misbehaves, enable the diagnostic log:

```json
{ "logLevel": "info", "logFile": "D:/logs/atc.log" }
```

or one-off: `atc --debug` / `atc run --debug`. Levels: `info` (session, permission, store, and scheduler lifecycle) or `debug` (additionally one line per backend event — a frozen session shows exactly when events stopped). `logFile` defaults to `~/.atc/atc.log`; the file is JSONL (grep-able), rotates at 5 MB (previous kept as `.old`), and records **metadata only — never prompts or transcript content**. At `debug` level the Copilot runtime's own diagnostics are also enabled (they land in the Copilot CLI's log location). Turn it back off (`"logLevel": "off"` or remove the key) once things are stable.

Permission lifecycle entries are the most useful: every request logs `permission.enqueued` (with queue depth) and `permission.answered` (with the decision and *who* decided — user, session-rule, allow-all, deny-list, or headless).

## Security posture

- Local-only by default: without `--serve`/`serve`, `atc` opens **zero listening ports** and makes no network calls of its own; all network traffic belongs to the agent runtime it supervises. The web UI is opt-in, binds to localhost, and is bearer-token gated — expose it across machines only via `tailscale serve` (tailnet-only), never a public bind.
- Push notifications (ntfy), when enabled, are **outbound POSTs only** — they add no inbound listener. Self-host ntfy on the tailnet to keep everything local; the notification body carries no secret unless you opt into `actions` (Approve/Deny buttons), which is why those are for trusted self-hosted servers only.
- `atc` never reads, stores, or forwards credentials — authentication is handled entirely by the Copilot CLI/SDK.
- `allow-all` is per-preset and deny-list-gated, never the global default.
- Hooks run only commands you wrote into your own config.

## Acknowledgments

- [CCManager](https://github.com/kbwo/ccmanager) — prior art for the multi-agent TUI pattern; `atc` exists for the SDK-events-instead-of-PTY-scraping approach and a from-source trust story.
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) / [Lip Gloss](https://github.com/charmbracelet/lipgloss) — the TUI foundation.
- [Capacitor](https://capacitorjs.com) (Android shell), [ntfy](https://ntfy.sh) (push), and [Tailscale](https://tailscale.com) (tailnet access) — the phone story.

## License

MIT
