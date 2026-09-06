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

  > ⚠️ **P1 crosses onto the tagged wire — the struct tag has to be right, and a
  > wrong one fails silently.** This field lands on the emitted **event struct**,
  > which is serialized to JSON and read cross-service (Loki/LogQL,
  > `aiqg-dashboard-be`). Unlike P0's probe — which logs via `logrus.Fields`
  > with explicit lowercase keys, so no Go field name ever reaches the wire — an
  > event-struct field takes its JSON name from its **`json:"..."` tag**, and Go's
  > default is the **PascalCase field name** if the tag is missing. So the field
  > MUST be tagged `json:"prompt_cache_mode,omitempty"` (snake_case, `omitempty`),
  > matching the sibling accounting fields (`json:"cache_read_tokens,omitempty"`,
  > `event.go:271`). Get it wrong — `PromptCacheMode` on the wire, or a typo — and
  > there is **no error**: the emitter still writes valid JSON, and every
  > dashboard query and LogQL `| json | prompt_cache_mode=...` silently matches
  > nothing. This is the exact silent-miss failure §14 warns about, one layer up.
  > **Grep the emitted JSON for the literal `prompt_cache_mode` before trusting
  > any panel built on it.**
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

### 9.1 Production evidence (Loki, 30 days, measured 2026-07-16)

Queried `{namespace="tas-llm-router", service="llm-router-aiqg"}` over 30d.
(Split by `service`, not `container` — both deployments share the container name
`llm-router`, so `container=` aggregates them; see §9.2.)

| | `llm-router-aiqg` | `llm-router` |
|---|---|---|
| `chat/completions` requests | **339** | 2 (idle) |
| AIQG response events parsed | **168** | 0 |
| Models | `claude-haiku-4-5` (156, 540,908 input tok) · `claude-opus-4-6` (12, 372) — **100% Anthropic** | — |
| Total prompt tokens | **541,280** | — |
| Total completion tokens | **13,356** | — |
| **Input : output ratio** | **40 : 1** | — |
| Total cost | **$0.8832** | — |
| **Events with any `cache_*` token** | **0** | 0 |
| **Nonzero cache tokens, ever** | **0** | 0 |

All AIQG traffic is `llm-router-aiqg`; the base `llm-router` deployment is
effectively idle. Consistent with Plan #7's shadow reduction being live on
`llm-router-aiqg` only.

**Not one cached token in 30 days**, across 168 real Anthropic requests. Absence is
conclusive rather than ambiguous: `builder.go:491` populates the fields only
`if routing.CacheCreationTokens > 0 || routing.CacheReadTokens > 0`, and they are
`omitempty` — so a missing key means a true zero, not a logging gap.

**The read side is fully wired and has never fired.** The whole chain exists:
`provider.go:199` → `types.Usage` (`responses.go:38`) → `StampTokenUsage`
(`server.go:779`) → `aiqg_routing.go:194` → `builder.go:491` → the event's
`token_accounting` block — plus `CacheAwareTotalCostUSD` and `pkg/clear/cost.go`'s
`CacheAwareCost` with the correct 1.25× write / 0.10× read multipliers.

> **We built, tested, and deployed complete cache-aware accounting for a feature
> the gateway cannot turn on.** That is §1 restated as an artifact: not an
> oversight of analysis, but a gap in the request path that the analysis layer
> already anticipated.

The **41:1 input:output ratio** is exactly the input-dominated shape
`AIQG_CACHING_PRIMER.md` §1 describes — the shape prompt caching exists to fix.
One observed request spent **70,122 prompt tokens on 4 completion tokens**, on
Haiku 4.5 (minimum cacheable prefix 4096 — clears it 17× over).

**Two honest limits on this evidence:**

1. **Volume is trivial** — $0.88 over 30 days. This is a test/demo cluster, so the
   *absolute* saving forgone is a rounding error and proves nothing about
   production value. What it proves is **structural**: the mechanism is inert.
2. **It does not show we are losing money.** Much of this traffic is
   latency-experiment flows (`flow_id: latexp-60000-*`) whose padding is plausibly
   unique per request — and a cache never hits on a prefix that never repeats.
   **Whether these prefixes repeat is exactly what P0 measures**, and is the
   difference between "the lever is unavailable" (proven here) and "the lever
   would pay" (not yet shown).

### 9.2 Three suspected observability gaps — all three investigated, none real

An earlier revision of this doc reported three Loki gaps. **All three were
artifacts of shallow checks and are withdrawn.** Recorded so nobody re-files
them:

| Suspected | Verdict |
|---|---|
| `llm-router` / `llm-router-aiqg` indistinguishable in Loki | ❌ **False.** They are cleanly separable. There is no `pod` label, but there are **`service`** and **`service_name`** (`llm-router` vs `llm-router-aiqg`) and **`instance`** (the pod name). The original claim came from a series parser that only looked for `container` and `pod`. |
| `aiqg` namespace missing from Loki | ❌ **False.** `aiqg` **is** collected — 5,292 lines over 30d. The `/label/namespace/values` endpoint defaults to a ~6h window, and `aiqg-dashboard-be`/`aiqg-ui` only log on request, so a quiet window read as an absent namespace. Alloy's regex is correct and deployed. |
| Loki ingress fails TLS verification | ❌ **Not a bug — by design.** The cert is issued by the internal `tas-ca-issuer` (`O=TAS, CN=TAS Root CA`), so a default trust store correctly rejects it. `curl --cacert <TAS Root CA> https://loki.tas.scharber.com/ready` → **200**. The ingress is fine. |

**The one real (small) finding:** CLAUDE.md's Loki example
(`curl -G 'https://loki.tas.scharber.com/...'`) fails as written on a machine
without the TAS Root CA installed, with an opaque `curl exit 60`. Worth a
one-line note pointing at `--cacert` or the port-forward — a docs nit, not an
infra gap.

> **Method note, since it changed the numbers.** Two of these three would have
> become filed issues on the strength of a single shallow query. The default
> label-values window and an incomplete label parse each produced a confident,
> wrong conclusion. Re-checking with an explicit window and a full label dump
> killed all three — and, in doing so, upgraded §9.1 from an aggregate across two
> deployments to a clean per-service attribution. Prefer `service="llm-router-aiqg"`
> over `container="llm-router"` in every query here: the container name is shared
> by both deployments and is *not* the discriminator.

---

## 10. Phasing

| | Stage | Contents | Gate |
|---|---|---|---|
| **P0** ✅ | **Measure (no request changes)** — *implemented, pending deploy* | `pkg/aiqg/promptcache` (probe, 5m sliding window, tenant-scoped, `aiqg:pcp:` namespace) + `cachePrefixHash` (`internal/server/server.go`) + `StampCachePrefixHash` + `probePromptCache` in the AIQG middleware's deferred path. Emits `msg="aiqg prompt-cache probe"` with `prefix_seen`, `model`, `prompt_tokens`. Zero risk, zero vendor change. | Reuse ≥ 2 on some route |
| **P1** ✅ | **Passthrough + `off`** | Done: `cache_control` on `ContentPart`/system/tool types threaded to the Anthropic provider (all four surfaces, PR #197, incl. the `omitzero` serialize fix); `TAS-Prompt-Cache` header + gateway-wide `prompt_cache.default_mode` (PR #198); `prompt_cache_mode`/`prompt_cache_breakpoints` on events. | — |
| **P2** ◑ | **`auto`** | Done: §4.1 (system) + §4.2 (last complete turn) placement + §4.5 model-minimum gate + OpenAI no-op (`Available(auto)`=true, `Apply(auto)` places); savings metric + zero-hit alert scaffold (PR #199). **Remaining: §4.3 lookback fillers** (intermediate breakpoint every ~15 blocks in a long agentic turn); per-route enable on P0-identified routes. | Measured savings > write premium |
| **P3** | **Router-aware** | Model-stickiness within a conversation (§5.1); cache-state in the routing cost model; failover-cold-write visibility. | — |
| **P4** | **SDK upgrade** | v1.7.0 → v1.58.x for 1h TTL. Its own project. | — |

**P0 is the whole trick.** It converts "should the default be auto?" from an
argument into a query against data we already have.

### 10.1 Reading the P0 result

Once deployed, the reuse rate is a Loki query — no new pipeline:

Filter by `service`, not `container` — both deployments share the container name
(§9.2), and today essentially all AIQG traffic is `llm-router-aiqg`:

```logql
# reuse rate (the number that picks the default)
sum(count_over_time({namespace="tas-llm-router",service="llm-router-aiqg"} |= "prompt-cache probe" | json | prefix_seen="true" [7d]))
  / sum(count_over_time({namespace="tas-llm-router",service="llm-router-aiqg"} |= "prompt-cache probe" [7d]))

# by model — a switch is a cold rebuild (§5.1), so this splits the tension out
sum by (model) (count_over_time({namespace="tas-llm-router",service="llm-router-aiqg"} |= "prompt-cache probe" | json | prefix_seen="true" [7d]))

# the prize, in tokens that would have billed at 0.1x instead of 1.0x
sum_over_time({namespace="tas-llm-router",service="llm-router-aiqg"} |= "prompt-cache probe" | json | prefix_seen="true" | unwrap prompt_tokens [7d])
```

**Read it against the write premium, not against zero.** A route pays off when
its prefix recurs ≥2× per window (§5); a 30% hit rate on 70k-token prefixes and
a 30% hit rate on 500-token ones are very different calls, which is why
`prompt_tokens` rides on every line.

Three ways this reads ~0% that are **not** "caching wouldn't help" — check them
before concluding anything:

1. **No probe lines at all** → Redis unconfigured (`AIQG_LINKAGE_REDIS_URL`), so
   the probe is nil. Absence of the metric, not absence of reuse.
2. **All lines `prefix_seen=false` with high volume** → prefixes genuinely don't
   recur (plausible for the `latexp-*` synthetic flows, §9.1), *or* an origin is
   varying its system prompt per request — which is itself a finding worth
   surfacing (§11's unstable-tool-list risk, same shape).
3. **Few lines relative to `chat/completions`** → most traffic has no cacheable
   span (no system prompt, no tools), so the hash is empty by design. That's a
   real answer: there is nothing for §4.1 to cache.

---

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| **Auto costs +25% on non-reused prefixes** | Default `passthrough`; enable per route on P0 evidence (§5) |
| **Model routing silently voids the cache** (§5.1) | Stickiness in P3; surface cold-writes so it's visible, not mysterious |
| **Unstable tool list** — tools render at position 0, so a per-request tool set caches nothing | P0 measures it; surface as a bug against the origin, don't paper over it |
| **Origin sends `cache_control` we override in `auto`** | Documented precedence; `passthrough` and `off` always available |
| **Silent zero-hit** | Alert on `cache_read_tokens == 0` for `auto` routes (§8) |
| **Redaction instability would break prefixes** | ✅ **Not a risk — for a blunter reason than expected: the router does not redact at all** (§11.1). Nothing mutates the prompt between the client and the vendor, so there is no transform that could destabilize a prefix. (Were redaction enabled, it would still be safe: every `pkg/scan` strategy is deterministic and content-derived — `generateToken`/`generateHash` are SHA-256 of the value, `redaction.go:295,302`.) |
| **Reduction re-editing the cached prefix** | Reduce-at-source only — already the design (`AIQG_CACHE_SAFE_REDUCTION.md`); this doc is the reason that discipline pays |

### 11.1 🚨 The router does not redact — it scans and blocks

Found while resolving Q2. Verified end-to-end; it invalidates a premise in the
sibling caching docs, so it is recorded here rather than in a comment.

**`tas-llm-router` never modifies prompt content. It scans, and it blocks or
reports. Content reaches the vendor byte-for-byte as the client sent it.**

The chain, each link checked:

| Link | Fact |
|---|---|
| Router → pipeline | `pipeline.NewProcessor(scanner, WithConfig(procConfig))` — **no `WithTokenizer`** (`internal/gatekeeper/gatekeeper.go:149`) |
| Tokenization | `EnableTokenization` unset by `DefaultProcessorConfig()` → **false**; call double-guarded (`default_processor.go:184`) |
| Pipeline redaction | **The pipeline never redacts.** `RedactionEngine` appears only in `pkg/scan`; the pipeline's *only* content-mutating path is the disabled tokenizer |
| Scanner | `ScanWithRedaction` exists (`default_scanner.go:406`) but the pipeline calls plain `Scan`. The engine is used only for `GeneratePreview` (safe finding display, `:262`) |
| Inbound scan | `ScanMessages` extracts text into a *separate* string to scan; it never mutates `messages`. The handler then blocks (403), sets `X-TAS-Scan-Status`, and stamps finding counts — **it never rewrites `req.Messages`** (`server.go:641-679`) |
| Outbound scan | Same shape: scan → `ShouldBlock` → block or pass (`server.go:926`) |
| Result | `ProcessResult.RedactedContent` is never populated, and no router code reads it |

**Why this matters here (mildly):** the pre-scan vs post-scan hashing question in
§9 is **moot** — pre-scan and post-scan content are identical, so `cachePrefixHash`
is exact rather than conservative. The documented under-count bias cannot occur
today. Good news, and it simplifies P0.

**Why it matters a lot next door (the real point):** both sibling designs assume a
redaction that does not exist.

- [`AIQG-CACHING.md`](AIQG-CACHING.md) §2/§3/§5 and
  [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md) §4.1 specify caching the
  **"post-scan (redacted) prompt"** so the cache "never keys on or stores raw
  PII". **Today, post-scan == pre-scan.** A cache built on that premise as written
  would store **raw PII bodies at rest in Redis**, TTL'd but unredacted — while
  the doc claims the opposite.
- The semantic design's claim that redaction is a **hit-rate win** (it
  "normalizes away the PII that would otherwise make every prompt unique") is
  likewise inoperative: there is nothing normalizing anything.

**This does not break those designs — it converts an assumed property into an
explicit prerequisite.** Enabling redaction in the router is a precondition of
any response-cache work (C1 or C4), not a detail of it. Sequenced correctly, it
is also cheap: the machinery exists (`ScanWithRedaction`, a deterministic
`RedactionEngine`), it is simply not wired.

Note this is **orthogonal to prompt caching** (this doc): vendor prompt caching
sends the same bytes either way and stores nothing on our side. The prerequisite
lands on the *response*-caching docs, which persist bodies.

**Open questions:**

1. **Default mode** — `passthrough` (recommended, §5) or `auto`? **P0 answers it
   with data; don't decide now.** *Owner: whoever reads the P0 numbers.*
2. ~~**Databunker tokenize determinism**~~ — ✅ **RESOLVED: not a blocker, because
   it is not in the router's path at all.** The Databunker tokenizer is dead code
   here, triple-gated:
   - The router never calls `WithTokenizer` — it builds
     `pipeline.NewProcessor(scanner, pipeline.WithConfig(procConfig))`
     (`internal/gatekeeper/gatekeeper.go:149`), so `p.tokenizer` is **nil**.
   - `DefaultProcessorConfig()` never sets `EnableTokenization`
     (`Gatekeeper/pkg/pipeline/processor.go:135`) → Go zero value → **false**.
     (It *does* populate `TokenizeTypes`, which reads as configured tokenization
     and is why this looked live.)
   - The call is guarded on both: `if p.config.EnableTokenization &&
     p.tokenizer != nil && …` (`default_processor.go:184`).
   - `ProcessResult.RedactedContent` is therefore never populated — and the
     router never reads it regardless.

   **The bigger finding this surfaced is §11.1.**
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
