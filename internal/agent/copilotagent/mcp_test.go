package copilotagent

import (
	"os"
	"path/filepath"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

func TestLoadMCPServers(t *testing.T) {
	dir := t.TempDir()
	const cfg = `{
	  "mcpServers": {
	    "obsidian-rjsv": {
	      "type": "stdio",
	      "command": "npx",
	      "args": ["-y", "obsidian-mcp"],
	      "env": {"VAULT": "/vault"},
	      "tools": ["*"]
	    },
	    "remote": {
	      "type": "http",
	      "url": "https://example.com/mcp",
	      "headers": {"Authorization": "Bearer x"}
	    }
	  }
	}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", dir)

	servers, err := loadMCPServers()
	if err != nil {
		t.Fatalf("loadMCPServers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %d", len(servers))
	}

	stdio, ok := servers["obsidian-rjsv"].(copilot.MCPStdioServerConfig)
	if !ok {
		t.Fatalf("obsidian-rjsv: want MCPStdioServerConfig, got %T", servers["obsidian-rjsv"])
	}
	if stdio.Command != "npx" || len(stdio.Args) != 2 || stdio.Env["VAULT"] != "/vault" {
		t.Errorf("stdio config mismatch: %+v", stdio)
	}

	http, ok := servers["remote"].(copilot.MCPHTTPServerConfig)
	if !ok {
		t.Fatalf("remote: want MCPHTTPServerConfig, got %T", servers["remote"])
	}
	if http.URL != "https://example.com/mcp" || http.Headers["Authorization"] != "Bearer x" {
		t.Errorf("http config mismatch: %+v", http)
	}
}

func TestLoadMCPServersPrefersConfigName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPILOT_HOME", dir)
	write := func(name, server string) {
		body := `{"mcpServers":{"` + server + `":{"command":"x"}}}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mcp.json", "fromMcpJson")
	write("mcp-config.json", "fromConfig")

	servers, err := loadMCPServers()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := servers["fromConfig"]; !ok {
		t.Errorf("expected mcp-config.json to win, got %v", servers)
	}
}

func TestLoadMCPServersMissing(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	servers, err := loadMCPServers()
	if err != nil || servers != nil {
		t.Fatalf("want (nil, nil) for missing file, got (%v, %v)", servers, err)
	}
}

func TestLoadMCPServersMalformed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPILOT_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMCPServers(); err == nil {
		t.Fatal("want error for malformed config, got nil")
	}
}
