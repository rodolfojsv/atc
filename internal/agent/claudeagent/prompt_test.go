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
		"  Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
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

// Claude's multi-question AskUserQuestion is a tabbed form: one question per
// pane, each option carrying an indented (un-numbered) description line, with a
// tab bar and a navigation hint as chrome. Detection must surface exactly the
// visible question — title above the blank gap, option labels only (not their
// descriptions) — captured live from Claude Code v2.1.x.
func TestDetectTabbedQuestion(t *testing.T) {
	pane := strings.Join([]string{
		"←  ☐ Color  ☐ Size  ✔ Submit  →",
		"",
		"What is your favorite color?",
		"",
		"❯ 1. Red",
		"     The color red.",
		"  2. Blue",
		"     The color blue.",
		"  3. Green",
		"     The color green.",
		"  4. Type something.",
		"  5. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}, "\n")

	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected the tabbed question to be detected")
	}
	if p.kind != "question" {
		t.Fatalf("kind = %q, want question", p.kind)
	}
	// Title skips the blank gap and ignores the tab bar above it.
	if p.title != "What is your favorite color?" {
		t.Errorf("title = %q", p.title)
	}
	// Options are the labels only — description lines have no number and must
	// not be picked up as extra options.
	if len(p.options) != 5 {
		t.Fatalf("want 5 options (labels only), got %d: %+v", len(p.options), p.options)
	}
	if p.options[0].label != "Red" || p.options[2].label != "Green" {
		t.Errorf("option labels wrong: %+v", p.options)
	}
	if p.isSubmitConfirm() {
		t.Error("a real question must not be mistaken for the submit tab")
	}

	// Each option's description is scraped from the indented line under it.
	if p.options[0].detail != "The color red." || p.options[2].detail != "The color green." {
		t.Errorf("option details wrong: %q / %q", p.options[0].detail, p.options[2].detail)
	}

	// "Type something" / "Chat about this" are escape hatches, not answers: they
	// must be hidden from what the user is offered (the index of a real option in
	// the full list is still used to drive the on-screen selection).
	surfaced, details := p.answerOptions()
	if len(surfaced) != 3 || len(details) != 3 {
		t.Fatalf("want 3 surfaced options+details (Red/Blue/Green), got %v / %v", surfaced, details)
	}
	if details[0] != "The color red." {
		t.Errorf("surfaced detail not carried: %q", details[0])
	}
	for _, s := range surfaced {
		if strings.Contains(s, "Type something") || strings.Contains(s, "Chat about") {
			t.Errorf("escape-hatch option leaked into surfaced answers: %v", surfaced)
		}
	}
	if indexMatching(p.options, typeSomethingMarkers) != 3 {
		t.Errorf(`"Type something" should sit at on-screen index 3: %+v`, p.options)
	}
}

// Descriptions can wrap onto a second line, and a horizontal-rule separator must
// not leak into the scraped text.
func TestOptionDetailWrapAndSeparator(t *testing.T) {
	pane := strings.Join([]string{
		"←  ☐ Deploy  ✔ Submit  →",
		"",
		"Which deployment strategy?",
		"❯ 1. Blue-green",
		"     Run two identical environments and switch",
		"     traffic to the new one all at once.",
		"  2. Canary",
		"     Release to a small subset first.",
		"────────────────────────────",
		"  3. Type something.",
		"",
		"Enter to select · Esc to cancel",
	}, "\n")

	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected detection")
	}
	if p.options[0].detail != "Run two identical environments and switch traffic to the new one all at once." {
		t.Errorf("wrapped detail not joined: %q", p.options[0].detail)
	}
	if p.options[1].detail != "Release to a small subset first." {
		t.Errorf("detail 1 wrong (separator may have leaked): %q", p.options[1].detail)
	}
}

// The form's final tab is a submit confirmation, recognized so it's answered
// automatically rather than surfaced as another question.
func TestDetectSubmitConfirm(t *testing.T) {
	pane := strings.Join([]string{
		"←  ☒ Color  ☒ Size  ✔ Submit  →",
		"",
		"Ready to submit your answers?",
		"❯ 1. Submit answers",
		"  2. Cancel",
		"",
		"Enter to select · Esc to cancel",
	}, "\n")

	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected the submit confirmation to be detected")
	}
	if !p.isSubmitConfirm() {
		t.Errorf("submit tab not recognized: %+v", p.options)
	}
	if i := indexMatching(p.options, submitMarkers); i != 0 {
		t.Errorf("submit option not matched at index 0: %+v", p.options)
	}
}

// Assistant prose that pastes a "❯ 1. …" picker example must NOT be detected as
// a live box: it has the cursor glyph and numbered lines but none of the
// picker's chrome (no nav hint, no tab glyphs). This was a real false positive.
func TestProseWithCursorExampleIsNotAPicker(t *testing.T) {
	pane := strings.Join([]string{
		"● Those long prompts carry descriptions that wrap, like this:",
		"",
		"    ❯ 1. Marco floral en el fondo",
		"         Las rosas viven en el fondo de la página…",
		"       2. Esquinas por bloque",
		"         Cada bloque trae sus propias esquinas…",
	}, "\n")

	if _, ok := detectPrompt(pane); ok {
		t.Error("prose containing a pasted picker example was wrongly detected as a live picker")
	}
}

func TestStripBoxGlyphs(t *testing.T) {
	if got := strings.TrimSpace(stripBoxGlyphs("│ ¿Por dónde arrancamos? │")); got != "¿Por dónde arrancamos?" {
		t.Errorf("stripBoxGlyphs = %q", got)
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

// A multiSelect AskUserQuestion renders as a checkbox frame: each option line
// carries a box glyph and the hint mentions Space to toggle. Detection must flag
// it multiSelect and still strip the box off the labels.
func TestDetectMultiSelectQuestion(t *testing.T) {
	pane := strings.Join([]string{
		"¿Qué grupos quieres curar primero?",
		"",
		"❯ 1. ☒ Serif elegantes",
		"     Cormorant, Playfair.",
		"  2. ☐ Scripts / caligráficas",
		"  3. ☐ Sans limpias",
		"  4. Type something",
		"",
		"Space to select · Enter to confirm · Esc to cancel",
	}, "\n")

	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected the multi-select question to be detected")
	}
	if !p.multiSelect {
		t.Error("multiSelect should be true for a checkbox frame")
	}
	if p.options[0].label != "Serif elegantes" || p.options[1].label != "Scripts / caligráficas" {
		t.Errorf("box glyph not stripped from labels: %+v", p.options)
	}
}

// The multi-*question* tabbed form (single-select per tab) draws ☐/✔ in its tab
// bar but never on a numbered option line — it must NOT be flagged multiSelect.
func TestTabbedQuestionIsNotMultiSelect(t *testing.T) {
	pane := strings.Join([]string{
		"←  ☐ Color  ☐ Size  ✔ Submit  →",
		"",
		"What is your favorite color?",
		"",
		"❯ 1. Red",
		"  2. Blue",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}, "\n")
	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("expected detection")
	}
	if p.multiSelect {
		t.Error("tab-bar ☐/✔ must not be read as a multi-select picker")
	}
}

// optionIndicesFor resolves a multi-select answer into the chosen rows: the
// whole string first (a single label, comma and all), then split on newlines or
// commas, de-duplicated and order-preserved.
func TestOptionIndicesFor(t *testing.T) {
	opts := []promptOption{{label: "Serif elegantes"}, {label: "Scripts / caligráficas"}, {label: "Sans limpias"}}
	cases := []struct {
		answer string
		want   []int
	}{
		{"Serif elegantes\nSans limpias", []int{0, 2}},
		{"1, 3", []int{0, 2}},
		{"Scripts / caligráficas", []int{1}}, // whole-string match wins despite the comma
		{"2\n2\nSans limpias", []int{1, 2}},  // duplicates dropped
		{"nothing here", nil},
	}
	for _, c := range cases {
		got := optionIndicesFor(c.answer, opts)
		if len(got) != len(c.want) {
			t.Errorf("optionIndicesFor(%q) = %v, want %v", c.answer, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("optionIndicesFor(%q) = %v, want %v", c.answer, got, c.want)
				break
			}
		}
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

// Claude Code 2.1.x renders an AskUserQuestion whose options carry `preview`
// content side-by-side: the focused option's preview in a box to the right, and
// the selected row marked by ANSI colour (bold + bright) instead of a ❯ glyph.
// A plain capture strips the colour, so the row looks cursor-less and the box was
// never detected — the session wedged in "working" with the picker waiting.
// normalizePromptPane (fed an escape-preserving capture) must recover both the
// cursor and clean labels. Escapes mirror a real `capture-pane -e`.
func TestDetectPreviewLayoutQuestion(t *testing.T) {
	const e = "\x1b"
	escPane := strings.Join([]string{
		e + "[38;5;246m──────────────────────────────────────" + e + "[39m",
		" ☐ Marco y Sello",
		"¿Cómo quieres resolver Marco y Sello para que el texto se lea en Noche Tinto?",
		e + "[38;5;246m 1." + e + "[1m" + e + "[38;5;153m Controles propios por " + e + "[0m        " + e + "[38;5;246m┌────────────────────┐" + e + "[39m",
		"   acomodo                        " + e + "[38;5;246m│ Muestrario:        │" + e + "[39m",
		e + "[38;5;246m 2." + e + "[39m Que el papel siga el tema    " + e + "[38;5;246m│" + e + "[39m   [ base | bloque ]  " + e + "[38;5;246m│" + e + "[39m",
		e + "[38;5;246m 3." + e + "[39m Tinta oscura fija en el       " + e + "[38;5;246m│" + e + "[39m   #2b303a oscuro     " + e + "[38;5;246m│" + e + "[39m",
		"   papel                          " + e + "[38;5;246m└────────────────────┘" + e + "[39m",
		"Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel",
	}, "\n")

	pane := normalizePromptPane(escPane)
	p, ok := detectPrompt(pane)
	if !ok {
		t.Fatal("preview-layout picker (colour-highlighted, no ❯) must be detected")
	}
	if p.kind != "question" {
		t.Fatalf("kind = %q, want question", p.kind)
	}
	if p.multiSelect {
		t.Error("single-select picker must not be flagged multiSelect")
	}
	if p.title != "¿Cómo quieres resolver Marco y Sello para que el texto se lea en Noche Tinto?" {
		t.Errorf("title = %q", p.title)
	}
	if len(p.options) != 3 {
		t.Fatalf("want 3 options, got %d: %+v", len(p.options), p.options)
	}
	// Labels must be clean — the preview panel (box borders, panel text) scraped off.
	want := []string{"Controles propios por", "Que el papel siga el tema", "Tinta oscura fija en el"}
	for i, w := range want {
		if p.options[i].label != w {
			t.Errorf("option[%d].label = %q, want %q", i, p.options[i].label, w)
		}
		if strings.ContainsAny(p.options[i].label, "│┌┐└┘") {
			t.Errorf("option[%d].label leaked preview-panel chars: %q", i, p.options[i].label)
		}
	}
	// The colour-highlighted first row must be reported as the live cursor so
	// selection navigates relative to the right starting position.
	if c := cursorOptionIndex(pane); c != 0 {
		t.Errorf("cursorOptionIndex = %d, want 0 (highlighted row)", c)
	}
}
