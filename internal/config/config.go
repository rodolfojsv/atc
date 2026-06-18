// Package config loads atc's JSON configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Approval modes for a preset. Allow-all is still gated by the
// deterministic deny-list in internal/policy.
const (
	ApprovalPrompt   = "prompt"
	ApprovalAllowAll = "allow-all"
)

// Preset is a reusable bundle of session settings referenced by name.
type Preset struct {
	Approval string `json:"approval,omitempty"`
	Model    string `json:"model,omitempty"`
	Backend  string `json:"backend,omitempty"` // "copilot" (default) or "claude"
}

// Schedule launches a session with a canned prompt on a cron expression
// (standard 5 fields: minute hour day-of-month month day-of-week).
type Schedule struct {
	Name     string `json:"name,omitempty"`
	Cron     string `json:"cron"`
	Preset   string `json:"preset,omitempty"`
	Repo     string `json:"repo"`
	Worktree bool   `json:"worktree,omitempty"`
	Prompt   string `json:"prompt"`
	// Write opts a scheduled task out of read-only mode. Scheduled tasks
	// run in the backend's plan/read-only mode by default — they inspect,
	// summarize, and advise but never modify files or run mutating tools —
	// so an unattended prompt can't change anything on its own. Set
	// write:true only for tasks you intend to make changes (and pair it
	// with an allow-all preset for unattended approval).
	Write bool `json:"write,omitempty"`
	// Precheck is an optional shell command run in Repo before each fire.
	// Exit 0 means "something changed, run the prompt"; a non-zero exit
	// means "nothing new, skip" — no session is created and no tokens are
	// spent. A command that fails to start (missing script, bad dir) is
	// recorded as an error rather than a silent skip. The skip/run/error
	// outcome of every fire is appended to the schedule run log so the UI
	// can show "no updates since X". Empty disables gating (always run).
	Precheck string `json:"precheck,omitempty"`
}

// Web configures the optional local web UI (atc serve / atc --serve).
// It binds to localhost by default; expose it on a tailnet with
// `tailscale serve` rather than binding to a network interface.
type Web struct {
	// Addr is the listen address (default "127.0.0.1:8787").
	Addr string `json:"addr,omitempty"`
	// Token protects the API. Empty means a random token is generated
	// each run and printed at startup; set one here to keep stable URLs.
	Token string `json:"token,omitempty"`
	// APKPath points at a built Android APK to serve from the "App" tab
	// (GET /api/app/download); empty or missing means the tab shows
	// "no build yet". APKVersion is the human label shown beside it.
	APKPath    string `json:"apkPath,omitempty"`
	APKVersion string `json:"apkVersion,omitempty"`
}

// Ntfy configures outbound push notifications via an ntfy server
// (https://ntfy.sh or self-hosted). atc only ever POSTs outbound — the
// phone subscribes to its topic in the ntfy app — so this adds no inbound
// surface. Notifications are scoped to whoever started a session via a
// per-device topic; Topic is the fallback when a session has none.
type Ntfy struct {
	Enabled bool `json:"enabled,omitempty"`
	// Server is the ntfy base URL atc POSTs to (default https://ntfy.sh).
	// For a self-hosted server this can be a fast localhost URL.
	Server string `json:"server,omitempty"`
	// SubscribeURL is the ntfy base URL the *phone* uses to subscribe,
	// shown in the web "App" panel. Defaults to Server. Set this when atc
	// posts to localhost but the phone reaches ntfy over the tailnet
	// (e.g. Server=http://127.0.0.1:2586, SubscribeURL=https://host.ts.net:8443).
	SubscribeURL string `json:"subscribeUrl,omitempty"`
	// Topic is the fallback topic used when a session carries no
	// per-device topic (e.g. TUI/scheduler sessions). Optional.
	Topic string `json:"topic,omitempty"`
	// Token is an optional ntfy access token (Bearer) for protected
	// topics on a self-hosted server.
	Token string `json:"token,omitempty"`
	// ServerName labels the notification title (default the OS hostname),
	// so one phone can tell which atc instance fired the alert.
	ServerName string `json:"serverName,omitempty"`
	// PublicURL is atc's own tailnet URL (e.g.
	// https://myhost.tailnet.ts.net). When set, notifications get a
	// tap-to-open deep link to the session in the web UI.
	PublicURL string `json:"publicUrl,omitempty"`
	// Actions adds Approve/Deny buttons to permission notifications.
	// These embed the atc bearer token in the message, so only enable
	// them with a self-hosted ntfy you trust — never on ntfy.sh.
	Actions bool `json:"actions,omitempty"`
}

type Config struct {
	// WorktreeRoot is where per-session worktrees are created.
	// Empty means ~/.atc/worktrees/<repo>/<session>.
	WorktreeRoot string `json:"worktreeRoot,omitempty"`
	// LogLevel enables the diagnostic log: "off" (default), "info"
	// (session/permission/store lifecycle), or "debug" (+ every backend
	// event). LogFile overrides the location (default ~/.atc/atc.log) —
	// set it to wherever suits the machine. Metadata only, never
	// transcript content.
	LogLevel string `json:"logLevel,omitempty"`
	LogFile  string `json:"logFile,omitempty"`
	// ExportDir is where session transcripts are exported as markdown
	// (point it inside an Obsidian vault and exports land in the vault).
	// Empty means ~/.atc/exports. AutoExport makes `atc run` export
	// every completed session automatically.
	ExportDir  string `json:"exportDir,omitempty"`
	AutoExport bool   `json:"autoExport,omitempty"`
	// Repos are the repositories you usually work with; the new-session
	// form offers them as a picker. DefaultRepo pre-fills the repo field
	// (falls back to the first of Repos).
	Repos       []string `json:"repos,omitempty"`
	DefaultRepo string   `json:"defaultRepo,omitempty"`
	// DefaultBackend pre-selects the backend in the new-session forms
	// ("copilot" or "claude"); empty falls back to the built-in default.
	DefaultBackend string `json:"defaultBackend,omitempty"`
	// CategoryByRepo overrides the default board category for new
	// sessions, keyed by either the repo's absolute path or its base
	// directory name (e.g. {"smib-12362": "smib"} groups several repos
	// under one category). Unmatched repos default to their base name.
	CategoryByRepo map[string]string `json:"categoryByRepo,omitempty"`
	// DefaultAutoApprove starts new sessions in allow-all (the ⚡ auto
	// state) without a per-session toggle. Deny-list still gates Copilot;
	// for Claude this means the process spawns in bypassPermissions.
	DefaultAutoApprove bool                `json:"defaultAutoApprove,omitempty"`
	Model              string              `json:"model,omitempty"`
	Presets            map[string]Preset   `json:"presets,omitempty"`
	Hooks              map[string][]string `json:"hooks,omitempty"`
	Schedules          []Schedule          `json:"schedules,omitempty"`
	// ScheduledRetentionDays auto-cleans finished schedule-originated
	// sessions (and their worktrees) older than this many days, so a
	// recurring task doesn't pile up sessions forever. 0 (the default)
	// keeps them indefinitely. Only sessions launched by a schedule are
	// affected — manually started sessions are never auto-removed. The
	// sweep runs on a timer while atc is open and once after each headless
	// `atc run`, so cron-driven schedules self-clean even with no UI open.
	ScheduledRetentionDays int  `json:"scheduledRetentionDays,omitempty"`
	Web                    Web  `json:"web,omitempty"`
	Ntfy                   Ntfy `json:"ntfy,omitempty"`
}

// Path returns the default config file location:
// %APPDATA%\atc\config.json on Windows, ~/.config/atc/config.json elsewhere.
func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving user config dir: %w", err)
	}
	return filepath.Join(base, "atc", "config.json"), nil
}

// Load reads the config at path ("" means the default location).
// A missing file yields the default config, not an error.
func Load(path string) (*Config, error) {
	if path == "" {
		var err error
		if path, err = Path(); err != nil {
			return nil, err
		}
	}
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg.withDefaults(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg.withDefaults(), nil
}

func (c *Config) withDefaults() *Config {
	if c.Presets == nil {
		c.Presets = map[string]Preset{}
	}
	if _, ok := c.Presets["default"]; !ok {
		c.Presets["default"] = Preset{Approval: ApprovalPrompt}
	}
	return c
}

// Preset resolves a preset by name, falling back to a prompt-everything
// default for unknown names.
func (c *Config) Preset(name string) Preset {
	if p, ok := c.Presets[name]; ok {
		if p.Approval == "" {
			p.Approval = ApprovalPrompt
		}
		return p
	}
	return Preset{Approval: ApprovalPrompt}
}
