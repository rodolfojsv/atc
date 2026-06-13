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
	// DefaultAutoApprove starts new sessions in allow-all (the ⚡ auto
	// state) without a per-session toggle. Deny-list still gates Copilot;
	// for Claude this means the process spawns in bypassPermissions.
	DefaultAutoApprove bool                `json:"defaultAutoApprove,omitempty"`
	Model              string              `json:"model,omitempty"`
	Presets            map[string]Preset   `json:"presets,omitempty"`
	Hooks              map[string][]string `json:"hooks,omitempty"`
	Schedules          []Schedule          `json:"schedules,omitempty"`
	Web                Web                 `json:"web,omitempty"`
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
