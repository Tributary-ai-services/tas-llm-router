# AIQG — Semantic Response Caching (Design)

Status: **Design / for review** — NOT implemented. Gateway feature
(`tas-llm-router`). Expands §9 of [`AIQG-CACHING.md`](AIQG-CACHING.md) (phase C4)
and closes out issue
[tas-llm-router#97](https://github.com/Tributary-ai-services/tas-llm-router/issues/97).

Related: [`AIQG-CACHING.md`](AIQG-CACHING.md) (exact-match v1 — **this doc depends
on it**), `AIQG-EXTENSION.md` (`cache_state` dimension), `AIQG-EXPERIMENTS-RUNNER.md`,
`account.md` (`payload_retention_mode`), `tas-aiqg/AIQG_CACHING_PRIMER.md` §5
(scope guard), `tas-aiqg/AIQG_GLOSSARY.md` (terminology — **needs a semantic-cache
entry**, see §16).

---

## 1. Problem & goal

Serve a stored answer for a request that is *semantically similar* — not
byte-identical — to one we've answered before, without calling the vendor.

Where the three levers sit ([primer](../../tas-aiqg/AIQG_CACHING_PRIMER.md) §5):

| Lever | What it does | Saves | Can it change the answer? |
|---|---|---|---|
| Prompt/prefix caching (vendor-native) | **cheapens** a call you still make | input prefix @ 0.1× read | **No** — exact hash, arithmetic reuse |
| Exact-match response cache (C1) | **eliminates** byte-identical repeats | 100% of the call | **No** — exact hash |
| **Semantic cache (this doc)** | **eliminates** *near*-duplicate repeats | 100% of the call | **YES — this is the entire design problem** |
| AIQG payload reduction | **shrinks** each call | input tokens | No (content-preserving by contract) |

Semantic caching is the only lever on that list that can return a **wrong
answer**. Everything below follows from that one asymmetry.

### 1.1 The governing finding

> **A single similarity threshold over a general-purpose embedding cannot reach
> an acceptable false-hit rate. Not at any value. The number does not exist.**

The evidence is unambiguous and convergent:

- **MeanCache** ([2403.02694](https://arxiv.org/abs/2403.02694), IPDPS'25), the
  *winning* system in its own paper, serves **89 false hits over 700 queries —
  ~12.7%**. It beats GPTCache (233 / ~33%) by 17% F-score. Both are unshippable.
- **vCache** ([2502.03771](https://arxiv.org/abs/2502.03771), ICLR'26) shows
  GPTCache's error rate **increases with sample size** — static thresholds never
  converge, they accumulate false hits.
- **Canonical AI's counterexample**, which no threshold survives:
  *"What are the fees on the Chase Sapphire card?"* vs *"…Chase Sapphire
  **Reserve** card?"* → **cosine 0.99**, different answers. That query shape —
  SKUs, plan tiers, model versions, dates, entity names — is everywhere in real
  products.
- **GPTCache's own last substantive commit** (2025-07-11) added *"post-processing
  for verifying hit questions using LLM"*. The reference implementation's final
  act was bolting a judge onto its own hits. The project then went dormant.

**Design consequence:** the similarity threshold is **candidate generation**, not
the decision. A hit must survive a **verification gate**. Any design that ships a
threshold as the accept/reject boundary is knowingly shipping a ~5–13% wrong-answer
rate into a compliance product. §5 is therefore a cascade, not a lookup.

### 1.2 🚨 The honest economics — this is a latency lever, not a cost lever

The savings math is worse than the framing in issue #97 implies, and the doc should
say so before anyone builds a business case on it.

**Redis's own framing** ([LangCache calculator](https://redis.io/calculator/langcache/)):
*"you don't pay for output tokens; input token costs are typically offset by
embedding and storage costs."* So the net saving ≈ **output tokens × hit rate**,
minus embedding + vector storage. Their worked example nets **$60/month on a $200
spend at a 50% hit rate**.

Now apply *our* threshold policy (§9.3). At a **safe** threshold (~0.97 ⇒ 5–10% hit
rate), that same $200 spend nets **~$6–12/month**. The rate that makes the cost
story sing — 50% — sits at a threshold where **7–15% of hits are wrong** (§9.3).

> **The profitable region and the safe region barely overlap.** That is the
> central tension of this feature, and no amount of tuning dissolves it.

Consequences:
- **Lead with latency.** A hit turns a 1–5s call into ~30ms. That is a real,
  defensible, *correctness-free* win. The cost saving is a rounding error at
  thresholds we'd actually ship.
- **This tension is exactly why AIQG's other two levers exist.** Prompt caching and
  payload reduction pay **without depending on repetition and without a
  wrong-answer mode**. Semantic caching is the narrow third lever, not the
  headline. Positioning it as a general cost lever would undercut the levers that
  actually carry the thesis.
- **It sharpens §18 Q5.** If S1 shadow mode shows a 2% hit rate, the honest saving
  is a few dollars a month against a per-tenant vector index, a judge budget, and
  two published attack classes. **Not shipping is a legitimate — possibly the
  likely — outcome.**

### 1.3 Non-goals

- Not vendor prompt-caching (different mechanism — see §12 for the Go hazard).
- Not the Redis token/bundle resolution cache, nor `linkage.Store`.
- **Not a general cost lever.** Hit rate ≡ query repetition; ~0 on agent/coding
  traffic. Advertising it broadly is how this feature earns a bad name (§3).

---

## 2. Prior art — what we're borrowing, and from whom

Surveyed 2026-07-16. The full landscape is in §17; the load-bearing conclusions:

| Source | What we take |
|---|---|
| **[vllm-project/semantic-router](https://github.com/vllm-project/semantic-router)** — Apache-2.0, **Go**, 5.0k★, active | **Reference architecture.** Envoy ExtProc — *the same slot as tas-llm-router* — shipping a semantic cache **and** a ModernBERT PII classifier. Gatekeeper + cache in one Go codebase. `cache_scope_isolation_test.go` / `cache_user_scope_test.go` — tenant scoping as a **tested invariant**. |
| **Redis `langcache-embed-v2/v3-small`** ([2504.02268](https://arxiv.org/abs/2504.02268)), Apache-2.0 | **Embedding model (§6).** Purpose-trained for *cache matching*; eval domains include **negation** — attacking the root cause, not the symptom. |
| **RedisVL** `SemanticCache` + `CacheThresholdOptimizer` | Knob shape; `filterable_fields` tenant story; **calibration methodology** (§9). |
| **Krites** ([2602.13165](https://arxiv.org/abs/2602.13165)) | **Async LLM-judge on near-misses, off the critical path** → 3.9× coverage, no latency cost. Best cost/latency idea found (§5, L3). |
| **vCache** ([2502.03771](https://arxiv.org/abs/2502.03771)) | Per-entry adaptive threshold + FPR-budget framing. ⚠️ **CC BY-NC-ND 3.0 — reimplement from the paper, never vendor the code** (§17.1). |
| **Bifrost** (Go) | Per-request headers; **`cache_debug` returning similarity + threshold**; `conversation_history_threshold`. |
| **LiteLLM** | Cache-Control surface (`no-cache`/`no-store`/`s-maxage`). And four cautionary tales (§15). |
| **Portkey** | Exact-before-semantic; **≤4 messages** — excludes agent loops *structurally*. |

**The gap we can fill:** vllm-sr has the Go production architecture but hand-tuned
YAML thresholds and no calibration. aurelio's semantic-router has the calibration
method but is Python. **Nobody ships both.** That's a real contribution — and it
lines up with the AIQG auto-learning thesis.

---

## 3. Scope guard — where this is allowed to run

Semantic caching pays only when **all three** hold (primer §5; issue #97 comment):

1. **Repetition** — requests recur in *meaning*, across users/sessions/time.
2. **Statelessness** — the answer depends only on the question, not on user,
   session, or changing context. A near-match must legitimately *deserve* the
   same answer.
3. **Staleness-tolerance** — an answer up to TTL old is acceptable.

**Qualifying** (opt-in per route): FAQ / support Q&A · classification, moderation,
routing over near-duplicates (**tightest threshold** — similar inputs routinely
deserve *different* labels) · burst/thundering-herd dedup · bounded-domain internal
assistants (fixed-schema SQL, common-phrase translation) · dev/test/eval replay.

**Structurally excluded** (not "default off" — *refused*, §10.3):
- **Agentic / coding / tool-using** — each request is unique; a hit would be
  **wrong**. The big disqualifier. Enforced by the tool-call and
  conversation-length guards, not by operator discipline.
- **Personalized / stateful** · **freshness-critical** (prices, inventory, news)
  · **creative** (variety is the point).

> **Enforcement over advice.** Portkey's ≤4-message cap and Bifrost's
> `conversation_history_threshold: 3` are the same insight: make the exclusion
> **structural**, so a misconfigured route *cannot* silently serve stale agent
> answers. We adopt both (§10.3).

---

## 4. Where it sits in the request path

Correcting a stale pointer in the C1 doc: the scan pipeline lives in
**`(*Server).handleChatCompletion`** (`internal/server/server.go:539`) — *not*
`handleAIQG` (`internal/middleware/aiqg.go:124`, which is AIQG auth/policy
middleware). Anchors: inbound scan `server.go:606-625`; shadow reduction
`server.go:666-696`; outbound scan `server.go:889`.

```
handleChatCompletion
  → resolve token / policy bundle
  → inbound Gatekeeper scan (+ redact/tokenize)        server.go:606
  → resolveExperiment                                   → BYPASS if claimed (§13)
  → route eligibility gate (§3, §10.3)                  → BYPASS if excluded
  → L0  exact-match lookup (C1, hash)                    ─┐
  → L1  embed + vector range search  (candidate)          │ §5
  → L2  verification gate            (accept/reject)      │
        hit    → return cached, cache_state=semantic_hit ─┘
        reject → L3 async judge enqueue, fall through
  → miss → Router.Route → vendor → outbound scan          server.go:889
         → CACHE STORE (post-outbound-scan) + embed
  → events.Build stamps cache_state + similarity + cache_key_hash
```

**Insertion point: `server.go:666`.** It is post-inbound-scan, holds
`req.Messages` and the resolved policy bundle, and **already pays an embedding
round-trip** for shadow reduction — the L1 embed can share it.

### 4.1 Gatekeeper ordering — scan first, then look up

Both orderings are defensible; only one is safe. **Scan before lookup, and key on
the post-scan (redacted) prompt:**

- A lookup *before* scanning means a **cache hit bypasses Gatekeeper entirely** —
  a policy change would not apply to cached traffic, and content that *should* be
  blocked gets served. Gatekeeper is a compliance control; it cannot have a
  bypass door.
- **Never store unscanned bodies.** A cache is a persistence layer; unscanned PII
  in Redis is PII at rest.
- Redaction is a **hit-rate win**, not just a tax: it normalizes away the PII that
  would otherwise make every prompt unique.

---

## 5. Architecture — the cascade

Four layers. Each exists because a specific measured failure mode kills the
simpler design.

```
L0  EXACT MATCH  (C1 — hash, sub-ms)
    ├─ hit → return. Zero false-hit risk. Covers the hottest traffic for free.
    └─ miss ↓                              [LangCache, Portkey, Bifrost all converge here]

L1  CANDIDATE GENERATION  (embed ~15-30ms + FLAT range search ~1ms)
    FT.SEARCH @embedding:[VECTOR_RANGE {d} $vec] + (@tenant:{t} @model:{m})
    ├─ no candidate → miss ↓
    └─ candidate(s) + similarity ↓
       NOT a decision — a shortlist. (§1.1)

L2  VERIFICATION GATE  (the part that makes it correct)
    Cheap, deterministic, synchronous. ALL must pass:
      a. Entity/number/date guard — discriminative tokens in the candidate
         prompt must match the incoming prompt. Kills "Sapphire" vs
         "Sapphire Reserve" @ cosine 0.99, which NO threshold reaches.
      b. Negation guard — polarity must agree ("is X safe" vs "is X not safe").
      c. Freshness — entry age ≤ TTL, and s-maxage if the caller set one.
      d. Scope — tenant + model + params match exactly (defense in depth; L1
         already filtered).
    ├─ pass → SEMANTIC HIT
    └─ fail → treat as MISS, and ↓

L3  ASYNC JUDGE  (Krites — OFF the critical path)
    Near-misses (similarity in the danger band, or L2-rejected) are sampled to a
    queue. An LLM judge decides post-hoc whether the answer would have been
    correct.
      → feeds the FPR metric (§14) — our only ground truth
      → feeds per-entry threshold adaptation (vCache-style, §9.3)
      → approved pairs promote into the cache
    Costs nothing in request latency. This is where the system LEARNS.
```

**Why L2 is not optional.** L1 alone *is* GPTCache, and GPTCache's own authors
concluded it needed a judge. L2 is the cheap deterministic 80% of what that judge
does, moved onto the fast path; L3 is the expensive 20%, moved off it.

**Why L2 is mostly lexical.** The failure mode is *embedding separation*, so the
gate must not be another embedding comparison — that's the same sensor twice. The
discriminators are entities, numbers, dates, and negation, which lexical checks
catch precisely where cosine fails. (GroundedCache's multi-gate design reports
**0.0% unsafe-served** vs 15–35% naive; single-author and unreviewed, so we take
the *pattern*, not the number.)

---

## 6. Embedding model & dimensionality

**Decision: [`redis/langcache-embed-v3-small`](https://huggingface.co/redis/langcache-embed-v3-small)** —
Apache-2.0, **384-dim**, 22.6M params, 128 max seq, served via **self-hosted TEI**
(HF `text-embeddings-inference`) over in-cluster HTTP.

| Candidate | Dim | Verdict |
|---|---|---|
| **`langcache-embed-v3-small`** | 384 | ✅ **Chosen.** Purpose-trained *for cache matching* (ArcFaceInBatchLoss, ~8M pairs). **Same 384 dims as the all-minilm already deployed** → drop-in. Apache-2.0. |
| `langcache-embed-v2` | 768, **Matryoshka [768→64]** | Upgrade path. 8192 seq; truncate at serve time — a memory/latency dial with **no retrain and no reindex lock-in**. Take it if 128 tokens proves too short (§18 Q3). |
| `all-minilm` (Ollama, deployed) | 384 | Dev/bootstrap only. General-purpose → *is* the false-hit root cause. |
| ada-002 (deeplake-api) | 1536 | ❌ Dimension-incompatible; wrong service; wrong namespace. |

**Why the model is the highest-leverage decision here.** Redis fine-tuned models
specifically for this task and benchmarked them on **negation** — the canonical
false-hit mode. Their finding: compact fine-tuned models, trained **one epoch**,
beat SOTA open *and* proprietary embeddings at cache matching. Threshold tuning
attacks the symptom; the embedding attacks the cause.

**Latency math (comfortable).** Embed ~15–30ms (CPU) + FLAT search ~1ms vs an LLM
call of 1–5s ⇒ **~1–3%** of the call avoided. On a **miss** we pay ~30ms as pure
overhead — acceptable even at a 10% hit rate. L1 also piggybacks the embed already
paid at `server.go:666`.

**Why TEI, not Ollama, and not in-process:**
- The deployed Ollama is **fragile as a request-path dependency**: `ollama.yaml`
  is *absent from* `aether-shared/k8s-shared-infrastructure/kustomization.yaml`,
  has **no model-pull step**, and its readiness probe (`/`) **passes with no model
  loaded**. A `kustomize build | apply` would not recreate it. It survives today
  only because it was applied out-of-band. Today that degrades shadow measurement;
  in the request path it is a latency/availability SPOF that reports healthy while
  broken.
- TEI keeps the model swappable without a router rebuild, and CLAUDE.md already
  carves out the GPU-workload exception for exactly this.
- In-process ([`hugot`](https://github.com/knights-analytics/hugot) now has a
  **pure-Go GoMLX backend** explicitly targeting "smaller models such as
  all-MiniLM-L6-v2" — genuinely viable in 2026, unlike a year ago) is a **later
  latency optimization**, not a starting point.

> **`EmbeddingsCache` (RedisVL's second-order trick):** cache embeddings keyed by
> `(content, model)`. Removes embed latency from the repeated-prompt path. Cheap;
> take it in S2.

---

## 7. Vector store — the decision, and its blocker

### 7.1 🚨 Blocker: nothing deployed can index a vector

Verified against the live cluster (`redis-shared`, `tas-shared`, single master —
`replicas: 1`, no split-brain):

```
redis:7-alpine → redis_version:7.4.7, standalone
MODULE LIST            → (empty)
COMMAND INFO FT.CREATE → (empty)     # RediSearch absent
COMMAND INFO VADD      → (empty)     # Vector Sets absent
FT._LIST               → ERR unknown command
```

And PostgreSQL is **stock `postgres:15-alpine`**
(`aether-shared/k8s-shared-infrastructure/postgres.yaml:23`) —
`pg_available_extensions WHERE name='vector'` returns **empty**. pgvector is not
merely uninstalled, it is **unavailable**; `CREATE EXTENSION vector` would fail.

**Every "we already have Redis" plan assumes the Redis Query Engine, which is only
core from Redis 8.** This is a prerequisite, not a detail. **S0 of §15 is an infra
change or this feature does not exist.**

### 7.2 Decision: Redis 8 + `FT.*` FLAT index

The only option giving **threshold + tenant filter + TTL in one system**, via a
Go client with typed, first-class support.

| Option | Verdict |
|---|---|
| **Redis 8 Query Engine** (`FT.*`, go-redis v9) | ✅ **Chosen.** Threshold + TAG pre-filter + native TTL. `go-redis` v9.21.0 typed API. Upgrades infra we already run. ⚠️ **Tri-license RSALv2/SSPLv1/AGPLv3 — legal must confirm against our distribution model (§18 Q1).** |
| **pgvector** | 🥈 **Fallback if Redis 8 is blocked** (incl. on licensing). Postgres already deployed; threshold = `WHERE emb <=> $1 < 0.1`; filters = SQL; TTL = `expires_at` + reaper. [`pgvector-go`](https://github.com/pgvector/pgvector-go) effectively first-party. **Still needs an image swap** → `pgvector/pgvector:pg15`. |
| Redis **Vector Sets** (`VADD`/`VSIM`) | ❌ **No per-element TTL** — disqualifying for a cache. go-redis support "experimental"; deletions cause **HNSW latency spikes**, so a hand-rolled TTL reaper periodically stalls Redis. Different score scale again (`(cos+1)/2`). |
| **DeepLake API** | ❌ Single-replica on a local-path RWO PVC (SPOF), different namespace (`aether-be`), deployed at 1536-dim. Wrong tool. |
| Qdrant / Milvus / Weaviate | ❌ New service to run for ~10k–100k rows. ([`qdrant/go-client`](https://github.com/qdrant/go-client) is the best of these if we ever need it. Note `milvus-sdk-go` was **archived 2025-03-21** — anyone citing it is on stale info.) |
| Embedded (chromem-go, sqlite-vec, `coder/hnsw`) | ❌ **Architectural, not quality:** the router runs **multiple replicas**; a per-pod cache **divides hit rate by replica count**. A semantic cache is only worth building if it's shared. (Also: `coder/hnsw` had *search-termination and heap-correctness* bugs fixed mid-2026. Pure-Go HNSW is unsolved.) |

**Use FLAT, not HNSW.** At 10k–100k entries brute force is **exact** — no recall
loss on a correctness-sensitive path, no graph build, no `EF_RUNTIME` tuning. HNSW
earns its keep at millions; we are three orders of magnitude away.

### 7.3 Storage shape

```
HASH   aiqg:scache:{tenant}:{hash} → {prompt, response, embedding, tenant, model,
                                      scoring_version, created_at, clear_score}
EXPIRE ttl                          → Redis 8 filters passively-expired docs at
                                      query start (pre-8 could return nil names)

FT.CREATE aiqg_scache_idx ON HASH PREFIX 1 aiqg:scache:
  tenant          TAG               → scope isolation (§8) — the CacheAttack defense
  model           TAG               → never cross-serve models
  scoring_version TAG
  embedding       VECTOR FLAT 6 TYPE FLOAT32 DIM 384 DISTANCE_METRIC COSINE

lookup (range query IS the cache primitive — not KNN):
  @embedding:[VECTOR_RANGE {d} $vec]=>{$YIELD_DISTANCE_AS: dist}
    (@tenant:{t} @model:{m} @scoring_version:{v})
  SORTBY dist ASC LIMIT 0 {k} DIALECT 2
```

`$YIELD_DISTANCE_AS` is **required** to get the distance back. Redis cosine
distance `= 1 - cos_sim`, so "similarity ≥ 0.95" ⇒ `VECTOR_RANGE 0.05`. Use
`FT.SEARCH` **range**, not KNN — we want *"everything within d"*, not *"the
nearest k regardless of how far"*. KNN with no range bound is a false-hit
generator.

Do **not** use `RediSearch/redisearch-go` (abandoned 2024-07-01). `go-redis`
v9.21.0 has typed `FTCreate`/`FTSearch`.

---

## 8. Cache key, isolation & security

Key inherits C1 (`AIQG-CACHING.md` §3) — `tenant_id` + vendor + model +
normalized post-scan messages + output-affecting params + `scoring_version` — with
`tenant` and `model` **also materialized as TAG fields** so they pre-filter the
vector query, not just the hash.

### 8.1 🚨 The threshold is a security parameter

**CacheAttack** ([2601.23088](https://arxiv.org/html/2601.23088v2), **NDSS 2026**)
models semantic cache keys as **fuzzy hashes** — locality-preserving by design,
which is the *exact opposite* of cryptographic avalanche. GCG-based adversarial
suffix search induces collisions on purpose:

| Attack | Success |
|---|---|
| Hijacking LLM responses via semantic cache | **86.9%** |
| Against **AWS Bedrock / Azure API Management** | **78.2–88.3%** |
| Agent tool-invocation hijacking | **90.6%** (up to 84.5% TSR degradation) |

Tested against GPTCache with all-MiniLM-L6-v2, gte-small, e5-small-v2,
bge-small-en-v1.5 — and it **transfers across embedding models**. Of the published
defenses, only **per-user/per-tenant isolation eliminates** cross-user collisions;
key salting is worth a mere −9.5–21.0%.

### 8.2 🚨 A shared cache is a PII oracle — even if it never returns the text

Cache hits (10–50ms) and misses (500–2000ms) are trivially distinguishable —
**100% accuracy** from temporal features alone. **["The Early Bird Catches the
Leak"](https://arxiv.org/pdf/2409.20002)** demonstrates *Peeping Neighbor Attacks*:
probe with prompts containing guessed private attributes, watch TTFT, recover
other users' prompt semantics at **89% accuracy, FPR 0.05**.
([InputSnatch](https://arxiv.org/html/2411.18191v2) does the same for input
stealing.)

> **Direct Gatekeeper implication.** If Gatekeeper's job is preventing PII egress,
> a **cross-tenant cache reintroduces the leak through a side channel Gatekeeper
> cannot see** — timing alone leaks, no bytes required. **Per-tenant partitioning
> is not hardening; it is a correctness requirement**, and it is the one defense
> the literature says actually closes the hole.

This upgrades C1's "cross-tenant leakage is the cardinal sin" from a rule to a
*theorem*: it is now the only known-effective control against two independent
published attack classes. It is also why §7.2 rejects any store that can't
pre-filter by tenant inside the vector query. Isolation costs hit rate and storage.
**Pay it.**

Adopt vllm-sr's posture directly: scope isolation as a **tested invariant**
(`cache_scope_isolation_test.go`), not a config flag.

---

## 9. Threshold calibration — how we actually pick the number

### 9.1 The number is not portable

Read from `semantic-router` source (no published defaults table exists):

| Encoder | Default `score_threshold` |
|---|---|
| OpenAI (ada-002) | **0.82** |
| Cohere | **0.3** |
| HuggingFace | **0.5** |
| FastEmbed (bge-small-en-v1.5) | **0.5** |

**0.3 → 0.82 — a 2.7× spread for the same task, from changing the embedding model
alone.** A threshold is a property of the **(encoder, task, aggregation)** triple,
never a constant.

Compounding it, **units differ across the ecosystem**: RedisVL uses cosine
**distance [0,2]** (default 0.1); LiteLLM/Bifrost/LangCache use **similarity
[0,1]**; Vector Sets use **`(cos+1)/2`**. **Any threshold copied from a blog post
without its units and its encoder is meaningless.** We standardize internally on
**cosine similarity [0,1], higher = stricter**, and convert at the Redis boundary
(`VECTOR_RANGE = 1 - similarity`).

### 9.2 Methodology (adopted from RedisVL + semantic-router + MeanCache)

1. **Build a labeled pair set from our own traffic.** Two classes — and the second
   is the one that matters: (a) same intent, different wording; (b) **looks
   similar, must NOT share an answer**. Mine (b) from near-misses and from the L3
   judge queue: that is where every false hit lives. RedisVL encodes expected
   misses as `query_match: ""`; semantic-router's `fit()` "works best with many
   `None` utterances". **Calibrating on positives alone tunes straight to
   threshold 0.**
2. **Log similarity on every lookup**, hit and miss. Expect **bimodal**: >0.95
   genuine near-dupes, <0.85 unrelated. **The danger zone is 0.88–0.94** — that's
   the L3 sampling band.
3. **Sweep offline — it's cheap.** semantic-router's `threshold_random_search`
   does 500 iterations in **~1 second** by **embedding the corpus once and reusing
   the vectors across every iteration** (~419 it/s). A Go implementation does
   exactly this. Calibration need not be online or expensive.
4. **Do not optimize F1 or accuracy.** MeanCache maximizes F-score; semantic-router
   maximizes accuracy. **Both are wrong for a cache** — they price a false hit like
   a miss. A miss is a *cost*; a false hit is a *correctness bug*. **Fix an FPR
   budget and maximize hit rate subject to it** (vCache's framing, and the right
   one). Use F_β with β<1 if a scalar is needed.
5. **Know when to stop.** Once FPR floors at **3–5%** and won't move with the
   threshold, **you've hit the embedding model's separation limit** and further
   tuning is theater. Fix it by changing the model (§6), tightening L2 (§5), or
   narrowing scope (§3).

### 9.3 Starting values (to be replaced by calibration, not defended)

| Route class | Initial similarity | Rationale |
|---|---|---|
| Global default | **0.97** | Deliberately over-tight. respan.ai: 0.97 ⇒ ~5–10% hit, ~0.5% FP. Portkey (>250M requests): start 0.95, backtest ~5k queries, target >99% accuracy. **Start where a false hit is nearly impossible and loosen with evidence.** |
| Classification / moderation | **0.98** + L2 mandatory | Near-identical inputs legitimately deserve different labels. |
| FAQ / support | 0.95 | The archetypal fit. |
| Code | 0.90 | Category-Aware ([2510.26835](https://arxiv.org/html/2510.26835)) — ⚠️ position paper, projected not measured. |

Indicative curve (respan.ai — **vendor content, no methodology, directional
only**): 0.99 ⇒ 1–3% hit / <0.1% FP · 0.95 ⇒ 15–25% / 1–3% · 0.90 ⇒ 35–55% /
**7–15%**. The vendor spread (Portkey 0.95, Bifrost 0.85–0.90, respan 0.97) is
itself the lesson: **nobody can hand you this number.**

**Adaptive (S4, vCache-style):** per-entry threshold, not global — fit
`Pr(correct | similarity)` as a sigmoid via online BCE over `(similarity,
correctness)` pairs from L3. Its `Pr ≥ 1-δ` bound assumes **(i) i.i.d. prompts**
(false for real traffic — topic drift, bursts, deploys) and **(ii) a sigmoid
correctness curve**, and it **buys the bound with LLM calls**. Take the
*mechanism* and the FPR-budget framing; do not quote the guarantee.

---

## 10. Config surface

### 10.1 Per-route profile (extends C1's cache profile, staged draft→enforce)

```yaml
semantic_cache:
  enabled: false                      # DEFAULT OFF, per-route opt-in (§3)
  similarity_threshold: 0.97          # cosine similarity [0,1], higher = stricter
  ttl: 1h
  max_entries_per_tenant: 10000
  embedding_model: langcache-embed-v3-small   # in the key: model change ⇒ new namespace
  embedding_dim: 384
  verification:                       # L2 — the part that makes it correct
    entity_guard: true                # ⚠️ disabling is a documented FPR regression
    negation_guard: true
  conversation_history_threshold: 3   # >3 messages ⇒ refuse (Bifrost/Portkey)
  async_judge:
    enabled: true
    sample_band: [0.88, 0.97]         # the danger zone (§9.2)
    sample_rate: 0.05
  exploration_rate: 0.01              # probabilistic bypass — free ground truth
                                      # (GPTCache `temperature`; vCache τ̂)
```

### 10.2 Per-request headers (extending C1's `TAS-Cache`)

| Header | Effect | Borrowed from |
|---|---|---|
| `TAS-Cache: bypass\|off` | C1 — force fresh | Cloudflare |
| `TAS-Cache-No-Store` | serve from cache, don't write | LiteLLM `no-store` |
| `TAS-Cache-Max-Age: {s}` | **max acceptable hit age** — caller-side freshness | LiteLLM `s-maxage` |
| `TAS-Cache-Threshold: {f}` | per-request override, **clamped ≥ route floor** | Bifrost, LangCache |
| `TAS-Cache-Namespace` | additional scope *within* a tenant | LiteLLM, Portkey |

**Threshold overrides may only tighten, never loosen.** A client must not be able
to widen its own false-hit rate — that's a cache-poisoning surface (§8.1).

### 10.3 Structural refusals (not configurable)

Refuse to *store or serve* semantically, regardless of route config:

- Any request with `tools`/`functions` defined, or any `role=tool` message.
  → **LiteLLM [#28778](https://github.com/BerriAI/litellm/issues/28778)**: tool-call
  content destroyed under semantic cache (only the prompt string embedded,
  `role=tool` payloads collapse) → **infinite re-invocation**.
- `> conversation_history_threshold` messages. Long conversations → topic overlap →
  false positives.
- `stream: true` in S1 (see §15 S3).
- Tenant `payload_retention_mode = off` (§11).
- Inbound Gatekeeper **block** (no response to cache).

---

## 11. Privacy & retention

Inherits C1 §5 wholesale; three semantic-specific additions:

- **Embeddings are derived personal data.** A 384-float vector of a redacted
  prompt is still derived from user content and is subject to the same retention
  and right-to-be-forgotten rules as the body. A tenant purge must drop the
  tenant's **vectors and index entries**, not just the hashes. `FT.DROPINDEX` does
  not delete hashes; the purge must delete keys by prefix.
- **`retention=off` ⇒ semantic cache disabled.** No content-free fallback: unlike
  C1, the *embedding itself* is retained content. There is no content-free mode
  for a vector cache.
- ⚠️ **TTL is not the safety net it looks like.** RedisVL's `check()`
  **auto-refreshes TTL on matched entries (sliding window)** — so a hot *wrong*
  entry **never expires**. We use **absolute expiry from `created_at`, never
  refresh-on-read.** A false hit must age out on schedule precisely *because* it's
  popular.

---

## 12. Interaction with prompt caching — and a Go hazard

Semantic caching and vendor prompt caching **compose**: prefix caching cuts TTFT
on **misses**; semantic caching removes the call on **hits**. Drawing the line
(for the glossary, §16):

> **Prefix caching reuses *computation* for a byte-identical prefix and can never
> change the answer. Semantic caching reuses an *answer* for a merely-similar
> question and always can.**

> 🚨 **Go-specific hazard, straight from Anthropic's docs:** *"Some languages (Go,
> Swift) randomize JSON key order; normalize before comparison."* **A Go gateway
> that unmarshals and re-marshals a request can silently destroy the client's
> provider-side prefix-cache hits** — turning 0.1× reads back into full-price
> input, invisibly. This is a live risk in `tas-llm-router` **today**, independent
> of this feature. **Rule: canonical JSON serialization, or pass bytes through
> untouched.** Worth its own issue.

Corollary for key normalization (§8): stable JSON ordering is required for *our*
key anyway — same discipline, two payoffs.

---

## 13. Accounting & experiment awareness

Extends C1 §6/§7. `cache_state` gains a value; the C1 enum `{hit, miss, bypass}`
becomes **`{hit, semantic_hit, miss, bypass}`** — `semantic_hit` **must** be
distinguishable from `hit`, because one is provably correct and the other is
probabilistic. Collapsing them would hide the FPR.

- A `semantic_hit` stamps **`cache_similarity`** and **`cache_threshold`** on the
  event. Bifrost's `cache_debug` returns similarity + the threshold that produced
  the hit — **the single cheapest observability win available**, and it's what
  makes §9.2's distribution plot possible at all.
- CLEAR for a hit is the **cached** score (carried on the entry), not recomputed —
  hence `scoring_version` in the key and as a TAG.
- **Experiments: bypass by default** (C1 §7). A semantic hit wasn't produced by the
  assigned variant *and* wasn't produced by an identical prompt — doubly
  confounding. Guardrail queries filter `cache_state=miss`.
- **Cache savings** (§ and tokens avoided) split by `hit` vs `semantic_hit`, so the
  CFO story never rests on probabilistic hits.

---

## 14. Observability — non-negotiable

> **A semantic cache that silently never hits is indistinguishable from a working
> one.** LiteLLM [#29086](https://github.com/BerriAI/litellm/issues/29086):
> `redis-semantic` **never produced a single semantic hit** across versions
> 1.85.1–1.86.2 — the full request hash was used as a RediSearch pre-filter,
> making KNN unreachable. **Silently.** Then
> [#31610](https://github.com/BerriAI/litellm/issues/31610) reports the same
> symptom on v1.90.0 — **still open**. LiteLLM has **no built-in hit-rate metric**.
> These two facts are the same fact.

Minimum metric set (`/metrics`, + `cache_state` in the AIQG event stream):

| Metric | Why | Alert |
|---|---|---|
| `semantic_cache_hit_rate` by route/tenant | the #1 observed failure is silent zero | **alert on == 0** for an enabled route |
| `semantic_cache_similarity` **histogram** | see the bimodality + the 0.88–0.94 danger zone (§9.2) | — |
| `semantic_cache_false_hit_rate` (sampled, from L3) | **our only ground truth** | alert on > FPR budget |
| `semantic_cache_entry_age` | staleness | — |
| `semantic_cache_embed_latency` | miss-path overhead | p95 > 100ms |
| `semantic_cache_store_errors` | fail-open is silent by design | rate > 0 |
| L2 rejects by guard (entity/negation/freshness) | **proves L2 earns its keep** — and if it never fires, L1 is over-tight | — |

**Fail open, always.** Any cache error ⇒ treat as a miss and call the vendor.
Counter-example: LiteLLM
[#25962](https://github.com/BerriAI/litellm/issues/25962) — cache init failure
**crashes the proxy**.

### 14.1 The eval loop — the feature's actual operating cost

> **A semantic cache without an eval loop is a footgun.** FPR is not a number you
> measure once at calibration and file away — it drifts as the corpus, the traffic
> mix, and the underlying documents change. An entry that was correct in March can
> be wrong in July without anything about the cache changing.

The standing loop (this **is** L3, operationalized):

1. **Sample 1–5% of served semantic hits.** Replay the original prompt against the
   live vendor.
2. **Grade blind** — LLM judge, with human spot-checks on disagreements.
3. **Compute FPR weekly, segmented by intent class / route.** A global FPR hides a
   single catastrophic route.
4. **Periodically re-grade aged entries** for corpus drift, independent of new
   traffic.
5. **Breach the budget ⇒ auto-tighten the route's threshold** (S4), and alert.

**FPR budgets:** ~**2%** unregulated routes, ~**0.5%** regulated/compliance-bearing
routes. Given §1.1's 3–13% floor for threshold-only designs, **the 0.5% budget is
unreachable without L2 + L3** — which is the quantitative restatement of why §5 is
a cascade.

> **Budget this as permanent opex, not a launch task.** The eval loop costs vendor
> calls (replay) + judge tokens, forever, on every enabled route. A semantic cache
> whose eval loop gets disabled under cost pressure has silently become an
> unmonitored wrong-answer generator — and §1.2 says the cost saving it was
> protecting was ~$6–12/month. **If we won't fund the loop, don't ship the
> feature.**

**No OTel semantic convention exists for gateway-level semantic cache hits.** The
GenAI semconv work
([PR #197](https://github.com/open-telemetry/semantic-conventions-genai/pull/197),
**open**) adds `gen_ai.usage.cache_read.input_tokens` and
`gen_ai.token.cache = {uncached, read, write}` — but those are **provider-side
prompt caching**, a different thing (§12). **Our metrics are bespoke; don't wait
for the standard, and don't misuse those attribute names for this.**

---

## 15. Phasing

C4 in `AIQG-CACHING.md` §10 expands to:

| | Stage | Contents | Gate to advance |
|---|---|---|---|
| **S0** | **Infra prerequisite** | **Redis 8 upgrade** for `redis-shared` (or pgvector fallback) + **licensing sign-off** (§18 Q1) + TEI deployment w/ `langcache-embed-v3-small`. Fix `ollama.yaml`'s kustomization/model-pull gap regardless (§6). | `FT.CREATE` works; TEI serves 384-dim |
| **S1** | **Shadow mode** | Full cascade wired, **serving nothing**. Embed, search, run L2, log what *would* have hit + similarity. Zero risk, real distributions. Mirrors the shadow-reduction precedent already at `server.go:666`. | ≥2 weeks of similarity distributions + a labeled pair set |
| **S2** | **Calibration + enable** | Offline sweep (§9.2) → per-route thresholds. Enable on **one** qualifying route (FAQ). `EmbeddingsCache`. Full metrics + alerts. | hit rate > 0 **and** sampled FPR within budget |
| **S3** | **Accounting, UI, streaming** | `semantic_hit` split in Cache panel / Cost report / Traffic Explorer. Streaming replay — ⚠️ **vllm-sr [#913](https://github.com/vllm-project/semantic-router/issues/913): a cache hit breaks SSE (`!!!!...`)**; a real Go implementation hit this. LiteLLM re-chunks at 5 chars; Portkey just refuses. | — |
| **S4** | **Learning** | L3 async judge in the loop → per-entry adaptive thresholds (vCache-style, reimplemented) → auto-recalibration. **The differentiator nobody ships.** | — |

**S1 is the whole point of the phasing.** It buys the labeled data that §9.2
requires, at zero correctness risk, before a single user sees a semantic hit. We
already have the pattern in-tree.

---

## 16. Work breakdown

1. **aether-shared/k8s-shared-infrastructure** — Redis 8 upgrade (`redis.yaml`);
   TEI deployment + kustomization; fix `ollama.yaml` kustomization + model-pull +
   a readiness probe that fails when no model is loaded.
2. **tas-llm-router** — `internal/cache/` (does not exist today; copy the
   `pkg/aiqg/linkage/store.go` pattern: interface + `RedisStore`/`MemoryStore`,
   nil-safe, TTL'd, tenant-prefixed, graceful degrade). Cascade L0–L3 at
   `server.go:666`; key builder; canonical JSON (§12); headers; `cache_state`
   /`cache_similarity`/`cache_threshold` on events; **`cache_scope_isolation_test.go`
   equivalent** (§8).
3. **tas-llm-router `cmd/`** — offline threshold-sweep tool (§9.2 step 3: embed
   once, sweep 500× in ~1s).
4. **aiqg-dashboard-be** — `semantic_hit` in `/events` + `/metrics`; similarity
   histogram; FPR aggregation.
5. **aiqg-ui** — `semantic_hit` split in the Cache panel; similarity distribution
   chart (the danger band is a *visual* — show it).
6. **aether-shared/data-models/aiqg/** — `response-cache.md` gains a semantic
   section; `response-event.md` for the new fields.
7. **tas-aiqg** — **`AIQG_GLOSSARY.md` has no "semantic cache" entry** despite
   being the self-declared vocabulary source of truth (locked 2026-07-13) and
   despite #97 citing glossary terminology as its motivation. Add it with §12's
   one-liner. Cross-link primer §5.
8. **Issues to file** — (a) Go JSON key-order destroying vendor prefix-cache hits
   (§12) — **independent of this feature, live today**; (b) `ollama.yaml` not in
   kustomization + probe passes with no model.

---

## 17. Risks

| Risk | Mitigation |
|---|---|
| **False hits** — 3–13% floor for threshold-only designs; *Sapphire/Sapphire Reserve* @ **0.99** | The cascade (§5). L2 is why this doc exists. Never ship L1 alone. |
| **Cache poisoning** — 86.9% hijack, 78–88% vs Bedrock/Azure APIM ([NDSS'26](https://www.ndss-symposium.org/ndss-paper/when-cache-poisoning-meets-llm-systems-semantic-cache-poisoning-and-its-countermeasures/)) | Per-tenant isolation (the only defense that works); threshold floors; no client-side loosening (§10.2). |
| **Timing side channel** — PII recovery @ 89% *without reading the cache* | Per-tenant partitioning as a **correctness requirement** (§8.2). Hit-latency padding is the only *additional* mitigation — **nobody in the landscape ships it**, and it burns the latency win that §1.2 says is the whole point. Treat as a known, accepted residual within a tenant boundary; document it. |
| **Silent zero hit rate** — LiteLLM shipped this **twice** | Alert on hit-rate == 0; S1 shadow proves the path before enabling. |
| **Tool-call corruption** → infinite re-invocation (LiteLLM #28778) | Structural refusal (§10.3), not config. |
| **Stale answers outliving a policy/data change** | Absolute TTL, no refresh-on-read (§11); `scoring_version` in the key; tag invalidation. |
| **Embedding model swap silently invalidates semantics** | Model + dim in the key ⇒ new namespace. Bifrost handles this by convention; we make it structural. |
| **Redis 8 licensing (AGPLv3 arm)** | §18 Q1 — **resolve before S0**. pgvector fallback exists. |
| **Ollama/TEI as a request-path SPOF** | Fail open to a miss (§14); fix the manifest gaps (§16.1). |
| **Concurrent identical misses → N vendor calls** | `golang.org/x/sync/singleflight` on the miss path. **Nobody in the landscape does this** — cheap win. |
| **Overselling** — "cut your LLM bill with semantic caching" | §3 scope guard; per-route hit-rate metrics make a non-benefiting route *visibly* useless rather than silently stale. |

### 17.1 ⚠️ License hazards found in the survey

- **vCache is CC BY-NC-ND 3.0** — verified by decoding the LICENSE blob, not the
  GitHub label (the API reports only `NOASSERTION`). **NonCommercial *and*
  NoDerivatives, on a software repo.** We cannot ship it and cannot modify it.
  **Treat the paper as a spec; reimplement clean-room.**
- **Redis 8** is tri-licensed RSALv2 / SSPLv1 / **AGPLv3** — fine for internal
  cluster use; **must be checked against how we distribute TAS** (§18 Q1).
- vllm-project/semantic-router (Apache-2.0), Redis langcache embeddings
  (Apache-2.0), pgvector-go, hugot (Apache-2.0) — all clear.

---

## 18. Open questions

1. **Redis 8 licensing** — does the AGPLv3/RSALv2/SSPLv1 tri-license clear our
   distribution model? **Blocks S0.** If no → pgvector (§7.2), which also needs an
   image swap. *Owner: legal + platform.*
2. **Who owns the L3 judge's cost?** Krites is free on *latency*, not on *tokens*.
   A judge is an LLM call — sampled, but real. Budget it, or L3 gets disabled first
   thing under cost pressure and S4 never happens.
3. **Is 128 max-seq (v3-small) enough for our prompts?** If typical post-redaction
   prompts exceed it, we truncate — and truncation silently destroys the
   discriminative tail (*"…Sapphire **Reserve**"*). Measure in S1; escalate to
   `langcache-embed-v2` (8192 seq, Matryoshka 768→384) if not. **This may be the
   deciding factor over the dimension convenience.**
4. **L2 entity guard: rules or a model?** Start with deterministic
   number/date/entity extraction. Presidio-style NER is already in the Gatekeeper
   orbit — reuse, or keep L2 dependency-free and fast?
5. **Do we need semantic caching at all for TAS's actual traffic mix?** §3 excludes
   agentic/coding — which is much of what TAS runs. **S1 shadow mode answers this
   empirically and cheaply.** A defensible S1 outcome is *"hit rate is 2%, we're not
   shipping it"* — and that result is worth having, because it's the honest version
   of a claim competitors make loudly.

---

## 19. References

**Reference architecture:** [vllm-project/semantic-router](https://github.com/vllm-project/semantic-router)
(Apache-2.0, Go, Envoy ExtProc, cache + PII classifier) ·
[cache pkg](https://github.com/vllm-project/semantic-router/tree/main/src/semantic-router/pkg/cache) ·
[#913 SSE bug](https://github.com/vllm-project/semantic-router/issues/913)

**Embeddings:** [langcache-embed-v2](https://huggingface.co/redis/langcache-embed-v2) ·
[langcache-embed-v3-small](https://huggingface.co/redis/langcache-embed-v3-small) ·
["Advancing Semantic Caching for LLMs with Domain-Specific Embeddings and Synthetic Data" (2504.02268)](https://arxiv.org/abs/2504.02268) ·
[TEI](https://github.com/huggingface/text-embeddings-inference) ·
[hugot](https://github.com/knights-analytics/hugot)

**Correctness / research:** [vCache (2502.03771)](https://arxiv.org/abs/2502.03771) ⚠️ CC BY-NC-ND ·
[MeanCache (2403.02694)](https://arxiv.org/abs/2403.02694) ·
[Krites (2602.13165)](https://arxiv.org/abs/2602.13165) ·
[GroundedCache (2605.27494)](https://arxiv.org/abs/2605.27494) ·
[SCALM (2406.00025)](https://arxiv.org/abs/2406.00025) ·
[Category-Aware (2510.26835)](https://arxiv.org/html/2510.26835)

**Security:** [CacheAttack (2601.23088)](https://arxiv.org/html/2601.23088v2) /
[NDSS'26](https://www.ndss-symposium.org/ndss-paper/when-cache-poisoning-meets-llm-systems-semantic-cache-poisoning-and-its-countermeasures/) ·
[The Early Bird Catches the Leak (2409.20002)](https://arxiv.org/pdf/2409.20002) ·
[InputSnatch (2411.18191)](https://arxiv.org/html/2411.18191v2)

**Implementations:** [GPTCache](https://github.com/zilliztech/GPTCache) (dormant — last release 2024-08-01) ·
[RedisVL cache API](https://docs.redisvl.com/en/latest/api/cache.html) ·
[RedisVL LLM cache guide](https://docs.redisvl.com/en/latest/user_guide/03_llmcache.html) ·
[EmbeddingsCache](https://docs.redisvl.com/en/latest/user_guide/10_embeddings_cache.html) ·
[redis-retrieval-optimizer](https://github.com/redis-applied-ai/redis-retrieval-optimizer) ·
[LangCache](https://redis.io/docs/latest/develop/ai/context-engine/langcache/) (preview, Redis Cloud only, no Go SDK) ·
[LiteLLM caching](https://docs.litellm.ai/docs/proxy/caching) ·
[Portkey](https://portkey.ai/docs/product/ai-gateway/cache-simple-and-semantic) /
[thresholds](https://portkey.ai/blog/semantic-caching-thresholds/) ·
[Bifrost](https://docs.getbifrost.ai/features/semantic-caching) ·
[aurelio-labs/semantic-router](https://github.com/aurelio-labs/semantic-router)

**Storage / Go:** [Redis Go vector search](https://redis.io/docs/latest/develop/clients/go/vecsearch/) ·
[Redis vector queries](https://redis.io/docs/latest/develop/ai/search-and-query/query/vector-search/) ·
[Redis index expiration](https://redis.io/docs/latest/develop/ai/search-and-query/advanced-concepts/expiration/) ·
[Vector Sets README](https://github.com/redis/redis/blob/unstable/modules/vector-sets/README.md) (no per-element TTL) ·
[go-redis](https://github.com/redis/go-redis) · [pgvector-go](https://github.com/pgvector/pgvector-go) ·
[qdrant/go-client](https://github.com/qdrant/go-client)

**Line-drawing (§12):** [Anthropic prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
(incl. the Go JSON key-order warning) · [vLLM APC design](https://docs.vllm.ai/en/stable/design/prefix_caching/) ·
[SGLang RadixAttention (2312.07104)](https://arxiv.org/pdf/2312.07104) ·
[OTel GenAI semconv PR #197](https://github.com/open-telemetry/semantic-conventions-genai/pull/197) (open)

**Practitioner:** [respan.ai](https://www.respan.ai/articles/semantic-cache-llm) — *vendor content, no published methodology; directional only.*
