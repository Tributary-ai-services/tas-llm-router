# AIQG — Experiments Runner (Design)

Status: **Design / for review** — NOT implemented. Gates a change to the
production gateway's routing of live customer LLM traffic; do not build
without sign-off on §7 (Guardrails) and §13 (Risks).
Owner: AIQG · Related: `AIQG-AGENT-FLOW-ATTRIBUTION.md` (identity ladder),
`aether-shared/data-models/aiqg/route-rule.md`, `aiqg-ui/docs/PERSONA-DASHBOARD-IA.md` §6.

---

## 1. Problem & goal

The shipped **Experiments comparison view** (`aiqg-ui /reports/experiments`)
compares two vendor/model configs *on traffic that already happened* — it
can't tell you how config B *would* perform on config A's traffic, because B
never ran it. Answering "is gpt-4o-mini good enough vs gpt-4o for our RAG
workflow, at what cost/quality?" requires actually **running a fraction of
live traffic on the variant and measuring it** — an online A/B test.

**Goal:** let an operator define an experiment (a cohort of traffic + two or
more variants + a traffic split), have the gateway route the split
deterministically and stickily, stamp each request with its variant, and
report per-variant CLEAR / cost / latency / success — with hard guardrails
and a kill switch, because this reroutes real customer traffic.

### Non-goals (v1)
- Automatic winner selection / auto-ramp (Phase 3).
- Full sequential/Bayesian statistics (v1 ships sample counts + a simple
  confidence note; see §6).
- Prompt/parameter experiments beyond vendor/model/policy-bundle (later).
- Multi-armed bandit allocation (later; v1 is fixed-weight split).

---

## 2. Prior art in TAS (what we build on)

- **Route rules** (`route-rule.md`, resolved in `internal/policy/resolver.go`)
  — bind a **policy bundle** to a request shape via matchers
  (`url_path`, `source_app`, `vendor`, `model`, `customer_header`),
  priority-ordered, first-match-wins, with a staged `mode`
  (`draft → dry_run → enforce → disabled`). Experiments reuse the **matcher
  shape** (the cohort) and the **staged-rollout discipline**, but target the
  *routing decision*, not a bundle.
- **Router** (`internal/routing/router.go` `Route()` → `determineStrategy()` →
  `routeByStrategy()`) — owns vendor/model selection. The experiment resolver
  produces a **variant override** the router honors instead of its default
  strategy for assigned requests.
- **Identity ladder** (`AIQG-AGENT-FLOW-ATTRIBUTION.md`, shipped) — gives a
  stable per-request assignment key: `baggage user.id` → `conversation.id` →
  `flow_id` → `principal` (token) → `client_ip`. **This is the keystone**:
  sticky assignment requires a stable identity, which we now have.
- **Events pipeline** (`pkg/aiqg/events`) — already promotes `vendor`, `model`,
  `workflow`, CLEAR scores, cost, status, identity to Loki. We add
  `experiment_id` + `variant` the same way; measurement is then the existing
  per-dimension aggregation grouped by variant.
- **Comparison view** (shipped) — becomes the *results* surface, fed by the
  live per-variant data instead of hand-picked vendor rows.

---

## 3. Core model

```
Experiment
  id, tenant_id, name, description, status
  cohort       — matcher set (which requests are eligible)
  variants[]   — { key, weight, override }   (key "control" = baseline routing)
  assignment   — { key_source, salt }        (how a request maps to a variant)
  guardrails   — { max_traffic_pct, stop_on, min_samples }   (§7)
  schedule     — { starts_at, ends_at }       (optional)
  created_by, created_at, transitions[]       (audit)

Variant.override   — { vendor?, model?, policy_bundle_id? }
                     control has an empty override (baseline strategy runs)
Cohort             — route-rule-style matchers + workflow_type (Phase-2 matcher
                     that Experiments needs from day one): url_path, source_app[],
                     model[], workflow_type[], customer_header
```

A request is **eligible** if it matches the cohort AND the experiment is
`running` AND within `schedule`. Eligible requests are assigned to exactly
one variant; everyone else routes normally (untouched).

---

## 4. Assignment (deterministic, sticky)

```
key      = first non-empty of [user_id, conversation_id, flow_id, principal_id, client_ip, request_id]
bucket   = crc32( experiment_id + ":" + salt + ":" + key ) % 10000     // 0.01% granularity
variant  = walk cumulative variant weights until bucket < cumulative
```

- **Deterministic + sticky**: the same user/conversation always lands in the
  same variant for the life of the experiment — critical so a user isn't
  bounced between models mid-conversation (jarring, and pollutes results).
  This is exactly why the identity ladder had to land first.
- **`key_source`** is configurable (default `conversation` so a multi-turn
  thread is coherent; `user` for cross-conversation stickiness; `request` for
  pure per-call randomization when no stable id exists).
- **Falls back down the ladder**: if no `user_id`, use `conversation_id`, etc.,
  ending at a per-request UUID (effectively random) — so an un-instrumented
  caller still gets a clean split, just not sticky.
- **Salt** lets two experiments on overlapping cohorts assign independently
  (no correlation between experiments).
- **Bucketing respects `max_traffic_pct`**: only the first N% of buckets are
  eligible; the rest force `control` regardless of variant weights, so a
  50/50 experiment capped at 10% traffic = 5% A / 5% B / 90% untouched.

---

## 5. Gateway integration

A new **experiment resolver** in `tas-llm-router`, modeled on the policy
resolver (per-tenant cache, loaded from aiqg-dashboard-be on miss):

```
handleAIQG → resolve token → resolve policy bundle
           → resolveExperiment(ctx, req, identity)   ← NEW
                match cohort → assign variant → produce *VariantDecision
           → Router.Route(...) honors VariantDecision.override
                (vendor/model) instead of its default strategy
           → events.Build stamps experiment_id + variant on the event
```

- **Override application**: `Router.Route()` gains an optional
  `forcedVendor/forcedModel` (from the variant) that short-circuits
  `determineStrategy()` for assigned requests. Control variant → no override,
  baseline strategy runs unchanged.
- **Header overrides / opt-out**: `TAS-Experiment: off` on a request bypasses
  all experiment assignment (escape hatch for a caller). A future
  `TAS-Experiment-Variant: <key>` can pin a variant for testing (dry-run only).
- **Event stamping**: add `ExperimentID` + `Variant` to the `AgentContext`
  sibling (or a small `ExperimentContext`); promote `experiment_id` + `variant`
  to the Loki line (same mechanism as the identity fields). No other event
  shape changes.
- **Fail-safe**: any resolver error, cache miss, or panic → **no assignment**
  (route normally). Experiments can never break routing; they can only
  *narrow* to control. Mirrors the policy resolver's degrade-to-Default().
- **`policy_bundle` variant override** reuses the existing route/policy path —
  an experiment can also A/B a *policy bundle*, not just a model.

---

## 6. Measurement & significance

- Per-variant metrics come from the **existing events**, grouped by
  `experiment_id` + `variant`: request_count, success/error/blocked,
  avg/percentile latency, total + per-request cost, CLEAR composite + dims.
  New dashboard endpoint `GET /api/v1/experiments/{id}/results` runs the same
  Loki/TimescaleDB aggregations the metrics endpoints already use, grouped by
  the `variant` label.
- **v1 significance**: report per-variant **sample count** and a coarse
  confidence flag — "insufficient samples (<min_samples)", "directional", or
  "clear" (non-overlapping simple CIs on the headline metric). Explicitly
  *not* a p-value engine in v1; surface the numbers honestly and let a human
  call it. Full sequential testing is Phase 3.
- **Guardrail metrics** (§7) are computed on the same rollup on a short cadence
  so a regression trips fast.

---

## 7. Guardrails (the part that gates the build)

This reroutes **real customer LLM traffic** to a different model/vendor. Every
experiment MUST carry:

1. **`max_traffic_pct`** (default 5%, hard cap configurable per tenant) — the
   ceiling on eligible traffic; the rest is forced to control.
2. **Auto-stop (`stop_on`)** — the gateway/aggregator trips the experiment to
   `paused` (all traffic → control) when, on ≥`min_samples`:
   - variant error rate > control error rate + `error_delta` (default +2pp), or
   - variant p95 latency > control × `latency_factor` (default 1.5×), or
   - variant avg cost/request > control × `cost_factor` (default 2×).
3. **Kill switch** — a single tenant-level "halt all experiments" flag the
   gateway honors on its next cache refresh (and an explicit per-experiment
   `paused`). Cache TTL bounds worst-case propagation (target ≤30s).
4. **Sticky-by-conversation default** — never bounce a user mid-thread.
5. **Dry-run mode** (§8) before any real reroute — validate cohort sizing &
   assignment with zero routing change.
6. **Tenant isolation** — experiments are strictly tenant-scoped; no
   cross-tenant cohorts (same invariant as route-rules).
7. **Audit** — every transition (start/pause/edit/stop) + auto-stops recorded
   in `audit-log-entry`, with the tripping metric.
8. **Cost ceiling (optional)** — absolute `$/day` budget for the variant arm;
   exceed → auto-pause.

Default posture is conservative: small %, auto-stop armed, sticky, dry-run
first, kill switch always available.

---

## 8. Lifecycle (staged, mirrors route-rule `mode`)

```
draft     — editing; never matches traffic
dry_run   — gateway ASSIGNS variants + stamps experiment_id/variant on events,
            but does NOT apply the override (everyone still routes to control).
            Proves cohort sizing, assignment balance, and stickiness with ZERO
            routing change. The safe pre-flight.
running   — live split; overrides applied within max_traffic_pct; guardrails armed
paused    — all traffic → control (manual or auto-stop); results retained
completed — ended (schedule or operator); read-only results
archived  — audit trail only
```

`dry_run → running` is the only transition that changes production routing and
should require an explicit confirm in the UI (and is audited).

---

## 9. Data model

**aiqg-dashboard-be (PostgreSQL)** — new tables, additive:

```
aiqg.experiment            (id, tenant_id, name, description, status,
                            cohort jsonb, assignment jsonb, guardrails jsonb,
                            starts_at, ends_at, created_by, created_at, updated_at)
aiqg.experiment_variant    (id, experiment_id, key, weight, override jsonb)
aiqg.experiment_transition (id, experiment_id, from, to, actor, reason, at)
```

**Gateway** caches active (`running`/`dry_run`) experiments per tenant (same
pattern + TTL as the policy resolver cache). New Kafka topic
`tas.aiqg.experiment.changed.v1` (additive, matches the existing AIQG topic
convention) invalidates the cache on edit for sub-cache-TTL propagation.

Per-request results need no new storage — they ride the existing event stream
via the `experiment_id` + `variant` Loki/TimescaleDB labels. (Phase 4: a
`scope_type='variant'` rollup in `aiqg.metrics_*` for fast results, like the
agent/user rollups.)

---

## 10. API (aiqg-dashboard-be)

```
GET    /api/v1/experiments                 list (tenant-scoped)
POST   /api/v1/experiments                 create (draft)
GET    /api/v1/experiments/{id}            detail
PATCH  /api/v1/experiments/{id}            edit (draft/dry_run only for cohort/variants)
POST   /api/v1/experiments/{id}/transition { to: dry_run|running|paused|completed }
DELETE /api/v1/experiments/{id}            archive (soft)
GET    /api/v1/experiments/{id}/results    per-variant metrics + sample counts + confidence
GET    /internal/experiments?tenant=…      gateway cache-load endpoint (internal auth)
```

Validation: weights sum to 100; exactly one `control`; `max_traffic_pct` ≤
tenant ceiling; cohort matchers validated like route-rules; running/paused
edits restricted to guardrails + schedule (not cohort/variants — those need a
new experiment to keep results clean).

---

## 11. UI (aiqg-ui — Reports ▸ Experiments)

Extends the existing page (today: comparison) with:
- **List** of experiments (status chip, cohort summary, traffic %, variant count).
- **Create/edit wizard**: cohort (reuse route-rule matcher inputs + workflow
  type) → variants (model/vendor/bundle + weight) → assignment key → guardrails
  → schedule. `dry_run → running` is a guarded confirm.
- **Results**: the comparison table we already built, now per *variant* (not
  hand-picked vendor rows), + sample counts, confidence flag, live status,
  and a prominent **Pause** button. Auto-stop events shown inline.
- The current ad-hoc comparison stays as a "quick compare (historical)" tab.

---

## 12. Phasing

- **Phase 1 — shipped**: historical comparison view.
- **Phase 2a — config + dry-run**: data model, CRUD API, UI wizard, gateway
  experiment resolver in **dry_run only** (assign + stamp, no reroute). Proves
  cohorts/assignment/stickiness against live traffic with zero risk.
- **Phase 2b — live split**: enable the routing override + guardrails +
  auto-stop + kill switch + results endpoint/UI. The gated step.
- **Phase 3**: significance engine, auto-ramp, winner declaration, bandit
  allocation, `scope_type='variant'` rollups.

---

## 13. Risks & open questions

- **Production routing blast radius** — mitigated by max_traffic_pct, dry-run
  pre-flight, auto-stop, kill switch, fail-safe-to-control. Still the #1 risk;
  Phase 2b needs explicit sign-off.
- **Cost runaway** — a variant on a pricier model multiplies spend on its arm;
  cost guardrail + ceiling required.
- **Result validity** — confounding (time-of-day, cohort skew), insufficient
  samples; v1 is honest about sample size and avoids over-claiming
  significance.
- **Stickiness vs. coverage** — keying on `conversation` is coherent but a few
  heavy users can dominate a variant; surface per-variant unique-key counts so
  skew is visible.
- **Open: where the override lives** — extend `Router.Route()` with a forced
  vendor/model arg (cleanest) vs. a synthetic high-priority route rule. Leaning
  to the `Route()` arg since route-rules bind bundles, not models.
- **Open: patent FTO** — A/B *metering/attribution* overlaps the FTO items
  flagged in `AIQG-AGENT-IDENTITY-RESEARCH.md`; clear before any billing use of
  experiment results (observability use is fine).

---

## 14. Work breakdown (per repo, Phase 2)

1. **aiqg-dashboard-be** — experiment tables + CRUD API + `/results` +
   `/internal/experiments` cache-load + `tas.aiqg.experiment.changed.v1` emit.
2. **tas-llm-router** — experiment resolver + per-tenant cache + assignment
   (reuse identity ladder) + `Route()` override arg + `experiment_id`/`variant`
   event stamping + emitter promotion + guardrail auto-stop signal. The core,
   and the production-routing change — lands behind dry-run first.
3. **aiqg-ui** — experiments list + create/edit wizard + live results
   (per-variant comparison + pause).
4. **tas-spark-jobs / TimescaleDB** (Phase 4) — `scope_type='variant'` rollups.
5. **aether-shared/data-models/aiqg/** — new `experiment.md` model doc.
