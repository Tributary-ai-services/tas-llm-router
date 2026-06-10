// Agent-flow layer for the AIQG demo-traffic generator.
//
// Groups individual step-events into the agent identity hierarchy from
// docs/AIQG-AGENT-FLOW-ATTRIBUTION.md:
//
//	agent → conversation → flow (run) → step
//
// Each demo agent persona runs flows whose steps span workflow types
// (an orchestrator flow = planner step + retrieval steps + synthesis
// step). Steps share a flow_id and conversation_id and link via
// step_id/parent_step_id, so the dashboard's per-agent rollup and flow
// drill-down (trace-waterfall) both have real structure to render.
//
// Field names match the AgentContext schema the gateway emitter (#6)
// will promote, so this generator pins what the dashboard queries:
// `| json | agent_id="..."`, group `by (flow_id)`.
package main

import (
	"fmt"
	"hash/fnv"
	"math/rand"
)

// agentContext is the per-step agent-flow identity stamped onto an event.
type agentContext struct {
	AgentID        string
	AgentName      string
	AgentVersion   string
	ConversationID string
	FlowID         string
	StepID         string
	ParentStepID   string
	FlowStepSeq    int
	IdentitySource string // header | trace | inferred
}

// stamp writes the agent-flow fields onto the emitter field map using
// the promoted top-level names. Empty fields are omitted (matching the
// emitter's omitempty behaviour) — notably, inferred flows carry no
// agent_id/agent_name.
func (a agentContext) stamp(f map[string]any) {
	put := func(k, v string) {
		if v != "" {
			f[k] = v
		}
	}
	put("agent_id", a.AgentID)
	put("agent_name", a.AgentName)
	put("agent_version", a.AgentVersion)
	put("conversation_id", a.ConversationID)
	put("flow_id", a.FlowID)
	put("step_id", a.StepID)
	put("parent_step_id", a.ParentStepID)
	put("identity_source", a.IdentitySource)
	if a.FlowStepSeq > 0 {
		f["flow_step_seq"] = a.FlowStepSeq
	}
}

// stepSpec is one step in a flow template: which workflow profile runs,
// and which earlier step (by index) is its parent (-1 = flow root).
type stepSpec struct {
	Profile string
	Parent  int
}

// agentPersona is a named demo agent and the shape of the flows it runs.
type agentPersona struct {
	Name      string
	Version   string
	TurnsLo   int     // flows per conversation (multi-turn); 1 = each flow its own conversation
	TurnsHi   int
	TraceProb float64 // fraction of flows whose flow_id arrived via W3C traceparent (identity_source=trace) vs an explicit header
	BuildFlow func(g rng) []stepSpec
}

// personas — four demo agents whose flows cover six workflow types.
var personas = []agentPersona{
	{
		Name: "Research Orchestrator", Version: "1.4.0",
		TurnsLo: 1, TurnsHi: 1, TraceProb: 0.30,
		BuildFlow: func(g rng) []stepSpec {
			// planner → N× retrieve → synthesize (fan-out under the planner)
			steps := []stepSpec{{Profile: "multi_agent_orchestration", Parent: -1}}
			for n := g.intIn(2, 4); n > 0; n-- {
				steps = append(steps, stepSpec{Profile: "rag", Parent: 0})
			}
			return append(steps, stepSpec{Profile: "summarization", Parent: 0})
		},
	},
	{
		Name: "Coding Copilot", Version: "2.1.0",
		TurnsLo: 1, TurnsHi: 2, TraceProb: 0.20,
		BuildFlow: func(g rng) []stepSpec {
			// plan → generate → (sometimes) fix-on-fail
			steps := []stepSpec{
				{Profile: "mcp_agentic", Parent: -1},
				{Profile: "code_generation", Parent: 0},
			}
			if g.chance(0.4) {
				steps = append(steps, stepSpec{Profile: "code_generation", Parent: 1})
			}
			return steps
		},
	},
	{
		Name: "Support Bot", Version: "3.0.1",
		TurnsLo: 2, TurnsHi: 4, TraceProb: 0.10,
		BuildFlow: func(g rng) []stepSpec {
			// classify intent → answer; multi-turn over one conversation_id
			return []stepSpec{
				{Profile: "classification_extraction", Parent: -1},
				{Profile: "conversational", Parent: 0},
			}
		},
	},
	{
		Name: "Data Extractor", Version: "1.0.0",
		TurnsLo: 1, TurnsHi: 1, TraceProb: 0.0,
		BuildFlow: func(g rng) []stepSpec {
			// batch: N sibling extraction steps under the first
			steps := []stepSpec{{Profile: "classification_extraction", Parent: -1}}
			for n := g.intIn(3, 6); n > 1; n-- {
				steps = append(steps, stepSpec{Profile: "classification_extraction", Parent: 0})
			}
			return steps
		},
	},
}

// profileByKey indexes the workflow profiles by their stable Key so flow
// step specs can resolve the profile to sample from.
var profileByKey = func() map[string]profile {
	m := make(map[string]profile, len(profiles))
	for _, p := range profiles {
		m[p.Key] = p
	}
	return m
}()

// agentIDFor returns a stable v4-shaped id derived from the agent name,
// so the same demo agent keeps the same agent_id across passes and runs
// (the agent is a definition, not a per-run value) regardless of --seed.
func agentIDFor(name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return uuidFromSource(rand.New(rand.NewSource(int64(h.Sum64()))))
}

// hex returns n random bytes as 2n lowercase hex chars (W3C trace ids are
// 16 bytes / 32 hex, span ids 8 bytes / 16 hex).
func (g rng) hex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(g.r.Intn(256))
	}
	return fmt.Sprintf("%x", b)
}

// uuidFromSource renders a v4-shaped id from an arbitrary rand source.
func uuidFromSource(r *rand.Rand) string {
	var b [16]byte
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
