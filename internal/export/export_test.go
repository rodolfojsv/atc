package export

import (
	"os"
	"strings"
	"testing"

	"github.com/rodolfojsv/atc/internal/supervisor"
)

func TestWrite(t *testing.T) {
	v := supervisor.SessionView{
		Name: "pr-triage", Repo: "/r", Branch: "atc/pr-triage", Backend: "copilot",
		Status: supervisor.StatusDone,
	}
	v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.NanoAiu = 1000, 500, 4.2e8
	entries := []supervisor.Entry{
		{Kind: supervisor.EntryUser, Text: "triage my PRs"},
		{Kind: supervisor.EntryTool, Text: "bash · gh pr list"},
		{Kind: supervisor.EntryAssistant, Text: "## Findings\n\nTwo PRs need attention."},
		{Kind: supervisor.EntrySystem, Text: "auto-approved: something"},
		{Kind: supervisor.EntryAssistant, Text: "streaming...", Partial: true},
	}
	path, err := Write(t.TempDir(), v, entries)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{"## ❯ triage my PRs", "gh pr list", "Two PRs need attention", "aic: 0.420", "backend: copilot"} {
		if !strings.Contains(out, want) {
			t.Errorf("export missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "streaming...") || strings.Contains(out, "auto-approved") {
		t.Error("partial/system entries must not be exported")
	}
}
