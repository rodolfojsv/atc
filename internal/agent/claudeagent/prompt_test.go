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

// cursorOptionIndex must report which option the selection cursor sits on, so
// selectIndex navigates relative to the real highlight (the resume dialog
// defaults its cursor to a non-first option).
func TestCursorOptionIndex(t *testing.T) {
	cases := []struct {
		name string
		pane []string
		want int
	}{
		{"cursor on first", []string{"  ❯ 1. A", "    2. B", "    3. C"}, 0},
		{"cursor on second", []string{"    1. A", "  ❯ 2. B", "    3. C"}, 1},
		{"cursor on third", []string{"    1. A", "    2. B", "  ► 3. C"}, 2},
		{"no cursor", []string{"    1. A", "    2. B"}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cursorOptionIndex(strings.Join(c.pane, "\n")); got != c.want {
				t.Errorf("cursorOptionIndex = %d, want %d", got, c.want)
			}
		})
	}
}

// optionIndexFor maps a bare number or matching label to the right option, and
// returns -1 for a genuine freeform answer so it's typed rather than mis-mapped.
func TestOptionIndexFor(t *testing.T) {
	opts := []promptOption{{label: "Resume from summary (recommended)"}, {label: "Resume full session as-is"}, {label: "Don't ask me again"}}
	cases := []struct {
		answer string
		want   int
	}{
		{"1", 0},
		{" 2 ", 1},
		{"3", 2},
		{"4", -1}, // out of range → not a selector
		{"full session", 1},
		{"something else entirely", -1},
	}
	for _, c := range cases {
		if got := optionIndexFor(c.answer, opts); got != c.want {
			t.Errorf("optionIndexFor(%q) = %d, want %d", c.answer, got, c.want)
		}
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
