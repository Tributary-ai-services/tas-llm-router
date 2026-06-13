# AIQG — Patent Position Analysis

**Status:** Working analysis — 2026-06-12. Technical/strategic assessment, **not
legal advice**; patentability and FTO calls require a patent attorney.
**Inputs:** [`AIQG-EXTENSION.md`](./AIQG-EXTENSION.md),
[`AIQG-AGENT-FLOW-ATTRIBUTION.md`](./AIQG-AGENT-FLOW-ATTRIBUTION.md),
[`AIQG-AGENT-IDENTITY-RESEARCH.md`](./AIQG-AGENT-IDENTITY-RESEARCH.md),
[`AIQG-EXPERIMENTS-RUNNER.md`](./AIQG-EXPERIMENTS-RUNNER.md).
**Companion:** [`AIQG-INVESTOR-ANALYSIS.md`](./AIQG-INVESTOR-ANALYSIS.md)

## Summary

The genuinely novel asset is the **zero-cooperation inferred attribution
layer** — the gateway recognizing its own served output echoing back in later
requests to reconstruct agent flows deterministically, plus the
fingerprint-ensemble agent registry built on top of it. The prior-art research
(21 verified claims, 22 sources; see `AIQG-AGENT-IDENTITY-RESEARCH.md`) found
nobody doing this. The CLEAR scoring, header taxonomy, and experiments work is
well-executed but has visible neighbors in the market.

## Patent candidates, in order of strength

### 1. Content-propagation graph (attribution doc §A.3) — file on this

Rabin-fingerprint chunking of served responses into a TTL'd index, then
matching incoming request content against it to build data-flow edges between
flows — catching planner→worker handoffs across *different processes, models,
and toolsets* where no header scheme can propagate anything.

- The WAN-dedupe analogy (Bluecoat/Riverbed byte caching) is prior art in a
  different field; applying it to *agent attribution in an LLM gateway*, with
  IDF suppression of boilerplate, is a new use.
- The non-obvious insight: the stateless chat-completions protocol
  **guarantees** the echo rather than making it statistically likely.
- Absent from all surveyed prior art (OTel GenAI, LangSmith, Langfuse,
  Helicone, AIP/A2A).

### 2. Deterministic flow linkage via vendor-minted token echo (§A.1)

Indexing every served `tool_call_id` and treating its reappearance as proof of
flow parentage — "the client cannot fake having received `call_abc123` from
us" — yields full ReAct-loop reconstruction with zero cooperation. Simple once
stated (cuts both ways for obviousness), but no surveyed tool does it.

### 3. The graded identity ladder as a system claim

Not the individual tiers (self-asserted headers are Helicone; principal/IP are
routine) but the **combination**:

- 8-tier resolution ladder stamping `identity_source` + `identity_confidence`
  per event;
- the split-trust rule — asserted wins for *who*, linked wins for *shape*;
- **asserted-vs-inferred mismatch as an agent-impersonation signal** (§E).
  The inference tier auditing the cooperative tier is a genuinely novel
  security mechanism — nobody else can detect a client lying about which agent
  it is.

### 4. Fingerprint ensemble + version lineage + registry (§B–D)

The 13-signal catalog individually borrows known techniques (Drain template
mining, MinHash, process mining), but the composition has no surveyed
precedent:

- ensemble clustering scoped per `(tenant, principal)`;
- version lineage (MinHash Jaccard) so identity survives prompt iteration —
  the failure mode of naive prompt-hashing;
- semi-supervised calibration where cooperatively tagged traffic teaches the
  inferrer;
- humans label *clusters*, not flows ("14 agents detected, 3 named").

### 5. Identity-ladder-keyed sticky experimentation

Gateway-native A/B on live LLM traffic where assignment stickiness rides the
*inferred* identity ladder (so even un-instrumented callers get coherent
splits), with claim-on-match mutual exclusion and non-inferiority verdicts.
A/B testing is ancient; doing it at an LLM gateway keyed on inferred identity
— including the Gatekeeper-impact design (shadow-eval pairwise to prove the
security layer's rewrites don't degrade answers) — is a differentiated
combination.

## What weakens or gates the position

- **FTO is unresolved and already flagged**: US 12,483,411 / 12,547,677 /
  10,924,326 / 11,785,115 were located but never claim-read
  (`AIQG-AGENT-IDENTITY-RESEARCH.md` coverage gaps). Both design docs
  correctly gate billing-grade metering on this. Do the FTO claim-read before
  fundraising diligence surfaces it.
- **Prefix chaining (§A.2) is the weakest claim** — the attribution doc itself
  cites Anthropic/OpenAI prompt caching and vLLM prefix caching as prior art
  that prefix identity ≡ conversation identity. The *application* to
  attribution is new; the core mechanism isn't.
- **Timing.** Identity plumbing shipped 2026-06-11 (`aiqg-v5.24`); the
  inferred-attribution section is design-only. If the docs and shipped
  behavior stay non-public, no statutory bar has started — but a provisional
  on the inference cluster (candidates 1–4) is cheap and starts priority
  before anyone publishes the same idea.

## Recommended next steps

1. **Provisional filing** on the inference cluster (candidates 1–4) before any
   public disclosure. Lean provisional over defensive publication for the
   moat-bearing pieces; defensive publication only for what we decide not to
   claim.
2. **FTO claim-read** on the four located USPTO patents and neighbors —
   required before billing use of per-agent metering, and before diligence.
3. **Run the untagged validation** (`cmd/demo-traffic --untagged`) to get
   precision/recall per inference tier — the single best artifact for both a
   patent filing (reduction to practice) and investor diligence.
