# Handoff: migrating atc's Claude backend from `-p` to interactive tmux

**Branch:** `claude-code-tmux` (off `origin/main` @ `bf16c89`)
**Commits:** `f2e8e7b` (tmux client) → `1befa03` (tmux backend, drops `-p`) → `f297d46` (permission/question prompt handling)
**Status:** Compiles, `go vet` clean, `gofmt` clean, full unit suite passes. **Not yet run live** against `claude` (needs a logged-in subscription + tmux). Live behavior — especially TUI scraping — is unverified.

This document is a self-contained brief so a fresh Claude Code session (with no memory of the original design conversation) can continue debugging.

---

## Why this migration exists

As of the **June 2026 Anthropic billing change**, programmatic Claude surfaces — `claude -p` (headless stream-JSON), the Agent SDK, and ACP — no longer draw from the Claude Pro/Max **subscription**. They draw from a separate, capped **agent-credit pool** ($20 Pro / $100 Max5x / $200 Max20x) billed at API rates. Only **Claude Code run interactively in a real terminal** still bills against the subscription.

atc previously drove `claude --print --output-format stream-json` (headless). To keep usage on the subscription, the Claude backend now drives the **real interactive `claude` TUI inside a detached tmux session**. tmux is used (not a bare PTY) because: (a) the tmux server owns a genuine PTY, so claude bills as interactive; and (b) the tmux server is a daemon, so the conversation survives an atc crash/restart — which also fixes a prior bare-PTY problem where the child died when the controlling process hiccupped.

---

## Architecture

```
atc supervisor ──(agent.Session interface, unchanged)──> claudeagent.session
                                                              │
   input: tmux send-keys (prompt+Enter, /model, Escape) ──────┤
                                                              ▼
                                                        tmux server (daemon)
                                                              │ owns real PTY
                                                              ▼
                                                          claude (interactive TUI)
                                                              │ writes
                                                              ▼
                                              ~/.claude/projects/<dir>/<id>.jsonl
   output: atc TAILS that jsonl ─────────────────────────────┘  (reuses the proven parser)
   turn-end + permission/question boxes: read via tmux capture-pane
```

**Transport split (the key design decision):**
- **Input** goes in via `tmux send-keys`.
- **Output content** (assistant text, tool calls, token usage, cost) is read by **tailing Claude's JSONL transcript** and parsing it with the same code `History()` uses — NOT by scraping the TUI. This is robust and version-independent.
- **`tmux capture-pane`** is used only for things the file can't tell us: detecting **turn-end (idle)** and detecting/answering **permission + AskUserQuestion prompts**.
- **`pane_current_command`** detects a claude that died inside a still-alive tmux session.

**Resume / durability (two layers):**
- **Layer 1 — atc restarts, claude still alive in tmux:** `ResumeSession` → first `Send` sees the tmux session exists → continues; no claude relaunch. History is replayed from the jsonl by the supervisor.
- **Layer 2 — claude died inside tmux (pane dropped to a shell):** `ensureLaunched` / the watch loop detect a shell via `pane_current_command` and relaunch `claude --resume <claudeID>` into the same session.

---

## Files

| File | What it is |
|---|---|
| `internal/tmux/tmux.go` | Generic tmux client: `New`, `NewSession` (detached, `-x/-y`, `-c`, `-e`), `HasSession`, `KillSession`, `SendText`/`SendKeys`/`SendEnter`, `Capture`, `SetOption`, `PaneCommand`. App-neutral. |
| `internal/tmux/tmux_test.go` | Integration tests against real tmux (skip if tmux absent). |
| `internal/agent/claudeagent/claude.go` | The tmux-driven `agent.Session`: lifecycle, JSONL tailing → events, turn-end detection, session-id handling, History. |
| `internal/agent/claudeagent/prompt.go` | Detect + answer TUI permission boxes and AskUserQuestion pickers; route through `OnPermission`/`OnQuestion`. |
| `internal/agent/claudeagent/claude_test.go`, `prompt_test.go` | Unit tests for the parser, history, drain-from-offset, prompt detection, option matching, plus an opt-in live smoke test. |

The Claude backend is constructed at `internal/supervisor/supervisor.go` (`"claude": claudeagent.New()`). The `agent.Session` interface is unchanged, so `atc --serve` uses the new backend with no wiring changes.

---

## Session identity, history, and "do I lose old conversations?"

- atc's session id (`ID()`) is a UUID, used as `claude --session-id <id>` for new sessions and as the **tmux session name** `atc-<id>`.
- The on-disk transcript is `~/.claude/projects/<cwd-with-non-alphanumerics-dashed>/<claudeID>.jsonl`. `claudeID` defaults to the atc id (so the jsonl filename == the id). If `--session-id` turns out **not** to be honored in interactive mode, `discoverClaudeID()` adopts the newest jsonl created right after launch.
- **Old `-p` conversations are preserved**: same jsonl files, same ids; `History()` reads them unchanged, and `claude --resume <id>` continues them. Nothing is deleted by switching backends. You only lose visibility if atc's own store (`~/.atc`) is wiped, or you move to a machine without these files.

---

## Tunables (where to look first when live behavior is wrong)

All version-specific TUI assumptions are centralized:

1. **Turn-end detection** — `workingMarkers` in `claude.go`. Substrings claude shows while a turn is in progress (currently `"esc to interrupt"` etc.). Symptoms if wrong: turns hang in "working" forever, or finish too early. Also `pollInterval` / `quiescence` constants.
2. **Prompt detection** — `prompt.go` Tunables block: `promptOptionRe` (option line regex), `cursorGlyphs` (highlight markers `❯►`), `permissionTitleMarkers` / `permissionOptionMarkers` (permission-vs-question classification).
3. **Option selection** — `selectIndex()` in `prompt.go`. Currently uses **arrow navigation** (`Down`×n then `Enter`). If this Claude Code version selects by **number key** instead, change it to send the digit. THIS IS THE MOST LIKELY THING TO NEED CHANGING.
4. **Session-id assumption** — `claudeArgs()` passes `--session-id` (new) / `--resume` (continue). If interactive claude rejects `--session-id`, rely on `discoverClaudeID()`.
5. **Geometry** — `paneWidth`/`paneHeight` (fixed so scraping is stable).

---

## Deferred / known limitations (intentional, with graceful fallbacks)

- **Inline image attachments**: dropped (stream-JSON-only). The supervisor falls back to saving attachments to disk and referencing them by path. Works.
- **Init-event slash-command list** (`CommandLister`): dropped (no init event in interactive mode). The TUI falls back to its own filesystem scan. `/`-completion is reduced but functional.
- **Per-token streaming deltas**: we emit per-message (from the jsonl), not per-token. Cosmetic — no live "typing" effect.
- **Runtime permission prompts under `acceptEdits`**: now handled (see prompt.go), but selection keys are unverified — see Tunable #3.

---

## How to build, run, and test

```bash
# build
go build -o atc .

# unit tests (no claude/tmux needed for most; tmux tests skip without tmux)
go test ./...

# opt-in live end-to-end (needs tmux + logged-in claude; spends a little subscription usage)
ATC_CLAUDE_SMOKE=1 go test ./internal/agent/claudeagent/ -run TestLiveSmoke -v

# run the server
./atc --serve
```

**Inspect a live session (primary debugging tool):**
```bash
tmux ls                                  # find atc-<uuid>
tmux attach -t atc-<uuid>                # watch the real claude TUI (Ctrl-b d to detach)
tmux capture-pane -t atc-<uuid> -p       # exact text atc sees — paste this when debugging
```
Confirm billing: run `/usage` inside the claude session; it should count against the subscription, not the agent-credit pool.

---

## Open debug checklist (do these live, in order)

1. **Billing**: start a session, run a couple of turns, `/usage` → confirm subscription, not credit pool. (The whole point.)
2. **Turn-end**: does a turn reliably transition to idle (atc shows the session done)? If not, capture-pane the busy and idle states and adjust `workingMarkers`.
3. **`--session-id` honored?**: after first prompt, check `~/.claude/projects/<dir>/` for `<atc-id>.jsonl`. If the file is named something else, `discoverClaudeID()` should have adopted it — verify history still shows.
4. **Permission prompt**: trigger a tool needing approval; capture-pane the box; verify atc surfaces an approval modal and the chosen option is selected. Tune `prompt.go` markers + `selectIndex` (arrow vs digit).
5. **AskUserQuestion**: trigger one; verify the picker is detected and answered.
6. **Resume layer 1**: restart atc with a live tmux session; verify the conversation reattaches.
7. **Resume layer 2**: `tmux send-keys -t atc-<id> C-c` to kill claude inside tmux; send a new prompt; verify `claude --resume` recovers.
8. **Old `-p` session continuation**: open a pre-migration session and send a prompt; verify it resumes.

When something misbehaves, the most useful artifact to capture is the **`tmux capture-pane -t atc-<id> -p`** output of the exact screen, plus the relevant atc log lines.

---

## Local git note

A pre-existing, unrelated WIP (the `scheduled-task-prechecks` work) was stashed to branch cleanly from main: `git stash list` shows `stash@{0}: On feature/claude-pty-tmux: precheck WIP`. It is **not** part of this migration. (That precheck work is also already merged into `main` via PR #21, so the stash may be redundant — review before popping.)
