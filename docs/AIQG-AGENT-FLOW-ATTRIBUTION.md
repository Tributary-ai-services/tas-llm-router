# AIQG — Traffic Attribution Per Agent Flow

Status: **Design / planning** (locked identity model, not yet implemented)
Owner: AIQG
Related: [`AIQG-EXTENSION.md`](./AIQG-EXTENSION.md), `aether-shared/data-models/aiqg/`

## Problem

AIQG today classifies each request by `workflow_type` and scores it (CLEAR),
but it cannot answer **"which agent, and which agent run, produced this
traffic?"** A single agent execution fans out into many gateway calls
(planner → workers, ReAct tool loops, RAG sub-queries) spanning multiple
workflow types. We want to **group gateway requests into the agent, the
agent run/flow, the conversation, and the step that produced them**, and
roll cost / CLEAR / avoidable-cost up to the **agent** and the **flow**.

A *flow is orthogonal to* `workflow_type`: one orchestration flow contains
agentic + rag + summarization *steps*. This feature adds a grouping axis
*above* the per-request classification we already have.

## Prior art (informs the model)

Surveyed standards, observability tools, and identity protocols
(deep-research, 2026-06; 21 verified claims). Consensus:

- **OpenTelemetry GenAI semantic conventions** (experimental) define the
  canonical layered scheme:
  - `gen_ai.agent.id` (machine id) + `gen_ai.agent.name` (human) +
    `gen_ai.agent.version` / `.description` — names the AGENT.
  - `gen_ai.conversation.id` — folds **session + thread into ONE id**.
  - the **trace** (`trace_id`) is the RUN; **spans** are STEPs, named by
    `gen_ai.operation.name` (`invoke_agent`, `execute_tool`,
    `invoke_workflow`), linked via `trace_id`/`span_id`/`parent_span_id`.
- **Observability tools converge** on the same layering with different
  names: LangSmith `run_id`+`parent_run_id`+`trace_id` and interchangeable
  `session_id`/`conversation_id`/`thread_id`; Langfuse 32-hex trace +
  16-hex observation + free-form `sessionId`; Helicone pure-HTTP-header
  `Helicone-Session-Id` + `Helicone-Session-Path` ('/' tree) +
  `Helicone-Session-Name`.
- **Propagation**: SDK-managed trace context in libraries; **a gateway
  must use headers** (Helicone is the proven header-only model), optionally
  honoring **W3C `traceparent`** for trace/span correlation.
- **Trust**: today's attribution is **self-asserted** (caller picks the
  ids/names) everywhere. **Authenticated agent identity is the frontier** —
  AIP (Agent Identity Protocol: `aip:web:` / `aip:key:ed25519:` ids,
  Ed25519-signed identity docs, Invocation-Bound Capability Tokens via
  `X-AIP-Token`), Google A2A, WSO2 AI Gateway "AgentID".
- **Gap**: patent freedom-to-operate is **unresolved** — 4 USPTO patents
  on gateway attribution/metering were located (US 12,483,411 /
  12,547,677 / 10,924,326 / 11,785,115) but their claims were not
  verified. FTO due diligence is owed before this feeds billing.

## Locked identity model

Four levels, named with OTel `gen_ai.*` where a standard attribute exists:

```
tenant
  └─ agent            gen_ai.agent.id  (+ gen_ai.agent.name, .version)   WHO
       └─ conversation gen_ai.conversation.id   (session + thread folded) THREAD
            └─ flow     flow_id  (= W3C trace_id when present)            ONE RUN
                 └─ step  step_id (= W3C span_id) + parent_step_id        ONE CALL
```

- A **step** is exactly today's `request_event` / `response_event`.
- A **flow** groups all steps sharing `flow_id` — one agent execution.
- **Conversation** groups flows in one thread; reuses/extends the existing
  `session_id_inferred`.
- **Agent** is the stable definition (maps to tas-agent-builder
  `Agent.ID`/`Name`).

### Identity sources (asserted-when-available, inferred-otherwise)

Every event records `identity_source ∈ {header, trace, inferred}` so the
dashboard never conflates a named flow with a guessed one.

| Field | Primary source | Fallback |
|---|---|---|
| `gen_ai.agent.id` / `.name` | `TAS-Agent-Id` / `TAS-Agent-Name` header | (none → unattributed) |
| `flow_id` / `step_id` / `parent_step_id` | W3C `traceparent` (`trace_id`/`span_id`) | `TAS-Flow-Id` header → else inferred from `session_id_inferred` + tool-chain |
| `gen_ai.conversation.id` | `TAS-Conversation-Id` header | existing `session_id_inferred` |
| `agent_version` | `TAS-Agent-Version` header | omit |

**Trust path**: ship self-asserted (matches the entire market); design the
schema so an **authenticated** `agent_id` (AIP IBCT via `X-AIP-Token`, or a
binding to the `TAS-Auth` token's claims, or WSO2-style AgentID) can slot in
later without a migration. A future `agent_identity_verified` boolean +
`agent_credential_ref` cover this.

## Header contract (new)

Added to `internal/middleware/aiqg_headers.go` `canonicalHeaderNames`
(parsed, validated, and **stripped before the vendor** via the existing
`StripFromOutbound`):

| Header | Meaning | Validation |
|---|---|---|
| `TAS-Agent-Id` | stable agent id | ≤128 chars |
| `TAS-Agent-Name` | human-readable agent name | ≤128 chars |
| `TAS-Agent-Version` | agent version | ≤64 chars, optional |
| `TAS-Flow-Id` | explicit run/flow id (when not using traceparent) | ≤128 chars |
| `TAS-Conversation-Id` | conversation/thread id | ≤128 chars |

Standard headers also honored (read, not stripped): **W3C `traceparent`**
(→ `flow_id`=trace_id, `step_id`=span_id), `tracestate` (passthrough).
Existing `X-Session-ID` / `X-Request-ID` behavior unchanged.

## Event schema additions (additive only)

Per the AIQG non-breaking constraint, add a new optional sub-struct to
`pkg/aiqg/events/event.go` `RequestEvent` and `ResponseEvent`:

```go
type AgentContext struct {
    AgentID          string `json:"agent_id,omitempty"`           // gen_ai.agent.id
    AgentName        string `json:"agent_name,omitempty"`         // gen_ai.agent.name
    AgentVersion     string `json:"agent_version,omitempty"`      // gen_ai.agent.version
    ConversationID   string `json:"conversation_id,omitempty"`    // gen_ai.conversation.id
    FlowID           string `json:"flow_id,omitempty"`            // run/trace
    StepID           string `json:"step_id,omitempty"`            // span
    ParentStepID     string `json:"parent_step_id,omitempty"`
    FlowStepSeq      int    `json:"flow_step_seq,omitempty"`      // 1-based order within flow
    IdentitySource   string `json:"identity_source,omitempty"`    // header|trace|inferred
    IdentityVerified bool   `json:"agent_identity_verified,omitempty"` // future: credential-bound
}
```

**Emitter promotion** (`emitter.go`): promote `agent_id`, `agent_name`,
`flow_id`, `conversation_id`, `identity_source` as top-level Loki fields so
the dashboard can `| json | agent_id="..."` / group `by (agent_id)` /
`by (flow_id)` — mirrors how `workflow` is already promoted.

## Work breakdown (per repo)

1. **tas-llm-router (gateway)** — header parsing + `traceparent` decode +
   flow/step derivation + inference fallback + `AgentContext` on events +
   emitter field promotion. The core.
2. **tas-agent-builder** — start sending `TAS-Auth` (needs a service
   account/token so its calls enter AIQG Path A at all) **plus** the new
   headers mapped from `Agent.ID`→`TAS-Agent-Id`, `Agent.Name`→
   `TAS-Agent-Name`, `AgentExecution.ID`→`TAS-Flow-Id`,
   `AgentExecution.SessionID`→`TAS-Conversation-Id`, and a per-call
   `TAS-Flow-Step` seq. Without this, agent-builder traffic stays invisible.
3. **aiqg-dashboard-be** — new aggregation: `GET /api/v1/metrics/agents`
   (per-agent rollup: runs, steps, cost, avoidable, avg CLEAR) and
   `GET /api/v1/flows` + `GET /api/v1/flows/{flow_id}` (flow list + step
   tree drill-down). Loki group-by `agent_id`/`flow_id`; TimescaleDB
   `scope_type=agent` rollups for the fast path.
4. **aiqg-ui** — "Traffic by Agent" view (table/cards per agent) and a
   **flow drill-down** showing the step tree (parent/child) with per-step
   workflow_type, cost, latency, CLEAR, and tags — the agentic equivalent
   of a trace waterfall.
5. **tas-spark-jobs / TimescaleDB** — add `scope_type ∈ {agent, flow}`
   rollups to `aiqg.metrics_*` so per-agent dashboards don't hit Loki.
6. **aether-shared/data-models/aiqg/** — new `agent-context.md` model doc +
   updates to `request-event.md` / `response-event.md` / `inferred-labels.md`
   (cross-repo PR lands together per repo convention).
7. **cmd/demo-traffic** — extend the generator to stamp `agent_id`,
   `agent_name`, `flow_id`, `conversation_id`, `step` so the new agent/flow
   views can be validated immediately against demo data (define a few demo
   agent personas, each running multi-step flows across workflow types).

## Phasing

- **Phase 1 — Gateway plumbing**: header contract + `AgentContext` +
  emitter promotion + inference fallback. Verifiable via demo-traffic
  (#7) and Loki group-by, with zero other-repo changes.
- **Phase 2 — Dashboard read path**: aiqg-dashboard-be endpoints +
  aiqg-ui agent view + flow drill-down. Validated against demo data.
- **Phase 3 — Real source**: instrument tas-agent-builder (service token +
  headers) so live agent traffic flows through with accurate identity.
- **Phase 4 — Scale + trust**: TimescaleDB/Spark per-agent rollups; then the
  authenticated-identity upgrade (AIP/IBCT or token-bound `agent_id`),
  gated on the **patent FTO** outcome.

## Open items

- **Patent FTO pass** (US 12,483,411 / 12,547,677 / 10,924,326 /
  11,785,115 and neighbors) before any billing use of per-agent metering.
- OTel GenAI conventions are **experimental** — pin the attribute names we
  adopt and revisit on each OTel semconv release.
- Decide whether `flow_id` defaults to W3C `trace_id` (OTel-native) or a
  TAS id when both `traceparent` and `TAS-Flow-Id` are present (proposed:
  explicit `TAS-Flow-Id` wins, else `traceparent`, else inferred).
```
