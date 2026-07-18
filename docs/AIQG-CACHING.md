# AIQG — Response Caching (Design)

Status: **C1–C3 IMPLEMENTED** — `pkg/aiqg/responsecache` + the lookup/store wiring
in `internal/server` (`maybeServeFromCache` / `maybeStoreInCache`). Default off;
enable per deployment with `AIQG_RESPONSE_CACHE_ENABLED=true`. **C2** (accounting/UI)
ships the savings metric + hit/miss split across `aiqg-dashboard-be` + `aiqg-ui`.
**C3** (experiment integration): bypass-by-default is on; caching *within* an
experiment is opt-in via `AIQG_RESPONSE_CACHE_IN_EXPERIMENTS=true` (variant folded
into the key), and the experiment results query excludes cache hits. Only **C4**
(semantic, `AIQG-SEMANTIC-CACHING.md`) remains design-only. Gateway feature
(`tas-llm-router`). Related: `AIQG-EXTENSION.md` (already reserves a
`cache_state` event dimension), `AIQG-EXPERIMENTS-RUNNER.md` (cache must be
experiment-aware), `AIQG-AGENT-FLOW-ATTRIBUTION.md`, `account.md`
(payload_retention_mode), `token-accounting.md` (vendor prompt-cache tokens).

> **What C1 ships (2026-07-17):** tenant-namespaced exact-match cache keyed on
> `hash(tenant + vendor + model + post-redaction messages + output-affecting
> params + scoring_version)` (§3); default-safe eligibility — deterministic-only,
> never streaming / tools / experiment-claimed (§1, §4, §7); `TAS-Cache: off|bypass`
> header (§4); Redis store (reuses the AIQG client) with an in-memory fallback,
> per-entry TTL, `max_body_bytes`, LRU + `PurgeTenant` for right-to-be-forgotten
> (§5, §8); `cache_state` + `cache_key_hash` on every event (§6). A hit skips the
> vendor call and does **not** stamp token usage, so cost/latency accounting reads
> ~0. **Deferred to C2+:** the savings metric and hit/miss dashboard split, route-
> level cache profiles (v1 is one global profile), and experiment-keyed entries
> (v1 bypasses experiment-claimed requests rather than partitioning them).

---

## 1. Problem & goal

Cloudflare-style LLM **response caching**: serve a previously-computed response
for an equivalent request instead of calling the vendor — cutting cost and
latency. We have none today. This directly amplifies the AIQG cost/token-
refinement thesis: **a cache hit is the ultimate token reduction (100%).**

But ours is **not** a copy of a generic cache — it has to be aware of the three
things that make AIQG different, or it will produce wrong dashboards, leak data,
or confound experiments:
- **Scoring-aware** — a hit has ~0 vendor cost and ~0 latency; it must not skew
  CLEAR / cost / latency unless explicitly attributed as "saved."
- **Gatekeeper-aware** — cache *after* inbound scan (never cache raw PII); a hit
  must still be safe on the outbound path.
- **Experiment-aware** — a hit bypasses variant routing, so cached traffic must
  be excluded from (or partitioned in) experiment measurement.

### Scope (v1) / non-goals
- v1 = **exact-match** cache for chat-completions. Semantic (embedding) cache is
  v2 (§9).
- **Never cache**: streaming responses (v1), tool/function calls (side-effecting),
  `temperature`-high / explicitly-non-deterministic requests unless opted in,
  any request whose tenant retention mode forbids storing bodies (§5).
- Distinct from existing caches: this is **response** caching, *not* the Redis
  token/bundle **resolution** cache, nor vendor-side **prompt-token** caching
  (token-accounting's `cached` tokens).

---

## 2. Where it sits in the request path

```
handleAIQG → resolve token → inbound Gatekeeper scan (+ redact/tokenize)
           → resolveExperiment (if any)                         ← cache BYPASS when experiment-claimed (§7)
           → CACHE LOOKUP  (post-scan key)                      ← NEW
                hit  → return cached response, stamp cache_state=hit, skip vendor
                miss → Router.Route → vendor → outbound Gatekeeper scan
                       → CACHE STORE (post-outbound-scan response)
           → events.Build stamps cache_state + cache_key_hash
```

Cache sits **after inbound scan** (key is built on the redacted/tokenized
prompt, so we never key on or store raw PII) and **stores the post-outbound-scan
response** (so a hit is already safe to return). It's checked **after** the
experiment resolver so experiment-claimed requests can bypass it.

> 🚨 **Prerequisite, verified 2026-07-16: the redaction this depends on is not
> wired.** `tas-llm-router` scans and then **blocks or reports — it never
> modifies prompt content**. The inbound handler never rewrites `req.Messages`
> (`server.go:641-679`); the pipeline's only content-mutating path is a
> Databunker tokenizer that is nil and disabled (`gatekeeper.go:149`,
> `default_processor.go:184`). **So `post-scan == pre-scan`, and C1 as written
> would store raw PII bodies at rest in Redis while claiming it does not** — the
> §5/§11 privacy posture is unmet, not merely imperfect.
>
> Wiring redaction (`ScanWithRedaction` + the deterministic `RedactionEngine`
> already exist in `pkg/scan`) is a **C1 precondition**, not a C1 detail.
> **→ Designed in [`AIQG-GATEKEEPER-INTEGRATION.md`](AIQG-GATEKEEPER-INTEGRATION.md)**,
> stage **G1** — no new infrastructure required. Note redaction is **lossy** and
> so is per-route/default-off, not a global switch; the utility-preserving
> tokenize round-trip (G4) is gated on Databunker, which is **not deployed**.
> Full chain: [`AIQG-PROMPT-CACHE-CONTROL.md`](AIQG-PROMPT-CACHE-CONTROL.md)
> §11.1; semantic-tier consequences:
> [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md) §4.1.1.

---

## 3. Cache key (exact-match, v1)

```
key = hash( tenant_id              # HARD isolation boundary — never cross-tenant
          + vendor + model
          + normalized_messages    # post-inbound-scan (redacted) message array
          + output-affecting params # temperature, top_p, max_tokens, stop, seed,
                                    #   response_format, tool definitions, system prompt
          + scoring_version )      # bust on scorer change so cached CLEAR stays valid
```

- **Tenant isolation is the cardinal rule** — `tenant_id` is always in the key;
  a leak across tenants is the worst failure mode. Keys are also Redis-namespaced
  `aiqg:cache:{tenant_id}:{hash}`.
- **Normalization**: trim/collapse whitespace, stable JSON key ordering, exclude
  non-output-affecting fields (user id, request id) — so equivalent prompts hit.
- **`scoring_version` in the key** so a CLEAR scorer bump doesn't serve stale
  scores with a cached body.

---

## 4. Determinism & cache profiles (opt-in, per route/workflow)

Caching a `temperature>0` response means serving one sampled answer to repeated
prompts — fine for some workflows, wrong for others. So caching is **opt-in and
configured**, not global:

- A **cache profile** is set per [[route-rule]] / `workflow_type` (reuses the
  policy-as-config model): `{ enabled, ttl, require_deterministic, max_body_bytes }`.
- Default-safe: `require_deterministic=true` caches only when `temperature==0`
  (or `seed` set). `single_turn_qa` / `rag` / `classification_extraction` are
  good fits; `code_generation` (often deterministic) opt-in; creative/agentic
  default off.
- Per-request override header **`TAS-Cache: bypass | off`** (force a fresh call),
  mirroring Cloudflare's cache control. Stripped before the vendor.

---

## 5. Privacy & retention reconciliation (must-resolve)

Caching stores request+response bodies — so it's governed by the same rules as
[[account]].`payload_retention_mode`:

- `retention=off` (no bodies stored): **either disable response caching** for
  that tenant, **or** restrict to a content-free mode (store only the response
  keyed by hash, with a short TTL, treated as transient) — operator choice,
  default = cache disabled when retention is off.
- Cached bodies live in Redis with a **TTL ≤ the profile TTL** and are
  tenant-namespaced; they inherit the tenant's PII posture (already redacted at
  key time). Right-to-be-forgotten / tenant purge must also flush the tenant's
  cache namespace.
- Never cache when an inbound finding triggered a **block** (no response to
  cache) or when policy marks the request non-cacheable.

---

## 6. Scoring-aware accounting (`cache_state`)

`AIQG-EXTENSION.md` already reserves a `cache_state` label — populate it:

- Every event stamps `cache_state ∈ {hit, miss, bypass}` + `cache_key_hash`
  (promoted to Loki/TS like the identity fields).
- A **hit**: `total_cost_usd ≈ 0` (vendor not called), `end_to_end_ms` =
  gateway-only; CLEAR for a hit is the **cached** score (carried with the entry),
  not recomputed.
- Dashboards must let the operator **include or exclude** cache hits (default:
  show both with a hit/miss split), so "true vendor cost/latency" and
  "as-served cost/latency" are both visible.
- **Cache savings** become a first-class metric — $ and tokens avoided by hits —
  feeding the Cost report / avoidable-cost story (the CFO value prop).

---

## 7. Experiment-aware (interaction with the runner)

A cache hit wasn't produced by the assigned variant, so it would confound A/B
results. Rules (see `AIQG-EXPERIMENTS-RUNNER.md`):

- **Default: experiment-claimed requests bypass the cache** (`cache_state=bypass`),
  so each variant's measurement reflects real variant calls.
- If caching *within* an experiment is desired, the **variant override is part of
  the cache key** (variant A and B never share an entry) AND hits are tracked in
  a separate bucket excluded from the per-variant verdict.
- The Experiments runner's guardrail/results queries filter `cache_state=miss`
  for latency/cost so cache hits don't mask a variant regression.

---

## 8. Controls, infra, metrics

- **Infra**: Redis (already deployed — `build-vs-reuse.md` "reuse Gatekeeper
  attestation cache"); tenant-namespaced keys, TTL, `max_body_bytes`, LRU
  eviction, a global + per-tenant size cap.
- **Config**: cache profiles on route-rules (staged like everything else:
  draft→enforce); `TAS-Cache` request header; account-level enable + retention
  gate.
- **Metrics / UI**: hit rate, $ saved, latency saved, by workflow/vendor/route —
  a **Cache** panel (Analysis) and a line in the Cost report. `cache_state`
  filter in Traffic Explorer.

---

## 9. Semantic cache (v2)

**→ Fully specified in [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md)**
(supersedes this section; closes issue #97). Summary of what that design concluded:

- Embedding-based near-match (serve a cached answer for a *similar* prompt).
  Needs an embedding model + vector store + a similarity threshold — **and a
  verification gate, which is the part this section originally missed.**
- **A conservative threshold is not sufficient.** Measured false-hit rates floor at
  3–13% for threshold-only designs, and pairs like *"Chase Sapphire"* vs *"Chase
  Sapphire **Reserve**"* sit at **cosine 0.99** — above any shippable threshold.
  The design is therefore a **cascade** (exact → candidate generation →
  verification → async judge), not a lookup. The threshold is candidate
  generation, not the decision.
- **Blocked on infra**: the deployed `redis:7-alpine` (7.4.7) has **no modules and
  no vector index** (`FT.CREATE` doesn't exist), and pgvector is **unavailable** in
  the stock `postgres:15-alpine`. Semantic caching needs a **Redis 8 upgrade** (or
  pgvector) before any code — and Redis 8's AGPLv3 arm needs legal sign-off.
- **The economics are a latency story, not a cost story**: at a safe threshold
  (~0.97 ⇒ 5–10% hit rate) the net saving is ~$6–12/month per $200 of spend. The
  hit rate that makes the cost case is where 7–15% of hits are wrong.
- **Still defer until exact-match v1 (C1) is proven.** C1 is also the L0 tier of
  the cascade, so it is a hard dependency, not just sequencing.

---

## 10. Phasing

- **C1 — exact-match v1**: cache profile config + post-scan key + Redis store +
  `cache_state`/`cache_key_hash` on events + `TAS-Cache` header + retention gate.
  Default safe (deterministic-only, opt-in per route).
- **C2 — accounting & UI**: cache-savings metric, hit/miss dashboard split,
  Traffic Explorer filter, Cost-report line.
- **C3 — experiment integration**: bypass-by-default + keyed-by-variant option.
- **C4 — semantic cache** (v2).

---

## 11. Risks
- **Cross-tenant leakage** — the cardinal sin; tenant_id always in key + namespace.
- **Stale / wrong answers** — TTL + determinism gate + per-workflow opt-in.
- **Privacy/retention** — reconciled in §5; default-disable when retention=off.
- **Scoring distortion** — `cache_state` split + savings as an explicit metric.
- **Experiment confounding** — bypass-by-default (§7).
- **Non-idempotent traffic** — never cache tool/function-calling or streaming (v1).

## 12. Work breakdown
1. **tas-llm-router** — cache lookup/store in the AIQG path (post-inbound-scan,
   pre/post outbound-scan), key builder, `TAS-Cache` header, `cache_state` +
   `cache_key_hash` on events + emitter promotion, Redis client reuse.
2. **aiqg-dashboard-be** — cache-savings aggregation + `cache_state` in
   `/events` + `/metrics`; Cache panel data.
3. **aiqg-ui** — Cache panel (Analysis) + Traffic Explorer `cache_state` filter +
   Cost-report savings line.
4. **aether-shared/data-models/aiqg/** — new `response-cache.md`; update
   `response-event.md` / `aggregated-metrics.md` for `cache_state`.
