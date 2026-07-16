# AIQG — Prompt-Cache Control (Design)

Status: **Design / for review** — NOT implemented. Gateway feature
(`tas-llm-router`). Independent of, and **higher-priority than**,
[`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md) — see §1.

Related: [`AIQG-CACHING.md`](AIQG-CACHING.md) (response caching — a *different*
mechanism), [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md) §12
(where this gap was found), `token-accounting.md` (vendor prompt-cache tokens —
already modelled), `tas-aiqg/AIQG_CACHE_SAFE_REDUCTION.md` (reduce-at-source),
`tas-aiqg/AIQG_CACHING_PRIMER.md` §2.

---

## 1. Problem

**`tas-llm-router` cannot enable vendor prompt caching.** Verified
(`AIQG-SEMANTIC-CACHING.md` §12.2):

- `ChatRequest` (`internal/types/requests.go:8`) is a typed OpenAI-shaped struct
  with **no unknown-field passthrough**. Go's `encoding/json` silently discards
  unknown fields, so a client's `cache_control` is **dropped without error**.
- `ContentPart` (`requests.go:51`) has no `cache_control` on any block type.
- The Anthropic provider never sets it — `anthropicReq.System =
  []anthropic.TextBlockParam{...}` (`internal/providers/anthropic/provider.go:445`)
  leaves `CacheControl` unset.
- There is **no `/v1/anthropic/*` raw-passthrough route** as an escape hatch.
- We nonetheless **read** cache-token usage back (`provider.go:193-200` →
  `responses.go:38`). We report a saving we can never request.

### 1.1 Why this outranks semantic caching

AIQG's thesis is three levers: prompt caching **cheapens**, semantic caching
**eliminates**, reduction **shrinks**. Prompt caching is the **broadest and
safest** of the three — it pays *without* depending on query repetition and has
**no wrong-answer mode** (exact prefix hash; arithmetic reuse). Semantic caching
is the narrow, risky one, worth ~$6–12/month per $200 spend at a safe threshold
(`AIQG-SEMANTIC-CACHING.md` §1.2).

**We are currently foreclosing the good lever while designing the risky one.**
Fix order should follow value: this first.

### 1.2 Why a header override, and not just passthrough

Passthrough alone assumes every origin gets `cache_control` right. **They don't**
— and the failure is **silent** (no error; `cache_creation_input_tokens: 0` and
you simply pay full price forever). The documented mistakes are exactly the ones
a caller makes and a gateway can see:

| Origin mistake | Why it kills caching | Gateway can fix? |
|---|---|---|
| No `cache_control` at all | Nothing cached | ✅ place it |
| Breakpoint after a timestamp / request ID | Prefix differs every request | ✅ place before the volatile tail |
| Breakpoint on the *whole* prompt incl. the varying question | Every request writes a distinct entry, nothing ever read | ✅ place at the stable/volatile boundary |
| Prefix below the model's minimum | Silently never caches | ✅ we know the model — skip or warn |
| >4 breakpoints | 400 | ✅ clamp |
| Long agentic turn >20 blocks | Lookback misses (§4.3) | ✅ intermediate breakpoints |

**The gateway is the right place to hold this knowledge.** It knows the model, the
render order, and the stable/volatile boundary; each origin re-deriving that is
how it gets got wrong N times. This is the same argument as policy-as-config: the
control plane knows, the callers shouldn't have to.

---

## 2. Scope

**In:** `cache_control` passthrough on our types; a per-request header; a per-route
default; gateway auto-placement; accounting of what was placed.

**Out (v1):** the 1h TTL (needs an SDK upgrade — §7); OpenAI (automatic, nothing to
place — §6); response caching and semantic caching (different mechanisms —
`AIQG-CACHING.md`).

---

## 3. Control surface

### 3.1 Three modes

| Mode | Behavior |
|---|---|
| **`auto`** | Gateway places breakpoints (§4). **Any client-supplied `cache_control` is ignored and replaced.** |
| **`passthrough`** | Honor exactly what the client sent; place nothing. For origins that know what they're doing. |
| **`off`** | Strip all `cache_control`; no caching. |

### 3.2 Per-route default + per-request header

Reuses the existing policy-as-config / route-rule model (the same shape as
`AIQG-CACHING.md` §4's cache profiles):

```yaml
prompt_cache:
  mode: passthrough        # auto | passthrough | off   — see §5 for the default debate
  ttl: 5m                  # 1h requires the SDK upgrade (§7)
  max_breakpoints: 4       # API hard limit
```

Per-request override header, matching the existing `TAS-*` convention
(`AIQG-CACHING.md` §4 already reserves `TAS-Cache` for **response** caching — this
is a **different** header for a **different** mechanism; do not conflate):

| Header | Effect |
|---|---|
| `TAS-Prompt-Cache: auto \| passthrough \| off` | Override the route's mode for this request |
| `TAS-Prompt-Cache-TTL: 5m \| 1h` | TTL (1h gated on §7) |

Stripped before the vendor call. **`off` must always be honored** — it is the
escape hatch for a caller who finds auto-placement wrong; never let a route
default override an explicit `off`.

---

## 4. Auto-placement

Render order is **`tools` → `system` → `messages`**; a change at any level
invalidates that level and everything after it. So placement follows one rule:

> **Put the breakpoint at the last byte that is stable across requests.**

### 4.1 Breakpoint 1 — end of system (the big, safe win)

A breakpoint on the **last system block** caches **tools + system together**
(tools render first). For TAS traffic this is the whole ballgame: aether-be's
agent prompts, agent-builder's generated personas, and the MCP tool definitions
are large, stable, and re-sent on every turn.

This one placement is safe in a way the others are not: system and tools are
*supposed* to be request-invariant. If they aren't, that's a bug worth surfacing
(§9).

### 4.2 Breakpoint 2 — end of the last complete turn (multi-turn)

For conversations, place on the **last content block of the most recently
appended turn**. Each subsequent request then reuses the whole prior
conversation. Earlier breakpoints stay valid as read points, so hits accrue
incrementally as the thread grows.

**Never** place after the current user question — that's the classic
shared-prefix mistake (every request writes a unique entry, nothing is read).

### 4.3 The 20-block lookback

A breakpoint walks back **at most 20 content blocks** to find a prior entry. A
single agentic turn can easily append more than 20 blocks (tool_use/tool_result
pairs), and then the next request's breakpoint **silently misses**. Auto-mode
inserts an intermediate breakpoint roughly every **15 blocks** within a long turn.

This is precisely the case an origin never handles, and it's the one that matters
most for TAS's agent traffic.

### 4.4 Budget

Max **4** breakpoints per request (API limit). Priority when we run out:
**system (§4.1) > last turn (§4.2) > lookback fillers (§4.3)**, and clamp. Never
emit a 5th — that's a 400.

### 4.5 Minimum cacheable prefix — model-dependent, silent

| Model | Minimum |
|---|---:|
| Opus 4.8 / 4.7 / 4.6 / 4.5, Haiku 4.5 | **4096** tokens |
| Fable 5, Sonnet 4.6, Haiku 3.5 / 3 | **2048** tokens |
| Sonnet 4.5 / 4.1 / 4, Sonnet 3.7 | **1024** tokens |

Below the minimum the API **silently does not cache** — no error, just
`cache_creation_input_tokens: 0`. It is also not billed, so placing a breakpoint
under the minimum is a harmless no-op. We should still **count** it (§8): a route
whose prefix never clears the bar is a route where someone believes caching is on
and it isn't.

> **A 3K-token system prompt caches on Sonnet 4.5 and silently won't on Opus 4.8.**
> The gateway knows the model; the origin usually doesn't think about it.

---

## 5. Economics — and the default-mode question

- **Read ≈ 0.1×** base input. **Write = 1.25×** (5m) / **2×** (1h).
- **Break-even: 2 requests at 5m** (1.25 + 0.1 = 1.35 vs 2.0 uncached); **3 at 1h**
  (2.0 + 0.2 = 2.2 vs 3.0).

So `auto` on **never-reused** prefixes **costs +25%** on the cached span. Auto-on
everywhere is not free, and "many origins get it wrong" cuts both ways — some of
them are single-shot.

**Recommended default: `passthrough`, and let measurement pick the routes.** Not
because auto is wrong, but because we can find out for free (§9) instead of
guessing — and an unmeasured +25% is exactly the kind of thing that discredits the
feature. Flip a route to `auto` when its measured prefix-reuse rate clears 2.

> This mirrors the S1-shadow discipline in `AIQG-SEMANTIC-CACHING.md` §15: measure
> the opportunity before enabling the mechanism. Here the measurement is nearly
> free, because we already collect it.

### 5.1 🚨 Router-specific: model routing destroys the cache

**Caches are model-scoped, and a model switch invalidates everything.** This
service is a *router* — `routing.strategy: cost-optimized` plus
`fallback_enabled: true` means the same conversation can land on a different model
turn-to-turn. Every such switch is a **full cache rebuild** (cold write at 1.25×,
zero reads).

This interaction is unique to us and is not in any vendor's guidance. Consequences:

- **Cost-optimized routing and prompt caching are in direct tension.** Routing to a
  marginally cheaper model can cost *more* once you count the discarded cache. Any
  future routing-cost model must price cache-state, not just per-token rates.
- **Auto-mode should prefer model-stickiness within a conversation** — we already
  have the conversation identity to do it with (§9).
- **A failover is a cache event**, and should be visible as one (§8).

Also (fan-out): a cache entry is readable only **after the first response begins
streaming**, so N parallel identical requests all pay full write price. Any
fan-out path should send one, await first token, then release the rest.

---

## 6. Provider matrix

| Provider | Mechanism | What `auto` does |
|---|---|---|
| **Anthropic** | Explicit `cache_control` breakpoints, 4 max, model-dependent minimum | Everything in §4 |
| **OpenAI** | Automatic caching — no `cache_control` to place | **No-op.** Report mode as `n/a`; never fail a request over it |

`auto` must be a **no-op, not an error**, on providers with nothing to place —
otherwise enabling a route breaks its OpenAI traffic. ⚠️ OpenAI's exact automatic
threshold/behavior is **not verified here** (§11 Q3) — don't quote numbers until
someone checks.

---

## 7. SDK constraints (verified against the pinned version)

We pin **`anthropic-sdk-go v1.7.0`** (`go.mod:10`). Latest is **v1.58.0** — a
51-version gap, so an upgrade is its own project, not a subtask.

**v1.7.0 is sufficient for v1:**

| Need | v1.7.0 | Note |
|---|---|---|
| `CacheControl` on system blocks | ✅ | `TextBlockParam.CacheControl` |
| `CacheControl` on tool definitions | ✅ | `ToolParam.CacheControl` |
| `CacheControl` on message content blocks | ✅ | 7 variants via `ContentBlockParamUnion.GetCacheControl()` |
| Read back cache tokens | ✅ | already wired (`provider.go:193-200`) |
| **1h TTL** | ❌ | `CacheControlEphemeralParam` has **only** `Type` — no `TTL` field |
| **Top-level auto-placement** | ❌ | no `CacheControl` on `MessageNewParams` |

So: **5-minute TTL only**, and **we place breakpoints ourselves** — which §4 wants
anyway, since SDK auto-placement puts one breakpoint on the last cacheable block
and none of §4.1–4.3's judgment. The missing feature is only the 1h TTL, which
matters for bursty traffic with >5m gaps. Defer to an SDK upgrade; don't block v1.

---

## 8. Accounting

The read side already exists — this makes it *attributable*:

- Stamp `prompt_cache_mode ∈ {auto, passthrough, off, n/a}` and
  `prompt_cache_breakpoints` (count) on the AIQG event.
- We already carry `CacheCreationTokens` / `CacheReadTokens` (`responses.go:38`) and
  cost (`pkg/clear/cost.go:159`) — so **savings become directly measurable**:
  `cache_read_tokens × (1 − 0.1) × input_rate`, minus write premium.
- **Alert on `cache_read_tokens == 0` for an `auto` route** — the silent-failure
  mode. Same lesson as `AIQG-SEMANTIC-CACHING.md` §14: a cache that never hits is
  indistinguishable from one that works unless you measure it.
- Surface **cold-write-from-model-switch** separately (§5.1), or routing changes
  will look like cache regressions.
- Count **below-minimum placements** (§4.5) — someone thinks caching is on there.

---

## 9. What we already have (this is cheaper than it looks)

The router already built the hard part, for agent-flow attribution:

- **`hashMessages`** (`internal/server/server.go:1007`) — canonical SHA-256 over a
  message sequence. Its own comment states the property we need: *"the hash of a
  state we serve matches the prefix the client re-sends verbatim on the next
  turn."* Deterministic (`json.Marshal` sorts map keys), tolerant of client-echoed
  extra fields.
- **`conversationPrefixHash`** (`server.go:~1029`) — hashes the conversation state
  up to the last assistant message.
- **`linkage.Store.IndexPrefix` / `ResolvePrefix`** (`pkg/aiqg/linkage/store.go:48`)
  — Redis-indexed, tenant-scoped, TTL'd.

> **A `ResolvePrefix` hit means a client re-sent a prefix verbatim — which is
> exactly the condition under which prompt caching pays.** So the existing linkage
> index is already a **live measurement of prompt-cache opportunity**, per tenant
> and per route, from traffic we are already serving.

That answers §5's default question **with data instead of a guess, before writing
any caching code**. It also gives §5.1 its lever: the conversation identity needed
for model-stickiness is already resolved.

Caveat: `ResolvePrefix` covers the **messages** tier. The **tools+system** tier
(§4.1) — probably the larger prize — isn't hashed today. Extending the same
normalizer to hash `(model, tools, system)` is small and self-contained.

---

## 10. Phasing

| | Stage | Contents | Gate |
|---|---|---|---|
| **P0** | **Measure (no request changes)** | Hash `(model, tools, system)` with the §9 normalizer; log prefix-reuse rate + reuse-within-5m per route/tenant from existing traffic. Zero risk, zero vendor change. | Reuse ≥ 2 on some route |
| **P1** | **Passthrough + `off`** | `cache_control` on `ContentPart`/system/tool types; thread to the Anthropic provider; `TAS-Prompt-Cache` header; `prompt_cache_mode` on events. Unblocks origins that already do it right. | — |
| **P2** | **`auto`** | §4 placement, per-route enable on the routes P0 identified. Savings metric + zero-hit alert. | Measured savings > write premium |
| **P3** | **Router-aware** | Model-stickiness within a conversation (§5.1); cache-state in the routing cost model; failover-cold-write visibility. | — |
| **P4** | **SDK upgrade** | v1.7.0 → v1.58.x for 1h TTL. Its own project. | — |

**P0 is the whole trick.** It converts "should the default be auto?" from an
argument into a query against data we already have.

---

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| **Auto costs +25% on non-reused prefixes** | Default `passthrough`; enable per route on P0 evidence (§5) |
| **Model routing silently voids the cache** (§5.1) | Stickiness in P3; surface cold-writes so it's visible, not mysterious |
| **Unstable tool list** — tools render at position 0, so a per-request tool set caches nothing | P0 measures it; surface as a bug against the origin, don't paper over it |
| **Origin sends `cache_control` we override in `auto`** | Documented precedence; `passthrough` and `off` always available |
| **Silent zero-hit** | Alert on `cache_read_tokens == 0` for `auto` routes (§8) |
| **Redaction instability would break prefixes** | ✅ **Checked — not a risk.** Every Gatekeeper strategy is deterministic and content-derived: `generateToken` and `generateHash` are SHA-256 of the value (`Gatekeeper/pkg/scan/redaction.go:295,302`), placeholders are a static map, masking is character-derived. Redaction is **cache-safe**, and the redacted form is *canonical* — it normalizes away PII variance. ⚠️ The separate **Databunker** tokenize path (`pkg/tokenize/databunker.go`) is **not** verified here — if it mints vault tokens per call rather than per value, it would break prefixes (§11 Q2) |
| **Reduction re-editing the cached prefix** | Reduce-at-source only — already the design (`AIQG_CACHE_SAFE_REDUCTION.md`); this doc is the reason that discipline pays |

**Open questions:**

1. **Default mode** — `passthrough` (recommended, §5) or `auto`? **P0 answers it
   with data; don't decide now.** *Owner: whoever reads the P0 numbers.*
2. **Databunker tokenize determinism** — does `pkg/tokenize/databunker.go` return a
   stable token per value, or a fresh one per call? A per-call token breaks *both*
   prompt caching and the §9 linkage index. **Verify before P2.** The in-band
   `pkg/scan/redaction.go` path is confirmed safe.
3. **OpenAI automatic caching** — exact threshold and behavior unverified (§6).
   Needed only to report `n/a` honestly, not for v1 function.
4. **Does `auto` override a client's `cache_control`, or merge?** Recommend
   override (a merge inherits the origin's mistakes, which is the thing we're
   fixing) — but it's a real call, and `passthrough` exists for the other answer.

---

## 12. Work breakdown

1. **tas-llm-router** — (P0) extend the `hashMessages` normalizer to
   `(model, tools, system)`; log reuse. (P1) `cache_control` on
   `ContentPart`/system/tool types + provider threading + `TAS-Prompt-Cache`
   header + `prompt_cache_mode`/`prompt_cache_breakpoints` on events. (P2)
   placement engine (§4) + route config.
2. **aiqg-dashboard-be** — prefix-reuse + cache-savings aggregation;
   `prompt_cache_mode` in `/events` + `/metrics`.
3. **aiqg-ui** — savings line in the Cost report; zero-hit warning on `auto` routes.
4. **aether-shared/data-models/aiqg/** — `response-event.md` for the new fields.
5. **Origins** — once P1 lands, aether-be / agent-builder can set breakpoints
   deliberately; until then, nothing they send has any effect.
