package supervisor

import (
	"testing"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
)

// spec() must inject every configured custom agent (in stable name order)
// and activate the one tagged onto the session — the mechanism that lets a
// custom persona drive a repo without committing .github/.claude agents.
func TestSpecTagsAgents(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents = map[string]config.AgentDef{
		"reviewer": {Description: "rev", Prompt: "p1", Tools: []string{"Read"}, Model: "sonnet"},
		"scribe":   {Prompt: "p2"},
	}
	s := New(cfg, bus.New())

	spec := s.spec(&Session{Name: "x", Preset: "default", Agent: "reviewer"}, "")
	if spec.Agent != "reviewer" {
		t.Fatalf("spec.Agent = %q, want reviewer", spec.Agent)
	}
	if len(spec.Agents) != 2 {
		t.Fatalf("len(spec.Agents) = %d, want 2", len(spec.Agents))
	}
	// AgentNames sorts, so reviewer precedes scribe.
	if spec.Agents[0].Name != "reviewer" || spec.Agents[1].Name != "scribe" {
		t.Fatalf("agents not in sorted order: %+v", spec.Agents)
	}
	if spec.Agents[0].Prompt != "p1" || spec.Agents[0].Model != "sonnet" {
		t.Errorf("reviewer def not carried through: %+v", spec.Agents[0])
	}
}

// A tagged agent that no longer exists in config must be dropped, not
// passed to the backend (where it would reject the launch).
func TestSpecDropsUnknownAgent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents = map[string]config.AgentDef{"reviewer": {Prompt: "p"}}
	s := New(cfg, bus.New())

	if got := s.spec(&Session{Name: "y", Preset: "default", Agent: "ghost"}, "").Agent; got != "" {
		t.Fatalf("spec.Agent = %q, want empty (unknown dropped)", got)
	}
}

// No configured agents → no agent payload at all.
func TestSpecNoAgents(t *testing.T) {
	s := New(testConfig(t), bus.New())
	spec := s.spec(&Session{Name: "z", Preset: "default"}, "")
	if spec.Agents != nil || spec.Agent != "" {
		t.Fatalf("expected no agents, got Agent=%q Agents=%+v", spec.Agent, spec.Agents)
	}
}
