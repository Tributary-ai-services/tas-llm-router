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
| **audimodal** | ✅ listed | — (not in `go.mod`) | **Its own DLP** — `pkg/dlp`, 17 files / 4,736 lines, live in `cmd/dlpworker` + `cmd/assembler`. Gatekeeper was **extracted from it** (§4a) |
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

## 2. Where scanning belongs — the rule

> **Scanning happens at the proxies and at ingest. Nowhere else.**

Those are the trust boundaries — where content crosses from untrusted to
trusted, or from trusted out to a third party. Everything behind them is
**interior** and must not re-scan; it **honors an attestation** instead (§5).

| # | Boundary | Service | Direction | Today |
|---|---|---|---|---|
| 1 | **LLM proxy** | `tas-llm-router` | prompt in, vendor egress out | **Scans + blocks. Never redacts** (§1.1) → **G1** |
| 2 | **MCP proxy** | `tas-mcp` | **ingress ← federated tools** (`TierExternal`) | **Nothing.** `pkg/extract` only — a reducer, not a gatekeeper → **G2** |
| 3 | **Ingest** | `audimodal` | **ingress ← customer documents** | Own DLP: 5 matchers, **no secrets detection** → **G6/G7** |

Everything else — `aether-be`, `tas-agent-builder`, `tas-workflow-builder`,
`tas-mcp-servers` — is **interior by design**. Content reaching them has already
crossed a boundary. Scanning there is duplicate work on the hot path and a second
place for a matcher to disagree with itself.

**This makes the three tracks one architecture rather than three chores.** Each is
a boundary that is currently either unguarded, or guarded weaker than the library
we already own:

- **The MCP proxy is the only *completely* unguarded boundary** — and it is the one
  taking `TierExternal` input from a dozen federated servers directly into LLM
  prompts.
- **Ingest is guarded but blind to secrets** — 5 matchers, zero credential
  detection (§4a.1).
- **The LLM proxy detects but cannot remediate** — it blocks or reports, never
  redacts, which is exactly what C1/C4 assume it does.

It also settles the aether-be question earlier revisions left open: **aether-be
should not integrate Gatekeeper.** It is not a boundary; the correct posture is to
trust the attestation minted upstream.

> **Which is why attestation (§5) is load-bearing, not an optimization.** Without
> it, "scan only at the boundary" has no enforcement mechanism — interior services
> must either re-scan (defeating the rule) or trust nothing (defeating the point).
> **G3 is what turns this rule from a convention into an invariant.** That raises
> its priority above where §7 currently places it (§10 Q1).

### 2.1 Scope

**In:** G1 (LLM proxy redaction) · G2 (MCP proxy scanning) · G6/G7 (ingest
migration + secrets) · G3 (attestation — the mechanism) · G4 (tokenize
round-trip).

**Out:** aether-be and every other interior service — **by design, not by
deferral**. Rewriting `Gatekeeper/CLAUDE.md` is a docs fix (G5, §10.4).

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

## 4a. Part D — audimodal: a migration, not an integration

**audimodal is the one service that already has real DLP.** "Integrating
Gatekeeper" here means **retiring `pkg/dlp` in favour of Gatekeeper** — a
consolidation with regression risk, not an addition.

- `audimodal/pkg/dlp` — **17 Go files, 4,736 lines**: `patterns/`, `compliance/`,
  `scanner/`, `service.go`, `interfaces.go`, `types/`.
- **It is live**, wired into two binaries: `cmd/dlpworker` (a dedicated DLP
  worker) and `cmd/assembler`.
- It has a real test suite encoding expected behaviour — `ssn_test`,
  `creditcard_test`, `hipaa_test`, `gdpr_test`, `pci_test`, `ccpa_test`, plus
  benchmarks.
- **Gatekeeper was extracted *from* it.** `Gatekeeper/CLAUDE.md` §"Migration from
  Existing Code" literally says to take the DLP pipeline and PII patterns from
  audimodal. This is the parent, not a new consumer.

### 4a.1 The migration is a coverage *gain* — verified, not assumed

The obvious fear is that swapping a working 4.7k-line DLP for the library
extracted from it loses detections. **The opposite is true:**

| | audimodal `pkg/dlp` | Gatekeeper |
|---|---|---|
| **PII matchers** | **5** — ssn, credit_card, email, phone_number, ip_address (`patterns/matchers.go`) | **21** (`configs/rules/pii.yaml`) |
| **Also detects** | — | bank_account, passport, drivers_license, date_of_birth, address, name, medical_record_number |
| **Secrets** | **none** | aws_access_key, aws_secret_key, azure_key, gcp_key, api_key, jwt_token, oauth_token, private_key, connection_string |
| **Compliance** | ccpa, gdpr, hipaa, pci-dss | + sox, soc2, eu_ai_act, iso_27001, nist_ai_rmf, nist_csf |
| **Injection** | **none** | ✅ `injection.yaml` |

Gatekeeper is a **strict superset** of audimodal's matcher set.

> **The sharpest argument isn't consolidation — it's the secrets gap.** audimodal
> ingests customer documents and **cannot detect a cloud credential in one**. No
> AWS key, no private key, no connection string. A customer uploading a config
> file, a `.env`, a runbook, or a support export puts credentials through a
> pipeline that has no matcher for them. Gatekeeper has had those patterns the
> whole time; audimodal just never got them, because the extraction went one way
> and never came back.

### 4a.2 What the migration must not break

- **Behaviour parity on the 5 shared matchers.** audimodal's tests encode the
  current contract (SSN/credit-card edge cases, framework mappings). **Port the
  test suite and run it against Gatekeeper** — it is the migration's acceptance
  gate, and it is the reason to do this incrementally rather than by deletion.
- **False-positive rate.** Going 5 → 21 matchers on document content will surface
  new findings. On document ingestion that means new *blocks* or *quarantines*.
  **Run the new matchers in log-only mode first** (the draft→enforce staging used
  everywhere else in AIQG), or a routine upload starts failing on a `name` or
  `address` match.
- **Throughput.** `pkg/dlp` has benchmarks; document ingestion is bulk, unlike
  the router's per-request path. ⚠️ Gatekeeper's speed story is Hyperscan, but
  **prod builds `nohs`** (the regexp engine) — so the migration must be
  benchmarked against `pkg/dlp` **as actually built**, not against the Hyperscan
  number in Gatekeeper's docs.
- **The `dlpworker` binary is a product surface**, not just a library call. Its
  interface and outputs may be depended on downstream.

### 4a.3 Direction: adapter first, deletion later

Do **not** rip out `pkg/dlp`. Put Gatekeeper behind audimodal's existing
`dlp.Interfaces` seam (it already has one), run **both** engines in shadow on
real ingestion traffic, diff the findings, and retire `pkg/dlp` only once the
diff is understood. The shadow step is cheap and it is the only way to learn the
false-positive delta before it blocks a customer upload.

This mirrors the discipline that has already paid twice in this workstream: the
P0 probe measures before enabling, and C4's S1 shadows before serving.

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
| **G6** | **audimodal: shadow** | Gatekeeper behind audimodal's `dlp.Interfaces` seam; run **both** engines on real ingestion; diff findings; port `pkg/dlp`'s test suite as the parity gate; benchmark **`nohs`-built** vs `pkg/dlp`. | FP delta understood; parity on the 5 shared matchers |
| **G7** | **audimodal: cut over** | Enable the 16 new matchers **log-only** first (secrets especially), then enforce; retire `pkg/dlp`. | No routine-upload regression |

**One track per boundary (§2), plus the mechanism:**

| Boundary | Track | Needs deployed | Closes |
|---|---|---|---|
| **LLM proxy** | **G1** | nothing | Detects but can't remediate; **unblocks C1/C4** (its only hard dependency) |
| **MCP proxy** | **G2** | nothing | The **only completely unguarded boundary** |
| **Ingest** | **G6/G7** | nothing | **No secrets detection** on customer documents |
| *(mechanism)* | **G3** | **Databunker** | Makes "scan at the boundary, honor inside" **enforceable** rather than advisory (§2) |
| *(utility)* | **G4** | **Databunker** | Redaction without losing the answer (§3) |

**The three boundary tracks are independent and need nothing deployed** — start
any of them today, in any order. **G3 is the one that makes them an
architecture** instead of three good deeds, and it is also what makes G2
affordable (Gatekeeper targets >80% skip via attestation; until G3, G2 pays a
full scan on every tool result). Only G3→G4 is hard-ordered.

> **Priority note.** §7's ordering predates §2's rule. Now that attestation is
> the enforcement mechanism and not a perf tweak, **G3 has a stronger claim than
> its position here suggests** — but it is gated on the Databunker decision
> (§10 Q1), which is why the boundary tracks are deliberately built to not wait
> for it.

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
3. **audimodal** — G6: Gatekeeper behind the `dlp.Interfaces` seam; dual-run +
   finding diff; port `pkg/dlp` tests as the parity gate; benchmark `nohs` vs
   `pkg/dlp`. G7: enable the 16 new matchers log-only → enforce; retire
   `pkg/dlp`. (Add `Gatekeeper` to `go.mod` — it isn't there today.)
4. **aether-shared/k8s-shared-infrastructure** — G3: Databunker deployment +
   key custody.
4. **Gatekeeper** — G5: correct `CLAUDE.md`; consider a nil-tokenizer guard in
   `tokenizeFindings` (today it's guarded only at the call site,
   `default_processor.go:184` — defensible, but the function itself would panic
   if ever called directly).
5. **aether-shared/data-models/aiqg/** — `response-event.md` for
   `redaction_applied`.

---

## 10. Open questions

1. **Do we run Databunker?** — **the decision, and §2 raises the stakes.** It gates
   A2 *and* attestation. Attestation is no longer a performance optimization: it
   is the **enforcement mechanism for "scan at the boundary, honor inside"**
   (§2). Without it, the architecture is a convention that interior services can
   only follow by re-scanning or by trusting blindly.

   So the real question is narrower than "do we want tokenization": **does the
   boundary rule get a mechanism?** If yes, Databunker (or another signing-key
   source — attestation needs `GetSecret`, not the vault's tokenization) becomes
   a platform dependency. If no, the rule stays advisory and §2's table is a
   description of intent rather than an invariant.

   **G1, G2, and G6/G7 all stand on their own without it** — each closes a real
   gap at its own boundary. G3 is what makes them a *system*.
2. **Databunker token determinism** — stable per value, or fresh per call? Per-call
   breaks the vendor prompt cache, our response cache, and the linkage prefix
   index. **Verify before G4.** (The in-band `pkg/scan` path is confirmed safe.)
3. **Which redaction strategy per route?** `mask` keeps shape (`a***@b.com` — a
   model can still infer the domain), `replace` is strongest but most lossy,
   `hash` is stable and comparable but unreadable. Probably per-PII-type, not
   per-route.
4. ~~**Does `Gatekeeper/CLAUDE.md` describe an aspiration or a plan?**~~ ✅
   **Answered by §2: proxies and ingest, nowhere else.** So audimodal is **in**
   (it *is* the ingest boundary — G6/G7), and **aether-be is out by design**, not
   deferred. The doc should say that: not "shared across TAS services" (which
   reads as everywhere, and is wrong in both directions), but *"enforced at the
   three trust boundaries; interior services honor attestations."* G5.
5. **Should redaction be enforced, or observed first?** Recommend log-only for
   one window per route (draft→enforce, matching policy staging) — the same
   discipline that made the P0 measurement worth trusting.
