# AIQG — Freedom-to-Operate Claim-Read

**Status:** Technical FTO triage — 2026-06-19. Granted **independent** claims of
four patents read against the AIQG attribution design. **Not legal advice and
not a clearance opinion** — infringement turns on claim construction, doctrine
of equivalents, prosecution history, and validity, none assessed here. Engage
patent counsel before relying on any conclusion. Claim texts verified directly
from USPTO grant PDFs.
**Companion:** [`AIQG-PROVISIONAL-BRIEF.md`](./AIQG-PROVISIONAL-BRIEF.md)

## Techniques assessed

- **(a)** Content-propagation graph (Rabin chunking, TTL index, IDF-weighted,
  zero-header inference)
- **(b)** `tool_call_id` echo (deterministic flow linkage)
- **(c)** Prefix chaining (hash multi-turn prefix → conversation id)
- **(d)** 8-tier identity-confidence ladder + asserted-vs-inferred impersonation
  signal
- **(e)** Agent-type fingerprinting (13-signal ensemble, MinHash lineage)
- **(f)** Per-agent / per-flow cost attribution & metering at the gateway

## The four patents (verified identities)

| Patent | Title | Assignee | Priority | Grant | Independent claims |
|---|---|---|---|---|---|
| **US 12,483,411 B1** | Blockchain-based AI agent life-cycle management & authentication | Tyntre LLC | 2025-03-04 | 2025-11-25 | 1 (system), 11 (method) |
| **US 12,547,677 B1** | Anticipatory (JIT) authentication for account-specific AI agents | Airia LLC | 2025-05-16 | 2026-02-10 | 1 (method), 19 (CRM), 20 (system) |
| **US 10,924,326 B2** | Clustered real-time correlation of trace-data fragments (distributed transactions) | Dynatrace LLC | 2015-09-14 | 2021-02-16 | 1 (method), 15 (system) |
| **US 11,785,115 B2** | Request tracing | IBM Corp | 2021-09-27 | 2023-10-10 | 1 (system), 11 (method), 18 (CRM) |

**What each covers (one line):**
- **Tyntre** — blockchain-stored agent identity token + permissioned
  cross-session context-passing, identity token attached to each response.
- **Airia** — pause/resume agent execution to acquire **per-user** (not global)
  OAuth-style tool credentials via a credentials service.
- **Dynatrace** — APM tracing where each agent **stamps an explicit
  correlation-server ID + transaction ID into outgoing messages**; dependents
  add per-customer metric attribution + trace splitting at client boundaries.
- **IBM** — attribute spans to a specific process by correlating **socket/port
  connection data at a sidecar proxy** with span metadata, to disambiguate
  name-collisions.

## Risk matrix (patent × technique)

HIGH = independent-claim language plausibly reads on the technique; MEDIUM =
conceptual overlap but a claimed element likely missing; LOW = distant; NONE =
no meaningful read.

| Technique | Tyntre 12,483,411 | Airia 12,547,677 | Dynatrace 10,924,326 | IBM 11,785,115 |
|---|---|---|---|---|
| (a) Content-propagation graph | LOW | NONE | LOW | LOW |
| (b) `tool_call_id` echo | MEDIUM | LOW | **MEDIUM** | LOW |
| (c) Prefix chaining | LOW | NONE | LOW | LOW |
| (d) Identity ladder + impersonation | MEDIUM | LOW | LOW | NONE |
| (e) Agent-type fingerprinting | LOW | NONE | LOW | **MEDIUM** |
| (f) Per-agent metering | LOW | LOW | **MEDIUM** | NONE |

**No HIGH-risk read on any technique as the design is described.**

## Key rationales (tied to claim language)

- **(a) is clear of all four.** Dynatrace and IBM both require an explicit
  propagated identifier or socket-connection observation; the zero-header
  content-fingerprint mechanism is the opposite. Tyntre requires content on a
  blockchain with an attached token. No claim recites content-similarity
  matching.
- **(b) vs Dynatrace (MEDIUM)** — Dynatrace claim 1 covers an agent
  *propagating* a transaction id into a message and a downstream agent
  *retrieving* it. A `tool_call_id` reappearing is functionally id-in-payload
  linkage. **Gap that saves us:** Dynatrace requires an instrumented agent that
  *deliberately appends* the id plus a correlation-server *selection/routing*
  scheme; the gateway here passively observes an id the model echoed and does no
  server selection.
- **(b)/(d) vs Tyntre (MEDIUM)** — same problem space (agent identity, carrying
  context across sessions) but every Tyntre independent claim **requires a
  blockchain and an attached agent identity token**, which the design uses
  neither of.
- **(e) vs IBM (MEDIUM)** — IBM disambiguates same-labeled services by
  *characteristics*; same goal as agent-type fingerprinting. **Gap:** IBM
  requires those characteristics from **socket-connection correlation at a
  network traffic component (sidecar proxy)**; the 13 signals here are
  L7-semantic at an API gateway.
- **(f) vs Dynatrace (MEDIUM)** — Dynatrace dependents 3/4/17 attribute
  per-transaction metrics per **customer** via appended customer id + trace
  splitting. **Gap:** the design attributes cost to *agent/flow* via *inference*
  from the gateway ledger, not to a *customer* via a *propagated* correlation
  id.

## Overall posture

| Patent | Posture |
|---|---|
| Tyntre 12,483,411 | Low-to-moderate — clear if no blockchain + no identity token attached to responses. |
| Airia 12,547,677 | Low — out of scope (JIT per-user tool auth). |
| **Dynatrace 10,924,326** | **Moderate — the one to take most seriously.** Broad, 2015 priority, sophisticated assignee, expires ~2035. |
| IBM 11,785,115 | Low-to-moderate — clear if no socket/port correlation at a sidecar proxy. |

**Clear:** (a) content-propagation graph, (c) prefix chaining.
**Lightly encumbered, defensible on a missing element:** (b), (d), (e), (f).

## Design-arounds for the MEDIUM risks

**vs Dynatrace (priority target):**
1. Keep flow linkage **inference-only** — never inject a TAS-generated
   correlation id into outgoing requests; rely on the model-produced
   `tool_call_id` echo and content propagation.
2. Do **not** implement a correlation-server *listing + selection/routing*
   scheme.
3. For (f), bill from the gateway's **own ledger keyed on inferred identity** —
   not a customer id appended to messages — and avoid "trace splitting at
   client-control boundaries" as a mechanism.

**vs IBM (for (e)):**
4. Keep fingerprinting at **L7/application semantics** (toolset hash, prompt
   template, JSON schema, call-graph, timing); **never** correlate raw
   socket/port to process at a sidecar proxy.

**vs Tyntre (for (b)/(d)):**
5. Do **not** store agent identity tokens / permissions / context on a
   blockchain, and do **not** attach an agent identity token to served
   responses. Frame impersonation detection as statistical mismatch, not token
   verification against a ledger.

**vs Airia:**
6. No change needed; just don't add a pause/resume-on-missing-credential JIT
   per-user tool-auth flow to the gateway.

## Recommendation

Commission a **counsel-led opinion focused on Dynatrace 10,924,326** and on
techniques (b) and (f) **before any billing-grade per-agent metering ships**.
The other three patents are narrower (blockchain identity, JIT auth,
socket-proxy tracing) and the design differs on a load-bearing element of each.
Observability-only use (no billing) carries materially lower exposure.
