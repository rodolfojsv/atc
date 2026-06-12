// Package policy decides what happens to a permission request before a
// human ever sees it. The deterministic deny-list runs first and applies
// to every approval mode — "allow-all" means "allow all except obviously
// catastrophic", never literally all.
package policy

import (
	"regexp"

	"github.com/github/copilot-sdk/go/rpc"

	"github.com/rodolfojsv/atc/internal/config"
)

type Verdict int

const (
	Ask Verdict = iota
	Allow
	Deny
)

type rule struct {
	re     *regexp.Regexp
	reason string
}

func mustRule(expr, reason string) rule {
	return rule{re: regexp.MustCompile(`(?i)` + expr), reason: reason}
}

// shellDeny is matched against the full shell command text.
var shellDeny = []rule{
	// Recursive/forced deletes aimed at a filesystem root, home, or drive.
	mustRule(`\brm\s+(-{1,2}\S+\s+)*(/|~/?|\$HOME|%USERPROFILE%)\s*$`, "recursive delete of root or home"),
	mustRule(`\brm\s+(-{1,2}\S+\s+)*[A-Za-z]:[\\/]?\s*$`, "recursive delete of a drive"),
	mustRule(`\b(rd|rmdir)\s+/s\b`, "recursive directory delete (rd /s)"),
	mustRule(`\bdel\s+(/\w+\s+)*[A-Za-z]:\\`, "mass delete on a drive root"),
	// Disk and filesystem destruction.
	mustRule(`\bmkfs(\.\w+)?\b`, "filesystem format"),
	mustRule(`\bdd\b[^|]*\bof=/dev/`, "raw write to a block device"),
	mustRule(`\bformat(\.com)?\s+[A-Za-z]:`, "drive format"),
	mustRule(`\bdiskpart\b`, "disk partitioning"),
	// Remote code execution via pipe-to-shell.
	mustRule(`\b(curl|wget|iwr|invoke-webrequest)\b[^|;&]*\|\s*&?\s*(ba|z|da)?sh\b`, "pipe download to shell"),
	mustRule(`\b(curl|wget|iwr|invoke-webrequest)\b[^|;&]*\|\s*(pwsh|powershell)\b`, "pipe download to PowerShell"),
	mustRule(`\b(iex|invoke-expression)\b`, "PowerShell Invoke-Expression"),
	// Credential and key material.
	mustRule(`\bid_(rsa|ed25519|ecdsa|dsa)\b`, "SSH private key access"),
	mustRule(`\.aws[/\\]credentials|\.netrc\b|\.npmrc\b|\.gnupg\b|\.docker[/\\]config\.json`, "credential file access"),
	// History destruction on shared branches.
	mustRule(`\bgit\s+push\b[^|;&]*(--force|-f)\b[^|;&]*\b(main|master)\b`, "force-push to main/master"),
	// Host control.
	mustRule(`(^|[;&|]\s*)(shutdown|reboot|halt)\b`, "host shutdown/reboot"),
	mustRule(`\bsystemctl\s+(poweroff|reboot|halt)\b`, "host shutdown/reboot"),
	mustRule(`\breg\s+delete\s+hk(lm|cu)`, "registry hive deletion"),
	// Classic fork bomb.
	{re: regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), reason: "fork bomb"},
}

// pathDeny is matched against paths the agent wants to read or write.
var pathDeny = []rule{
	mustRule(`\bid_(rsa|ed25519|ecdsa|dsa)$`, "SSH private key"),
	mustRule(`\.aws[/\\]credentials$|\.netrc$|\.npmrc$|\.gnupg\b|\.docker[/\\]config\.json$`, "credential file"),
	mustRule(`[/\\]etc[/\\](passwd|shadow|sudoers)$`, "system auth file"),
}

// Evaluate applies the deny-list, then the preset's approval mode.
// A non-empty reason accompanies Deny.
func Evaluate(approval string, req rpc.PermissionRequest) (Verdict, string) {
	if reason := denied(req); reason != "" {
		return Deny, reason
	}
	if approval == config.ApprovalAllowAll {
		return Allow, ""
	}
	return Ask, ""
}

func denied(req rpc.PermissionRequest) string {
	switch r := req.(type) {
	case *rpc.PermissionRequestShell:
		return matchRules(shellDeny, r.FullCommandText)
	case *rpc.PermissionRequestWrite:
		return matchRules(pathDeny, r.FileName)
	case *rpc.PermissionRequestRead:
		return matchRules(pathDeny, r.Path)
	}
	return ""
}

func matchRules(rules []rule, text string) string {
	for _, r := range rules {
		if r.re.MatchString(text) {
			return r.reason
		}
	}
	return ""
}
