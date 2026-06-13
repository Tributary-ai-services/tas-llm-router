# AIQG — Investor Positioning Analysis

**Status:** Working analysis — 2026-06-12.
**Inputs:** [`AIQG-EXTENSION.md`](./AIQG-EXTENSION.md),
[`AIQG-AGENT-FLOW-ATTRIBUTION.md`](./AIQG-AGENT-FLOW-ATTRIBUTION.md),
[`AIQG-AGENT-IDENTITY-RESEARCH.md`](./AIQG-AGENT-IDENTITY-RESEARCH.md),
[`AIQG-EXPERIMENTS-RUNNER.md`](./AIQG-EXPERIMENTS-RUNNER.md).
**Companion:** [`AIQG-PATENT-ANALYSIS.md`](./AIQG-PATENT-ANALYSIS.md)

## Summary

The pitch is not any one feature — it's that the four pieces (extension /
attribution / identity / experiments) close a loop nobody else closes:
**attribute → score → experiment → optimize, all at the gateway, with zero
client code changes.**

## The wedge: agentless attribution in an instrumented market

The LLM gateway/observability category is crowded (Helicone, Langfuse,
LangSmith, Portkey, Kong/Cloudflare AI gateways) — but every competitor shares
one assumption AIQG breaks: **the customer must instrument their code**
(headers, SDKs, trace context). The prior-art research confirms this is
universal ("today's attribution is self-asserted everywhere").

AIQG's inferred tier means a customer points DNS at the gateway and gets
per-agent cost/quality attribution on day one with zero code changes. This is
the agentless-vs-agent-based monitoring story replayed for AI — and
historically the agentless entrant wins the land-grab because it removes the
adoption tax.

## Three investor-legible storylines

### 1. FinOps for AI agents

Per-agent and per-flow cost rollup, plus the avoidable-cost decomposition
(direct / induced / genuine waste) from the CLEAR extension. *"Which of our 40
agents is burning the budget, and how much of that spend is avoidable"* is a
CFO question nobody can currently answer without an instrumentation project.

The agent registry ("14 distinct agents detected, 11 unnamed") doubles as
**shadow-AI discovery**, which sells to the CISO in the same meeting — and the
asserted-vs-inferred impersonation cross-check gives security a reason to keep
the product after the cost story lands it.

### 2. The closed loop (measurement → action)

Competitors stop at dashboards. The Experiments Runner makes the measurement
actionable: detect that a workflow is over-provisioned on gpt-4o → run a
guardrailed, sticky, non-inferiority A/B against 4o-mini → quantified verdict.
That's Statsig/LaunchDarkly-for-LLM-routing, executed at the gateway so it
again needs no app changes.

The **Gatekeeper-impact experiment** is a unique proof-point: we can *show
customers data* that the security layer doesn't degrade answer quality — a
claim every competing AI firewall asserts and none can demonstrate.

### 3. Positioned for the authenticated-agent-identity wave

The schema ships self-asserted (matching the whole market) but reserves
`agent_identity_verified` + credential refs so AIP/IBCT, Google A2A, or
token-bound identity slots in without migration. Agent identity is an emerging
2026 standards fight; being the gateway that already records graded identity
confidence is a cheap option on that future. OTel `gen_ai.*` alignment also
de-risks the "proprietary schema lock-in" objection.

## Moat shape

- **Data network effect**: the fingerprint registry compounds — every labeled
  cluster and every framework signature (CrewAI / LangChain / AutoGen idioms)
  improves inference for all tenants; competitors can't shortcut it.
- **Deterministic floor**: the token-echo and prefix-chain tiers are
  proof-not-probability, so the headline capability holds even if fingerprint
  clustering underperforms.
- **IP option**: a provisional on the inference tier (see
  `AIQG-PATENT-ANALYSIS.md`) turns the wedge into a defensibility story beyond
  "we built a nice proxy."

## Risks an investor will probe (be ready)

| Risk | Honest counter |
|---|---|
| FTO overhang on metering/billing (4 located USPTO patents, claims unread) | Scoped: gates billing use only, not observability; FTO pass is planned and cheap relative to the raise |
| Inference precision/recall unproven | The validation harness with known ground truth (`cmd/demo-traffic --untagged`) is already built; deterministic tiers set a high floor |
| OTel GenAI conventions are experimental | Names pinned; additive schema means renames are migrations, not breakage |
| Hyperscalers bundle basic gateway attribution | They'll ship header-based (self-asserted) attribution; the inferred tier + registry is the differentiated layer |
| Crowded category | The wedge is adoption cost, not feature count — zero-code-change attribution is structurally hard for SDK-first incumbents to retrofit |

## Concrete next steps

1. **Run the untagged validation** and publish precision/recall per inference
   tier internally — the single best artifact for the pitch deck.
2. **FTO claim-read** before diligence surfaces it for us.
3. Keep the inferred-attribution design non-public until the
   provisional-vs-defensive-publication decision is made.
