# AIQG — Patent Candidates: Scout & Autolearning

**Status:** Working analysis / counsel-ready draft — 2026-06-19.
Technical/strategic assessment, **not legal advice**; patentability and FTO
calls require a patent attorney.
**Inputs:** `aiqg-ui` Experiments (Scout panel), `aiqg-dashboard-be`
`/api/v1/metrics/by-vendor` + verdict/significance, `aether-shared/data-models/aiqg/experiment.md`,
[`CLAIMS_TRACEABILITY.md`](../../CLAIMS_TRACEABILITY.md) (claims C7/C9/C13).
**Family companions:** [`AIQG-PATENT-ANALYSIS.md`](./AIQG-PATENT-ANALYSIS.md),
[`AIQG-PROVISIONAL-BRIEF.md`](./AIQG-PROVISIONAL-BRIEF.md),
[`AIQG-FTO-CLAIM-READ.md`](./AIQG-FTO-CLAIM-READ.md)

## Summary

Two adjacent candidates that extend the attribution family into the
**experimentation + adaptation** layer:

- **Scout (shipped 2026-06-18)** — a traffic-mining experiment-prioritization
  engine whose distinguishing feature is a **pre-launch time-to-verdict
  estimate** computed from per-model cost/latency/quality variance observed in
  the gateway's own historical traffic.
- **Autolearning (design-only; the open loop in C9/C13)** — closing the
  feedback loop so that statistically-proven experiment verdicts and human/
  implicit feedback **automatically and safely adapt** routing, judge
  calibration, and guardrails, gated by the inferred identity ladder.

**Honest prior-art posture up front:** adaptive/bandit model routing and online
learning are a **crowded field** (RouteLLM, bandit model selection, A/B
auto-promotion). Neither candidate should be filed on the bare idea of "learn
to route." The defensible novelty in both is a **specific combination** with
AIQG-only primitives — the inferred identity ladder, CLEAR's decomposed scoring,
and judge-calibration coefficients — not the generic adaptation loop.

---

## Candidate S1 — Pre-launch time-to-verdict from observed per-model variance (Scout)

**Mechanism.** Before any experiment is launched, the system:
1. aggregates the gateway's historical traffic per `(vendor, model)` over a
   window, computing mean and **standard deviation** of cost, latency, and CLEAR
   quality (`/api/v1/metrics/by-vendor` returns `cost_stddev` / `latency_stddev`
   per vendor/model);
2. for a proposed incumbent→candidate swap at a default exposure (e.g. 50/50,
   5% of traffic), computes the **sample size to statistical significance**
   from those observed variances —
   `requests_to_significance ≈ (z / Δmean)² · (σ_a² + σ_b²)` — and converts it
   to a **time-to-verdict** at the model's observed request rate;
3. **surfaces the estimate before the operator commits** ("≈ N days", or
   "traffic too thin"), flagging amber when the projected verdict time exceeds a
   threshold (~90 days).

**Why potentially novel.** A/B power analysis is textbook; doing it *up front*
from **per-model variance harvested from the same live gateway traffic the
experiment will run on**, to triage which swaps are even worth testing, is the
differentiated step. The system answers "is this experiment feasible, and how
long will it cost you?" before a single live request is split — turning power
analysis from a post-hoc check into a **prioritization filter**.

**Status:** shipped (UI `ScoutPanel`, `deriveSuggestions()`; backend by-vendor
stddev + significance calc). Per-model variance integration finished via
aiqg-dashboard-be#66.

## Candidate S2 — Confounding-aware suggestion → auto-drafted controlled experiment (Scout)

**Mechanism.** Scout mines historical traffic for incumbent→candidate model
swaps that achieved **comparable quality at lower cost/latency**, ranks them by
ROI, and — crucially — **explicitly models the confounding**: historical numbers
are confounded because the two models served *different* requests, so Scout
presents each as a *hypothesis only* and **auto-drafts the controlled split
experiment** (variants, cohort, guardrails) that would actually prove it,
prefilled and one click from launch.

**Why potentially novel.** The combination of (a) automated traffic-mined
candidate discovery, (b) first-class confounding disclosure in the
recommendation, and (c) one-click materialization of the suggestion into a
properly-powered controlled experiment closes the
*observe → hypothesize → prove* loop inside one tool. Weaker than S1 on its own;
strongest claimed as a dependent combination with S1's time-to-verdict gate.

**Status:** shipped (suggestion engine + Runner draft prefill).

---

## Candidate A1 — Closed-loop verdict-to-routing promotion, gated by the identity ladder

**Mechanism (design-only).** When the experiment Runner reaches a verdict that a
candidate is **statistically non-inferior on quality and a strict win on
cost/latency** (existing z-test non-inferiority + objective-win logic), the
system **automatically promotes the candidate into routing policy** for the
specific cohort that was tested — but only for traffic whose **inferred identity
(agent cluster / flow type) and identity_confidence** match the experiment's
cohort. The learned route carries provenance (which verdict authorized it) and
auto-reverts if live guardrails (error/latency/cost deltas) breach.

**Why potentially novel.** Auto-promotion of A/B winners exists. The
differentiator is **gating the learned routing decision on the zero-cooperation
inferred-identity ladder** from the attribution family — the system applies a
learned route to *un-instrumented* callers it recognizes by fingerprint cluster,
not by a self-asserted tag, with confidence-weighted rollout. This ties A1
directly to provisional Candidates 3–4 and is hard to replicate without that
attribution layer.

**Status:** design-only. Today: verdicts render to a human (C7 shipped); policy
is static observe-only (C9); no promotion path exists.

## Candidate A2 — Feedback-recalibrated scoring (calibration coefficients as live correction, not display)

**Mechanism (design-only).** AIQG already ingests explicit/implicit feedback
(thumb, accept_reject, rating, task_success, reward, edit_distance) keyed to
response events, and already computes **judge-calibration metrics** (agreement,
bias, MAE) between the LLM-judge's CLEAR-efficacy score and human outcomes —
**but only displays them**. A2 turns those metrics into **live per-(agent
cluster, workflow_type) correction coefficients**: the measured judge bias/MAE
recalibrates future CLEAR efficacy/assurance scores, and contradicting feedback
**suppresses repeat false-positive flags**. The calibration card becomes a
control input, not a readout.

**Why potentially novel.** Closing the judge → calibration → score-correction
loop **scoped per inferred agent cluster** (so a judge that's systematically
harsh on one agent type is corrected only there) is the differentiated piece.
Generic RLHF / judge-calibration is prior art; **per-cluster online correction
driven by the inferred-identity scoping** is the AIQG-specific combination.

**Status:** design-only. Feedback ingest + calibration compute shipped (C13
"open loop"); no consumer applies them.

## Candidate A3 — Adaptive guardrails + autonomous next-experiment proposal

**Mechanism (design-only).** Two pieces: (i) replace today's **hardcoded**
auto-stop guardrail thresholds (error delta, latency factor, cost factor) with
**thresholds learned per agent cluster** from that cluster's observed variance
(the same σ Scout already computes); (ii) feed verdict outcomes + Scout
suggestions into a loop that **proposes the next experiment automatically**
(autonomous experimentation) within a tenant-set risk budget, never
auto-launching without the A1 promotion gate.

**Why potentially novel.** The combination of variance-derived per-cluster
guardrails with Scout's time-to-verdict feasibility filter to drive an
auto-proposed experiment queue. Lowest-priority of the three; most exposed to
bandit/auto-experimentation prior art. Likely better as a **dependent claim**
on A1/S1 than a standalone.

**Status:** design-only.

---

## What weakens or gates the position

- **Prior-art density (autolearning).** Adaptive model routing, multi-armed
  bandits over models, and automated A/B promotion are heavily published and
  patented. A1–A3 must be claimed as **combinations with the inferred-identity
  ladder + per-cluster CLEAR calibration**, never as generic adaptive routing. A
  dedicated prior-art / FTO search on *bandit model routing* and *automated
  experiment promotion* is required before filing A-series.
- **Scout S1 is the stronger, cleaner candidate** — it is shipped (reduction to
  practice exists), and "up-front time-to-verdict from same-traffic per-model
  variance" is a narrower, more defensible step than the A-series.
- **FTO not yet read for these.** The existing claim-read
  ([`AIQG-FTO-CLAIM-READ.md`](./AIQG-FTO-CLAIM-READ.md)) covered the attribution
  techniques, not experimentation/adaptation. The four patents there (esp.
  Dynatrace 10,924,326 on metric attribution) do **not** obviously read on
  Scout/autolearning, but a fresh search is owed.
- **Autolearning is unbuilt.** A provisional can cover it with a sufficiently
  enabling description, but value is far higher once A2's feedback→score loop is
  prototyped against `cmd/demo-traffic` (reduction to practice).

## Recommended next steps

1. **Fold Scout S1 into the main provisional** (with Candidates 1–4) — it is
   shipped, narrow, and shares the gateway/CLEAR substrate. S2 as a dependent.
2. **Hold the A-series (autolearning) for a second provisional** after (a) a
   bandit/auto-experimentation prior-art search and (b) a prototype of A2's
   feedback→score-correction loop. Claim them only as combinations with the
   inferred-identity ladder.
3. **Prototype A2 first** — it is the most defensible (per-cluster judge
   recalibration), reuses already-shipped feedback + calibration data, and
   produces the reduction-to-practice artifact that lifts the A-series from idea
   to filing-ready.
4. **Disclosure hygiene:** keep the autolearning design non-public until the
   prior-art search settles provisional-vs-defensive-publication for the
   A-series (same statutory-bar guard as the attribution family).
