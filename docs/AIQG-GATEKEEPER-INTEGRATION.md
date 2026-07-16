# Gatekeeper — Integration Design (redaction + MCP scanning)

Status: **Design / for review** — NOT implemented. Spans `tas-llm-router`,
`tas-mcp`, and `Gatekeeper`.

**Precondition for** [`AIQG-CACHING.md`](AIQG-CACHING.md) (C1) and
[`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md) (C4) — both specify caching
the "post-scan (redacted) prompt", and that redaction does not exist (§1.1).
Related: [`AIQG-PROMPT-CACHE-CONTROL.md`](AIQG-PROMPT-CACHE-CONTROL.md) §11.1
(where this was found), `tas-aiqg/AIQG_CACHE_SAFE_REDUCTION.md` (reduce-at-source
— the pattern §6 extends).

---

## 1. Problem — the integration surface is smaller than the documentation

Verified 2026-07-16 across the monorepo:

| Service | `Gatekeeper/CLAUDE.md` claims | Actually imports | Actually does |
|---|---|---|---|
| **tas-llm-router** | ✅ "linked" | `pkg/pipeline`, `pkg/scan`, `pkg/extract`, `pkg/types` | **Scans → blocks/reports. Never redacts** (§1.1) |
| **tas-mcp** | ✅ "MCP Proxy (linked)", with its own integration section | **`pkg/extract` only** | **Reduction. No scanning at all** — it uses Gatekeeper as a text reducer, not a gatekeeper |
| **audimodal** | ✅ listed | — | **Nothing.** Not in `go.mod` |
| **aether-be** | ✅ listed | — | **Nothing.** Not in `go.mod` |

So the "unified content scanning layer shared across TAS services" is **one
service, and it doesn't redact there**. The `tas-mcp` import is Plan #7
reduce-at-source (`internal/reduction/reducer.go` → `pkg/extract`), not a scan
path — an easy thing to misread as an integration, which is likely how the docs
drifted.

### 1.1 The router scans but never redacts

Full chain in [`AIQG-PROMPT-CACHE-CONTROL.md`](AIQG-PROMPT-CACHE-CONTROL.md)
§11.1. In short: `ScanMessages` extracts message text into a **separate string**
to scan; the handler then blocks (403), sets `X-TAS-Scan-Status`, and stamps
finding counts — it **never rewrites `req.Messages`** (`server.go:641-679`). The
pipeline's only content-mutating path is a Databunker tokenizer that is **nil and
disabled** (`gatekeeper.go:149`, `default_processor.go:184`). Content reaches the
vendor byte-for-byte as sent.

### 1.2 Why the tokenizer was never wired — the actual reason

Not an oversight. **The client is complete; the server was never deployed.**

- `Gatekeeper/pkg/tokenize/client.go` implements the full `Tokenizer` interface —
  `Tokenize`, `TokenizeBatch`, **`Detokenize`**, `GetSecret`, `LogAudit`.
- **Databunker is not deployed** — no Deployment or Service in any namespace
  (verified against the live cluster).

So the code path is dead because its dependency doesn't exist. That reframes §4:
the question isn't "why didn't we wire it", it's "do we want to run Databunker".

### 1.3 Why this matters now

Three reasons, in descending order of force:

1. **It is a caching precondition, and the docs currently state it as a fact.**
   C1 and C4 both promise the cache "never keys on or stores raw PII" *because*
   the prompt is redacted. Today `post-scan == pre-scan`, so a cache built to
   those docs **stores raw PII bodies at rest in Redis** while documenting the
   opposite. Caching cannot ship before this resolves.
2. **Tool results are the untrusted-input surface, and nothing scans them.**
   Gatekeeper's own design puts MCP responses at `TierExternal`. `tas-mcp`
   federates a dozen servers; their output flows straight into LLM prompts.
   That is the prompt-injection and third-party-PII path, and it is unscanned.
3. **The compliance story is thinner than it reads.** "Scanning + blocking on
   the LLM path" is real. "Unified content scanning across the platform,"
   "tokenization via Databunker," and "MCP Proxy integration" are not.

---

## 2. Scope

**In:** wiring redaction in the router (Part A); Gatekeeper scanning in `tas-mcp`
(Part B); attestation so the two don't double-scan (Part C); the cache
interaction (§6).

**Out:** audimodal / aether-be integration (they have none; separate work, no
caching dependency). Rewriting `Gatekeeper/CLAUDE.md` — a docs fix, tracked
separately (§10.4).

---

## 3. Part A — the router must redact, and redaction is lossy

**This is the decision the whole design turns on.** Redaction is not a filter you
switch on; it changes what the model sees, and therefore what it can answer.

> Redact `customer@acme.com` → `[EMAIL]`, then ask *"what company is this
> customer from?"* The answer is now unknowable. The scan succeeded and the
> product broke.

That is why `pkg/tokenize` exists, and why it is the harder, better path.

### 3.1 The two strategies

| | **Lossy redaction** (`pkg/scan`) | **Tokenize + detokenize round-trip** (`pkg/tokenize`) |
|---|---|---|
| Mechanism | Replace PII with a mask/placeholder/hash before the vendor call | Replace with a vault token inbound; **restore on the way out** |
| Model utility | **Degraded** — the value is gone | **Preserved** — the model reasons over a stable token, and the caller gets the real value back |
| New infra | **None** — `ScanWithRedaction` + `RedactionEngine` already exist | **Databunker must be deployed** (§1.2) |
| Determinism (cache-safety) | ✅ **Verified** — every strategy is content-derived; `generateToken`/`generateHash` are SHA-256 of the value (`redaction.go:295,302`), placeholders are a static map, masking is character-derived | ⚠️ **Unverified** — vault-assigned tokens; stable-per-value or per-call is unknown until deployed (§10 Q2) |
| Reversible | ❌ Never | ✅ With authorization + audit (`Detokenize`, `LogAudit`) |
| Right for | Routes where PII is **incidental** — it appears in the text but isn't the subject | Routes where PII **is** the subject — support, records, anything user-specific |

**Neither is universally correct**, which is why this is per-route config, not a
global switch — the same policy-as-config shape as everything else in AIQG.

### 3.2 Recommendation: ship lossy first, per-route, default off

- **A1 — lossy redaction, opt-in per route.** Unblocks C1/C4 with **zero new
  infrastructure**, using machinery that already exists and is already verified
  deterministic. Start on routes where PII is incidental.
- **A2 — tokenize round-trip.** Requires a Databunker deployment, a detokenize
  step on the outbound path, and a key-custody story. Real work; do it when a
  route needs PII preserved.

**Default `off`.** Redaction that silently degrades an answer is worse than no
redaction — the failure is invisible to the caller, who just gets a subtly worse
response. Turning it on must be a decision with a known blast radius.

### 3.3 Where it goes

```
handleChatCompletion
  → inbound scan            server.go:641   ← exists
  → BLOCK on policy         server.go:648   ← exists
  → REDACT (A1/A2)                          ← NEW: rewrite req.Messages
  → cache lookup / vendor call
  → outbound scan           server.go:926   ← exists (block-only today)
  → DETOKENIZE (A2 only)                    ← NEW: restore before returning
```

**Redact after the block decision**, not before: blocking policy should see the
real content, and a blocked request has nothing to redact.

⚠️ **A2's detokenize step is the hard half.** The outbound path must restore
tokens **in a streamed response**, where a token may straddle a chunk boundary.
That is a real streaming-buffer problem — and it is why A2 is not v1.

---

## 4. Part B — Gatekeeper in the MCP proxy

**Tool results are `TierExternal` and are not scanned.** This is the larger
security gap; Part A is the larger *correctness* one.

### 4.1 The hook point already exists

`tas-mcp` already imports Gatekeeper and already has the tool-result text in
hand at the exact right moment:

```
federation → ResultProcessor
  → reducing_processor.go:128   p.reducer.Reduce(ctx, content, query)
                                 └── internal/reduction/reducer.go → pkg/extract
```

`Reduce` receives the raw tool-result `content`. **Scanning belongs on the same
call path**, in the same package that already owns the Gatekeeper dependency —
`internal/reduction` is already the seam that keeps `pkg/extract` out of the
federation layer (its own doc comment says so). Adding a scan there needs no new
architecture, only `pkg/scan` alongside `pkg/extract`.

### 4.2 Scan before reduce, redact at source

**Order: scan → redact → reduce.**

- **Scan before reduce.** Reduction is lossy; scanning the reduced text would
  under-report — you'd miss PII that reduction dropped, and lose a finding that
  compliance wants recorded even when the content never reached the model.
- **Redact at source, once.** This is exactly the reduce-at-source discipline
  from `AIQG_CACHE_SAFE_REDUCTION.md`, applied to redaction: the tool result
  enters the conversation **already redacted**, gets cached in that form, and is
  never re-edited. Redacting the whole window later would re-edit cached content
  and break the prefix every turn — the same losing move that doc already
  documents for reduction.

> **Redact-at-source is cache-safe for the same reason reduce-at-source is: you
> only ever touch content *before* it is first cached.** One pattern, two uses.

### 4.3 Per-server trust

Gatekeeper's tiers map directly onto federation: a first-party MCP server is
`TierPartner`; an arbitrary federated one is `TierExternal`. `tas-mcp` already
has per-server config (per-server `spec.reduce` opt-in), so per-server
`scan`/`trust_tier` follows the established shape.

---

## 5. Part C — attestation, so we don't scan twice

`Gatekeeper/pkg/attest` exists and the router already sets
`HonorAttestations: true` (`gatekeeper.go:101`) — but **`EnableAttestation =
false // Simplified for now`** (`gatekeeper.go:144`), so nothing ever mints one.
The honor path is live with nothing to honor.

Turning it on is what makes Part B affordable: MCP scans a tool result and mints
a signed attestation; the router honors it and skips re-scanning the same bytes
when they arrive in the next prompt. Gatekeeper's own target is **>80% of content
skipped via valid attestation** — unreachable while minting is off.

**Prerequisite:** attestation signs with a key from Databunker
(`signing_key_name: tas-scan-signing-key`, `GetSecret`) — which is **not
deployed** (§1.2). So Part C needs either a Databunker deployment or a different
key source. **Attestation and A2 share that blocker**, which is the main argument
for deploying Databunker once rather than working around it twice.

---

## 6. Interaction with caching

| Concern | Resolution |
|---|---|
| **C1/C4's privacy premise** | Part A satisfies it. Until then, both docs' "we never store raw PII" is **false**, not merely optimistic (§1.1). |
| **Cache-safety of redaction** | A1 is safe — deterministic and content-derived (verified). A2 is **unknown** until Databunker is deployed (§10 Q2): per-call vault tokens would change the prompt bytes on every request and break *both* the vendor prompt cache and our response cache. |
| **Hit rate** | Redaction **raises** it: it normalizes away the PII variance that otherwise makes each prompt unique. This is why C4's S1 shadow measurement should run **with redaction wired**, or it understates the ceiling. |
| **Prompt caching** (`AIQG-PROMPT-CACHE-CONTROL.md`) | Unaffected either way — it sends bytes and stores nothing our side. Only *response* caching depends on Part A. |
| **P0 probe** | Unaffected today (nothing mutates the prompt, so its hash is exact). **A1 changes that**: once redaction lands, hash post-redaction, or the probe measures a prefix we no longer send. |

---

## 7. Phasing

| | Stage | Contents | Gate |
|---|---|---|---|
| **G1** | **Router redaction, lossy** | Wire `ScanWithRedaction` post-block; per-route strategy config; **default off**; stamp `redaction_applied` + counts on the AIQG event. No new infra. | A route runs redacted with no answer-quality regression |
| **G2** | **MCP scanning** | `pkg/scan` in `internal/reduction`; scan → redact → reduce at `reducing_processor.go:128`; per-server trust tier; findings to the existing stream. | Tool-result findings visible per server |
| **G3** | **Databunker + attestation** | Deploy Databunker; `EnableAttestation = true`; MCP mints, router honors → skip rate. Unblocks G4. | Skip rate > 0; measured |
| **G4** | **Tokenize round-trip** | `WithTokenizer` + `EnableTokenization`; outbound detokenize incl. **streaming** (§3.3); authorization + `LogAudit`. | PII-subject route preserves answer quality |
| **G5** | **Docs** | Correct `Gatekeeper/CLAUDE.md` to the real surface (§1). | — |

**G1 alone unblocks C1/C4** — it's the only hard caching dependency, and it needs
nothing deployed. G2 is the security win. G3/G4 are the "do we run Databunker"
decision, and should be taken as one.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| **Redaction silently degrades answers** (§3) — the caller can't see it | Default off; per-route opt-in; stamp `redaction_applied` on the event so a quality regression is attributable, not mysterious |
| **A2 detokenize on streamed responses** — tokens straddling chunks | Real work; deferred to G4. Do not attempt in v1 |
| **Databunker becomes a request-path dependency** | It gates A2 + attestation, not A1. A1's whole appeal is needing nothing. If Databunker is in the path, it needs the availability story the current design has never had to make |
| **Databunker token determinism unknown** (§10 Q2) | Verify **before** G4 — per-call tokens break both caches |
| **Scanning every tool result costs latency** | Attestation (G3) is the answer — Gatekeeper targets >80% skip. Until then, G2 pays full scan cost on every result |
| **Redacting the wrong thing at source** is unrecoverable | MCP redaction happens before content enters the conversation; a false positive silently removes real data. Start `TierExternal`-only, log findings before enforcing (the same draft→enforce staging as policy) |

---

## 9. Work breakdown

1. **tas-llm-router** — G1: redact post-block in `handleChatCompletion`; route
   config; `redaction_applied` on the event. G3: `EnableAttestation`. G4:
   `WithTokenizer` + outbound detokenize + streaming.
2. **tas-mcp** — G2: `pkg/scan` in `internal/reduction`; scan→redact→reduce at
   `reducing_processor.go:128`; per-server trust tier.
3. **aether-shared/k8s-shared-infrastructure** — G3: Databunker deployment +
   key custody.
4. **Gatekeeper** — G5: correct `CLAUDE.md`; consider a nil-tokenizer guard in
   `tokenizeFindings` (today it's guarded only at the call site,
   `default_processor.go:184` — defensible, but the function itself would panic
   if ever called directly).
5. **aether-shared/data-models/aiqg/** — `response-event.md` for
   `redaction_applied`.

---

## 10. Open questions

1. **Do we run Databunker?** It gates A2 *and* attestation (§5) — two features,
   one dependency. Deploying it once is likely cheaper than working around it
   twice, but it is a new stateful, key-custody-bearing service in the request
   path. **This is the decision; G1 deliberately doesn't wait on it.**
2. **Databunker token determinism** — stable per value, or fresh per call? Per-call
   breaks the vendor prompt cache, our response cache, and the linkage prefix
   index. **Verify before G4.** (The in-band `pkg/scan` path is confirmed safe.)
3. **Which redaction strategy per route?** `mask` keeps shape (`a***@b.com` — a
   model can still infer the domain), `replace` is strongest but most lossy,
   `hash` is stable and comparable but unreadable. Probably per-PII-type, not
   per-route.
4. **Does `Gatekeeper/CLAUDE.md` describe an aspiration or a plan?** If audimodal
   and aether-be integration is genuinely wanted, that's a roadmap item. If not,
   the doc should stop claiming it. Either way it shouldn't read as done (§1).
5. **Should redaction be enforced, or observed first?** Recommend log-only for
   one window per route (draft→enforce, matching policy staging) — the same
   discipline that made the P0 measurement worth trusting.
