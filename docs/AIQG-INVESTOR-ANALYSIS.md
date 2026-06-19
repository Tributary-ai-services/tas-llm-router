# AIQG — Investor Positioning Analysis

**Status:** Working analysis — 2026-06-12; **IP section revised 2026-06-19** to
reflect the completed FTO claim-read and the six-candidate provisional brief.
**Inputs:** [`AIQG-EXTENSION.md`](./AIQG-EXTENSION.md),
[`AIQG-AGENT-FLOW-ATTRIBUTION.md`](./AIQG-AGENT-FLOW-ATTRIBUTION.md),
[`AIQG-AGENT-IDENTITY-RESEARCH.md`](./AIQG-AGENT-IDENTITY-RESEARCH.md),
[`AIQG-EXPERIMENTS-RUNNER.md`](./AIQG-EXPERIMENTS-RUNNER.md).
**Companions:** [`AIQG-PATENT-ANALYSIS.md`](./AIQG-PATENT-ANALYSIS.md),
[`AIQG-PROVISIONAL-BRIEF.md`](./AIQG-PROVISIONAL-BRIEF.md) (Candidates 1–6),
[`AIQG-FTO-CLAIM-READ.md`](./AIQG-FTO-CLAIM-READ.md),
[`AIQG-PATENT-SCOUT-AUTOLEARNING.md`](./AIQG-PATENT-SCOUT-AUTOLEARNING.md),
[`AIQG-PATENT-ML-IN-LOOP.md`](./AIQG-PATENT-ML-IN-LOOP.md)

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
again needs no app changes. **Scout** sharpens this: it mines the customer's own
traffic for the cheaper-model swaps worth testing and estimates the
**time-to-verdict before launch** ("this swap would take ≈3 weeks to prove at 5%
exposure"), so the optimization loop opens with a prioritized, feasibility-sized
list rather than a blank experiment form.

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
- **IP option, now de-risked**: the defensibility story is no longer
  hypothetical. A **six-candidate provisional** is drafted
  (`AIQG-PROVISIONAL-BRIEF.md`) — the zero-cooperation attribution tier
  (content-propagation graph, tool-call echo, graded identity ladder,
  fingerprint registry) plus identity-ladder-keyed sticky experimentation and
  Scout's pre-launch time-to-verdict — and the **FTO claim-read is done**
  (`AIQG-FTO-CLAIM-READ.md`): no high-risk read against the four located
  patents, and the strongest candidate (content-propagation graph) is clear of
  all four. A second filing track (autolearning + SLM/ML-in-loop, e.g.
  self-labeling a classifier from the protocol's forced echo) is scoped behind a
  bandit-routing prior-art search.

## Risks an investor will probe (be ready)

| Risk | Honest counter |
|---|---|
| FTO overhang on metering/billing (4 located USPTO patents) | **Claim-read now done** (`AIQG-FTO-CLAIM-READ.md`): no high-risk read on any of our six techniques. Only Dynatrace US 10,924,326 is a medium on per-agent metering — gates *billing-grade* use, not observability, and our inference-only (no injected correlation id) design is the documented design-around. Counsel opinion on that one patent is the remaining cheap step |
| Inference precision/recall unproven | The validation harness with known ground truth (`cmd/demo-traffic --untagged`) is already built; deterministic tiers set a high floor |
| OTel GenAI conventions are experimental | Names pinned; additive schema means renames are migrations, not breakage |
| Hyperscalers bundle basic gateway attribution | They'll ship header-based (self-asserted) attribution; the inferred tier + registry is the differentiated layer |
| Crowded category | The wedge is adoption cost, not feature count — zero-code-change attribution is structurally hard for SDK-first incumbents to retrofit |

## Concrete next steps

1. **Run the untagged validation** and publish precision/recall per inference
   tier internally — the single best artifact for the pitch deck, and the
   reduction-to-practice exhibit for the provisional.
2. **File the six-candidate provisional** (`AIQG-PROVISIONAL-BRIEF.md`) before
   any public disclosure; commission the counsel opinion on Dynatrace
   10,924,326 ahead of any billing-grade metering. *(FTO claim-read — done.)*
3. **Prototype one ML-in-loop claim** for the second track — the self-labeling
   classifier (M1) or feedback-recalibrated SLM judge (M2) — to turn the
   autolearning story from idea into a demonstrable, defensible asset.
4. Keep the inferred-attribution, autolearning, and ML-in-loop designs
   non-public until each provisional is on file.
