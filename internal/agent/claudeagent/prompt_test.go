package claudeagent

import (
	"strings"
	"testing"
)

func TestDetectPermissionPrompt(t *testing.T) {
	pane := strings.Join([]string{
		"  ⏺ Bash(go test ./...)",
		"",
		"  Do you want to proceed?",
		"  ❯ 1. Yes",
		"    2. Yes, and don't ask again this session",
		"    3. No, and tell Claude what to do differently",
		"",
	}, "\n")

	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected a prompt to be detected")
	}
	if p.kind != "permission" {
		t.Errorf("kind = %q, want permission", p.kind)
	}
	if len(p.options) != 3 {
		t.Fatalf("want 3 options, got %d: %+v", len(p.options), p.options)
	}
	if indexMatching(p.options, alwaysMarkers) != 1 {
		t.Errorf("don't-ask-again option not matched at index 1: %+v", p.options)
	}
	if indexMatching(p.options, denyTalkMarker) != 2 {
		t.Errorf("deny-with-feedback option not matched at index 2: %+v", p.options)
	}
}

func TestDetectQuestionPrompt(t *testing.T) {
	pane := strings.Join([]string{
		"  Indentation: Tabs or spaces?",
		"  ❯ 1. Tabs",
		"    2. Spaces",
	}, "\n")

	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected a prompt to be detected")
	}
	if p.kind != "question" {
		t.Errorf("kind = %q, want question", p.kind)
	}
	if indexMatching(p.options, []string{"Spaces"}) != 1 {
		t.Errorf("answer match failed: %+v", p.options)
	}
}

// A numbered list in ordinary assistant prose (no selection cursor, no
// permission wording) must NOT be treated as an interactive prompt.
func TestDetectProseIsNotAPrompt(t *testing.T) {
	pane := strings.Join([]string{
		"Here are the steps to set up:",
		"1. Install the dependencies",
		"2. Run the test suite",
		"3. Build the binary",
	}, "\n")

	if _, ok := detectPrompt(pane); ok {
		t.Error("numbered prose was wrongly detected as a prompt")
	}
}

func TestIndexMatchingCaseInsensitive(t *testing.T) {
	opts := []promptOption{{label: "Yes"}, {label: "No, and tell Claude what to do differently"}}
	if i := indexMatching(opts, yesMarkers); i != 0 {
		t.Errorf("yes match = %d, want 0", i)
	}
	if i := indexMatching(opts, []string{"NO"}); i != 1 {
		t.Errorf("case-insensitive No match = %d, want 1", i)
	}
	if i := indexMatching(opts, []string{"maybe"}); i != -1 {
		t.Errorf("no-match should be -1, got %d", i)
	}
}
