# AIQG — Agent Identity & Per-Flow Attribution: Prior-Art Research

Reference findings that informed [`AIQG-AGENT-FLOW-ATTRIBUTION.md`](./AIQG-AGENT-FLOW-ATTRIBUTION.md).

- **Date:** 2026-06-09/10
- **Method:** multi-source deep-research — 5 search angles, 22 sources
  fetched, 105 claims extracted, top 25 verified via 3-vote adversarial
  checking (need 2/3 to refute). **21 confirmed, 4 killed.** 104 agents.
- **Question:** How do systems identify an AI agent and attribute LLM/API
  gateway traffic *per agent flow* — grouping many calls into the agent,
  the run/execution, the conversation/session, and the step?

> **Confidence note:** confirmed claims rest on primary sources (the specs
> and product docs themselves), so confidence reflects faithful reading of
> authoritative documentation, not independent cross-vendor benchmarking.

## Headline

A consistent **four-level layering** emerges everywhere — *agent →
conversation(session/thread) → run/flow → step* — but the **trust model
splits**: production observability tooling attributes traffic via
**self-asserted** ids the caller picks (headers/metadata), while emerging
agent-identity protocols make identity **cryptographically authenticated**.
That split is the central design choice for an LLM gateway: descriptive
(easy, today) vs. trustworthy (frontier, billing-grade).

## Confirmed findings by area

### Standards — OpenTelemetry GenAI semantic conventions (the canonical scheme)
Status: **experimental / "Development"** — names may change.

- **Agent:** `gen_ai.agent.id` ("unique identifier of the GenAI agent",
  e.g. `asst_5j66…`) + `gen_ai.agent.name` ("human-readable name … provided
  by the application", e.g. `Math Tutor`) + `gen_ai.agent.description` +
  `gen_ai.agent.version`.
- **Conversation/session/thread:** a **single** `gen_ai.conversation.id`
  — "The unique identifier for a conversation (session, thread)…" — *folds
  session and thread into one id*. There is **no** `gen_ai.session.id` or
  `gen_ai.thread.id`. (A generic cross-domain `session.id` exists outside
  the `gen_ai` namespace, not cross-referenced.)
- **Step:** an OTel **span**; agent operations are named via
  `gen_ai.operation.name` ∈ {`create_agent`, `invoke_agent`,
  `invoke_workflow`, `execute_tool`, `chat`, `embeddings`, `retrieval`, …},
  with span names like `invoke_agent {gen_ai.agent.name}`.
- **Run/flow & correlation:** ordinary trace/span parent-child structure
  (`trace_id` / `span_id` / `parent_span_id`).
- Sources: opentelemetry.io `/gen-ai/gen-ai-agent-spans/`,
  `/registry/attributes/gen-ai/`, `/gen-ai/gen-ai-spans/`.

### Industry tools — same layering, different names, mostly self-asserted

- **LangSmith (LangChain):** run-tree model — `run_id` = the span/step,
  `trace_id` = the root trace/run (equals the root run's own id),
  `parent_run_id` = tree linkage (`dotted_order` encodes hierarchy,
  `RunTree` auto-links via context). Session layer = **any one of three
  interchangeable** metadata keys: `session_id` / `conversation_id` /
  `thread_id`. Docs: docs.langchain.com/langsmith (export-traces,
  run-data-format).
- **Langfuse:** OTel-aligned ids — **32-hex trace ids, 16-hex observation
  ids** (W3C 16-byte/8-byte sizing). Session = free-form `sessionId`
  (US-ASCII, <200 chars, app-chosen; 1:n to traces; groups all observations
  + enclosing traces). Propagation: **SDK-managed context**
  (`propagate_attributes`), with an **opt-in `as_baggage`** flag to
  propagate via W3C Baggage HTTP headers cross-service. Docs:
  langfuse.com/docs/observability.
- **Helicone (the pure-header gateway model):** three **client-supplied
  HTTP headers** — `Helicone-Session-Id` (UUID recommended; groups all
  related requests), `Helicone-Session-Path` (STEP hierarchy via `/`
  parent-child tree, e.g. `/abstract/outline/lesson-1`),
  `Helicone-Session-Name`. **Any** request type (LLM, vector DB, tool call)
  sharing the same session headers is folded into one session trace.
  Purely value-based and self-asserted. Docs: docs.helicone.ai/features/sessions.

### Propagation mechanism
- Libraries: **SDK-managed trace context** (OTel context inheritance,
  Langfuse `propagate_attributes`, LangSmith `RunTree`).
- Gateways: **HTTP headers** (Helicone), optionally honoring **W3C
  `traceparent` / Baggage**. A gateway cannot ride the caller's SDK context,
  so headers are the practical mechanism.

### Agent-identity protocols — the authenticated frontier (diverges)
- **AIP (Agent Identity Protocol)** — two id schemes: DNS-based
  `aip:web:<domain>/<path>` (resolved at `https://<domain>/.well-known/aip/<path>.json`)
  and self-certifying `aip:key:ed25519:<multibase>` (the id *is* the public
  key). Identity documents **MUST** carry a `document_signature` (Ed25519
  over RFC 8785 canonical form) — "**not self-asserted — it is
  cryptographically authenticated**", tamper-evident even if the hosting
  domain is compromised. Correlation across protocols uses
  **Invocation-Bound Capability Tokens (IBCTs)** with transport bindings:
  `X-AIP-Token` HTTP header (MCP tool calls), `aip_token` task-metadata
  field (A2A), `Authorization: AIP <token>` (plain HTTP). A credential-bound
  correlation artifact, not a self-asserted id. Sources: arXiv 2603.24775,
  IETF `draft-prakash-aip-00`. *Status: 2026 preprint + early I-Ds, not
  ratified; adoption unproven.*
- Related (lower-confidence / blog): Google **A2A** protocol spec,
  **WSO2 AI Gateway "AgentID"** (gateway-level agent identity/auth).

## Killed claims (failed verification — do NOT rely on)
- ✗ "gen_ai.operation.name is a *required* attribute" (0-3) — it is
  conditionally required, not required.
- ✗ "Langfuse uses a four-part trace/observation/span/parent model" (0-3)
  — it is trace + observation (16-hex), not a separate span/parent split.
- ✗ "LangSmith grouping ids are metadata-only with no propagation
  mechanism" (0-3) — they propagate via the RunTree/context model.
- ✗ "GenAI correlation relies on no GenAI-specific propagation field; the
  spans page defines no propagation" (1-2, not killed but contested).

## Coverage gaps (absence of evidence ≠ evidence of absence)
- **Academic papers:** no paper-level correlation model survived
  verification — the scholarly landscape here is **unconfirmed**.
- **Other tools:** Arize Phoenix, Traceloop/OpenLLMetry, Datadog LLM
  Observability, Honeycomb, W&B Weave produced **no confirmed claims**.
- **W3C Trace Context/Baggage** appears only indirectly (via Langfuse
  sizing), not surveyed as a standalone propagation standard.
- **Patents:** **zero confirmed claims.** Four USPTO patents on gateway
  attribution/metering were *located* — **US 12,483,411 / 12,547,677 /
  10,924,326 / 11,785,115** — but their claims/assignees were not verified
  within budget. **Freedom-to-operate is unresolved** and owed before any
  billing use (see task: Patent FTO pass).

## Design implications (carried into the plan)
1. **Adopt OTel `gen_ai.*` names** rather than inventing TAS-specific field
   names — standards-aligned, OTel-export-ready. Pin names (experimental).
2. **Fold session+thread into one `conversation.id`** (matches OTel +
   LangSmith's interchangeable keys).
3. **Header-based propagation** for the gateway (Helicone-proven), honoring
   W3C `traceparent` so `flow_id = trace_id`, `step = span_id` when present.
4. **Ship self-asserted, design for authenticated.** Record
   `identity_source ∈ {header, trace, inferred}`; reserve
   `agent_identity_verified` + credential ref so AIP/IBCT, token-bound, or
   AgentID can slot in later without migration.
5. **Patent FTO** gates billing-grade per-agent metering.

## Source index (confirmed-claim sources)
- opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/
- opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/
- opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/
- opentelemetry.io/docs/concepts/context-propagation/
- docs.langchain.com/langsmith/export-traces, /run-data-format
- langfuse.com/docs/observability (trace-ids-and-distributed-tracing, sessions, data-model, sdk/instrumentation)
- docs.helicone.ai/features/sessions
- arxiv.org/html/2603.24775 + IETF draft-prakash-aip-00 (AIP)
- a2a-protocol.org/latest/specification/ (A2A)
- w3.org/TR/trace-context/ (W3C Trace Context)
- USPTO patents (located, claims unverified): 12483411, 12547677, 10924326, 11785115
