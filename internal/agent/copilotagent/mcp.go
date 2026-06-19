package copilotagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/rodolfojsv/atc/internal/agent"
)

// The SDK runtime atc spawns is headless: unlike the interactive `copilot`
// command, it does not auto-load user-level MCP config from COPILOT_HOME,
// and atc does not enable working-directory discovery. So a server the user
// drops in ~/.copilot/mcp.json never reaches a session. atc bridges that
// gap by reading the file itself and passing the servers in via
// SessionConfig.MCPServers.

// mcpConfigFiles are the filenames atc looks for under COPILOT_HOME, in
// preference order. mcp-config.json is the Copilot CLI's own name; mcp.json
// is accepted too since that's what people tend to create.
var mcpConfigFiles = []string{"mcp-config.json", "mcp.json"}

// copilotHome resolves the runtime's config home: COPILOT_HOME if set, else
// ~/.copilot (matching the SDK's default when atc supplies no BaseDirectory).
func copilotHome() string {
	if h := os.Getenv("COPILOT_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot")
}

// rawMCPFile mirrors the on-disk {"mcpServers": {...}} shape shared by the
// Copilot CLI and VS Code. Entries are decoded leniently so a file written
// for either tool loads here.
type rawMCPFile struct {
	MCPServers map[string]rawMCPServer `json:"mcpServers"`
}

type rawMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Tools   []string          `json:"tools"`
	Timeout int               `json:"timeout"`
}

// loadMCPServers reads the first present MCP config file under COPILOT_HOME
// and converts it to the SDK's server map. Returns (nil, nil) when no file
// exists — the common case — and a non-nil error only when a file is present
// but malformed, so callers can surface it without failing the session.
func loadMCPServers() (map[string]copilot.MCPServerConfig, error) {
	home := copilotHome()
	if home == "" {
		return nil, nil
	}
	var (
		data []byte
		path string
	)
	for _, name := range mcpConfigFiles {
		p := filepath.Join(home, name)
		if b, err := os.ReadFile(p); err == nil {
			data, path = b, p
			break
		}
	}
	if data == nil {
		return nil, nil
	}
	var file rawMCPFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(file.MCPServers) == 0 {
		return nil, nil
	}
	out := make(map[string]copilot.MCPServerConfig, len(file.MCPServers))
	for name, s := range file.MCPServers {
		out[name] = s.toConfig()
	}
	return out, nil
}

// reportMCPLoad emits a one-line transcript note about the MCP load: a
// confirmation when servers were seeded, or a warning when the config file
// was present but unparseable. Silent when there was nothing to load, so a
// session without an ~/.copilot/mcp.json stays quiet.
func reportMCPLoad(onEvent func(agent.Event), servers map[string]copilot.MCPServerConfig, err error) {
	if onEvent == nil {
		return
	}
	switch {
	case err != nil:
		onEvent(agent.Event{Type: agent.EventMessage, Text: "⚠ MCP config not loaded: " + err.Error()})
	case len(servers) > 0:
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)
		onEvent(agent.Event{Type: agent.EventMessage,
			Text: fmt.Sprintf("Loaded %d MCP server(s) from ~/.copilot: %s", len(names), strings.Join(names, ", "))})
	}
}

// toConfig picks the stdio vs HTTP shape: a url (or an http/sse type) means
// remote, otherwise it's a local stdio server launched via command/args.
func (s rawMCPServer) toConfig() copilot.MCPServerConfig {
	if s.URL != "" || s.Type == "http" || s.Type == "sse" {
		return copilot.MCPHTTPServerConfig{
			Tools:   s.Tools,
			Timeout: s.Timeout,
			URL:     s.URL,
			Headers: s.Headers,
		}
	}
	return copilot.MCPStdioServerConfig{
		Tools:            s.Tools,
		Timeout:          s.Timeout,
		Command:          s.Command,
		Args:             s.Args,
		Env:              s.Env,
		WorkingDirectory: s.Cwd,
	}
}
