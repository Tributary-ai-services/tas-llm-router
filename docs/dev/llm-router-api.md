---
doc_type: dev
audience: "Backend engineer integrating with or extending this gateway; reads Go, new to this repo"
assumes: ["HTTP APIs", "OpenAI or Anthropic SDK basics", "Go module layout"]
answers:
  - "What problem does this solve that I should not solve myself?"
  - "What are the core abstractions and how do they relate?"
  - "What is the shortest path to a working call?"
  - "What is the complete set of errors I can receive, and which are retryable?"
  - "Can I point a stock OpenAI or Anthropic SDK at this, and what changes if I do?"
  - "What are the compatibility guarantees — what may change without warning?"
  - "Where do I add behaviour, and what is deliberately closed?"
  - "Why is it designed this way rather than the obvious alternative?"
depth: deep
verified_against: "tas-llm-router@39e8d77, live gateway 2026-08-24"
---

# LLM Router — Developer Guide

> **Verified 2026-08-24 against `tas-llm-router@39e8d77`** and live probes against
> `gateway.aiqg.tas.scharber.com`. Responses shown are real captures. Where a
> behaviour was read from source rather than observed, it says so.

## Why this exists

This gateway is the AI Quality Gateway (AIQG) ingress — the name appears
throughout the codebase (`wrapAIQG`, `aiqg_headers.go`), in error codes, and in
the deployment name `llm-router-aiqg`. AIQG is the governance layer; the router
is what it governs.

**Vocabulary used below**, defined here because none of it is guessable:

- **surface** — a wire dialect (`/v1/chat/completions`, `/v1/messages`), not a pipeline
- **finding** — one result from scanning a prompt: a matched pattern with a severity
- **bundle** — the set of patterns a tenant's policy enables, and what each should do
- **tier** — a named routing target in a fallback chain, typically a model with its
  own context window; tiers are ordered by the operator, and "advancing a tier"
  means retrying the identical request against the next one
- **`fallback.on`** — the tenant's list of failure classes that are allowed to
  advance the chain; a class absent from it stops the request instead
- **Path A** — the customer-facing ingress, which is why an authentication failure
  there carries the error code `path_a_auth_required`

You reach for this instead of calling Anthropic or OpenAI directly when the call
must be governed: scanned for sensitive content before it leaves the cluster,
attributed to a tenant for spend, routed to whichever provider is cheapest or
healthiest right now, and able to survive one vendor failing without the caller
writing failover logic.

What you give up is direct vendor semantics — every request is parsed, scanned,
routed, and re-serialized. There are **no raw passthrough routes**. If you need
byte-identical vendor behaviour with no interposition, this is the wrong door.

## Mental model

```mermaid
flowchart TD
  sdk[Caller / stock SDK] --> mw[middleware.ParseHeaders<br/>TAS-Auth]
  mw --> surf{surface}
  surf -->|/v1/chat/completions| oai[handleChatCompletion]
  surf -->|/v1/messages| anth[handleMessages]
  surf -->|/v1/responses| resp[handleResponses]
  oai --> pipe[shared pipeline]
  anth --> pipe
  resp --> pipe
  pipe --> scan[scan + decideEnforcement]
  scan --> route[Route: select provider]
  route --> call[provider call]
  call --> cls{failure?}
  cls -->|classify twice| chain[walk fallback chain]
  chain --> call
  cls -->|success| render[render in caller's dialect]
```

Four abstractions carry the design.

**Surfaces** are wire dialects, not pipelines. `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses` translate at the boundary and converge on one
shared pipeline (`internal/server/server.go:833`, `:837`, `:846`). The surface you
called determines only how the request is parsed and how the response and errors
are rendered — **not** which vendor serves it. An Anthropic-dialect request can
be cost-routed to OpenAI and comes back shaped as Anthropic.

**Routing** selects a provider per request from the tenant's configuration.

**The fallback chain** advances through named tiers when an attempt fails in a
way another provider could survive. It is walked at the completion boundary, not
inside `Route()`, because only that layer knows whether an attempt succeeded.

**Enforcement** decides per finding what a scan result means — allow, redact, or
block (`internal/server/enforcement.go:63`).

What these abstractions do **not** own is as load-bearing as what they do. The
surfaces own no policy and no vendor choice; they translate and render. Routing
owns provider selection but not retry — it hands back a provider and never learns
whether the attempt worked, which is precisely why the chain lives elsewhere.
Enforcement owns the verdict but not the scan: findings arrive already produced,
and policy only decides what they mean for this tenant. Tenant identity comes
from outside the gateway entirely.

## How it works end to end

A request arrives at the customer ingress. `ParseHeaders`
(`internal/middleware/aiqg_headers.go:170`) lifts `TAS-Auth` and validates its
shape: it must carry the `tas_qg_live_` prefix
(`internal/middleware/aiqg_headers.go:141`), and a value that does not is
rejected as `ErrAuthMalformed` (`:148`) **before** any authentication lookup.
That ordering is why a malformed token and an unknown token produce different
status codes — see error semantics.

The surface handler parses the body in its own dialect. `handleChatCompletion`
(`internal/server/server.go:953`) reads OpenAI shape; `handleMessages`
(`internal/server/anthropic_messages.go:487`) reads Anthropic shape, including
top-level `system` and required `max_tokens`. Both produce the same internal
request, and both are wrapped by `wrapAIQG`, which is what attaches the
governance pipeline.

The prompt is scanned and `decideEnforcement` resolves findings against the
tenant's policy. In `observe` mode policy decides nothing and the pre-existing
controls keep running unchanged — it records what it *would* have done. Only in
enforcing mode can it block.

Routing then selects a provider and the call goes out. On failure the attempt is
classified **twice**, by two functions that deliberately disagree:
`ClassifyError` (`pkg/aiqg/breaker/breaker.go:382`) asks *should this count
against the provider?*, and `ClassifyFailure` (`:533`) asks *would a different
provider do better?* A 429 must not eject a healthy vendor, yet another vendor
has capacity. A context overflow is never the provider's fault, yet a
larger-window tier serves it unchanged. If the failure class is in the tenant's
`fallback.on` list, the chain advances and the loop repeats.

The response is rendered back in the dialect of the surface that was called.

## Getting started

Every completion endpoint requires a `TAS-Auth` token carrying the
`tas_qg_live_` prefix. Read endpoints do not — see the exposure note under limits.

`-k` below skips certificate verification: the ingress serves a certificate
from the internal `tas-ca-issuer`, which is not in a default trust store. Your
HTTP client will need the same accommodation, or the issuer's certificate.

```bash
curl -sS -k https://gateway.aiqg.tas.scharber.com/v1/models
{"object":"list","data":[{"id":"claude-haiku-4-5-20251001","object":"model","created":0,"owned_by":"anthropic"},{"id":"claude-opus-4-6","object":"model","created":0,"owned_by":"anthropic"}]}
```

With a token, the shortest real completion:

```bash
curl -sS -k https://gateway.aiqg.tas.scharber.com/v1/chat/completions \
  -H "TAS-Auth: $TAS_TOKEN" -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'
{"id":"chatcmpl-...","object":"chat.completion","model":"claude-sonnet-4-6","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}
```

Tokens live in `aether-secrets`, a separate repository in the TAS monorepo — not
in this repository and not in this document.

Expect your first failure to be an authentication one, and expect it to be
confusing: the gateway validates token *shape* before it authenticates, so a
token with the wrong prefix returns `400` rather than `401`. Confirm the prefix
before debugging anything else in your request.

**Pointing a stock SDK at it.** Put the gateway token in the SDK's `api_key`
slot and the vendor key in the encrypted vault; the gateway lifts the token from
`Authorization: Bearer` (OpenAI SDK) or `x-api-key` (Anthropic SDK), recognizing
it by prefix, then injects the stored vendor key upstream.

```python
OpenAI(api_key="tas_qg_live_…", base_url="https://gateway.aiqg.tas.scharber.com/v1")
Anthropic(api_key="tas_qg_live_…", base_url="https://gateway.aiqg.tas.scharber.com")
```

If you would rather hold the vendor key yourself, send it as `api_key` and pass
the gateway token in a `TAS-Auth` header instead. The `base_url` values differ by
vendor convention: the OpenAI SDK appends paths under `/v1`, the Anthropic SDK
supplies its own.

**Two things change that the SDK will not warn you about.** A malformed gateway
token returns `400`, so a stock SDK raises its bad-request exception rather than
its authentication exception — you will be looking at your request body while the
problem is your key. And both SDKs retry automatically by default, while nothing
here is idempotent: every retry is a second billed call. Set the SDK's max-retries
to 0 unless you have decided the duplicate spend is acceptable.

## API reference

`docs/openapi.yaml` in this repository is the reference source of truth; where
this document and the spec disagree, the spec wins and the disagreement is a bug
worth reporting. Registration lines are cited so you can jump to the handler.

**Completion surfaces** — all `POST`, all require `TAS-Auth`, none idempotent
(each call bills and emits telemetry):

| Endpoint | Dialect | Notes |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `"stream": true` for server-sent events |
| `/v1/completions` | OpenAI | Compatibility shim; expects `messages[]`, not the legacy `prompt` |
| `/v1/messages` | Anthropic | Top-level `system`, **`max_tokens` required**, content block arrays, native named-event streaming |
| `/v1/messages/count_tokens` | Anthropic | Returns `{"input_tokens": N}` from the vendor's own count endpoint; `max_tokens` not required |
| `/v1/embeddings` | OpenAI | Routes to the embeddings-capable provider; returns float vectors regardless of requested `encoding_format` |
| `/v1/responses` | OpenAI Responses | Input items or string translated to messages; `output[]` / `output_text` returned |

All six are registered together in `internal/server/server.go:833-846`, if you are
extending rather than calling.

**Read surfaces** — `GET`, no authentication enforced (verified live):

| Endpoint | Returns |
|---|---|
| `/v1/models` | Model catalogue. **SDK-aware**: Anthropic's `{data:[{type:"model"}]}` shape when the caller sends `anthropic-version`, otherwise OpenAI's `{object:"list"}` |
| `/v1/models/{model}` | Single model, same dialect switching |
| `/v1/providers` | `{"count":2,"providers":["openai","anthropic"]}` |
| `/v1/capabilities` | Per-provider capability matrix including `max_context_window` |
| `/v1/health`, `/health` | Provider health with per-provider `response_time_ms` |

The catalogue on 2026-08-24 was `claude-haiku-4-5-20251001`, `claude-opus-4-6`,
`claude-sonnet-4-6`, `gpt-3.5-turbo`, `gpt-4o`, `gpt-4o-mini`. Query the endpoint
rather than trusting that list — it is the authority, this document is not.

There are **no** `/v1/openai/*` or `/v1/anthropic/*` passthrough routes. Requests
are never reverse-proxied verbatim.

## Data model & contracts

Request and response shapes crossing this boundary are documented in
`aether-shared/data-models/tas-llm-router/` — `request-format.md`,
`response-format.md`, and `model-configurations.md`. Those are the source of
truth for field-level detail; restating them here would drift.

Two contracts worth stating because they are not in either schema:

**The surface determines the response dialect, not the provider.** A
`/v1/messages` call served by OpenAI still returns Anthropic-shaped content
blocks. Do not infer the serving vendor from the response envelope; read the
`model` field.

**Streaming preserves the caller's dialect.** OpenAI surfaces emit
`data:` / `[DONE]` server-sent events; `/v1/messages` emits Anthropic's named
events. A client written against one will not parse the other.

**Tenant scoping is implicit and carried by the token.** Nothing in the request
body identifies the tenant; it is resolved from `TAS-Auth`. A body field that
looks like it selects an account does not, and spend is attributed to whoever
owns the token. When copying a working request between environments, the token
is the part that changes meaning.

## Error semantics

Auth is resolved **before** body validation, so a bad token masks every body
error until it is fixed. All four rows below were verified with live probes on
2026-08-24.

| Condition | Status | Body | Retryable | Caller action |
|---|---|---|---|---|
| No `TAS-Auth` header | `401` | `{"error":{"code":"path_a_auth_required","message":"AIQG ingress requires a recognized TAS-Auth token","reason":"token_unknown"}}` | No | Supply a token |
| Token missing the `tas_qg_live_` prefix | **`400`** | `{"error":{"code":"aiqg_header_invalid","message":"aiqg: TAS-Auth token is malformed"}}` | No | Fix the token format — the shape check runs before the lookup (`aiqg_headers.go:141`) |
| Well-formed but unrecognized token | `401` | same as row 1, `"reason":"token_unknown"` | No | Use a provisioned token |
| Same, on `/v1/messages` | `401` | `{"type":"error","error":{"type":"authentication_error","message":"AIQG ingress requires a recognized TAS-Auth token"}}` | No | Note the **Anthropic-shaped envelope** — error rendering follows the surface |

The second row is the sharp edge: **a bad credential returns `400`, not `401`**,
because a malformed value never reaches authentication. An integrator debugging
a `400` will look at their request body and find nothing wrong with it.

Post-authentication statuses, read from source rather than observed (a valid
token was not available for probing):

| Status | Source | Meaning |
|---|---|---|
| `402` | `server.go:1358` | Payment or quota condition |
| `413` | `anthropic_messages.go:581` | Request entity too large |
| `429` | `anthropic_messages.go:583` | Rate limited — retryable with backoff |
| `503` | `server.go:1265`, `:2751` | `Routing failed: …` — every provider in the chain exhausted; retryable |
| `422` | `enforcement.go:132` | **Blocked by policy** — body `blocked by policy: <pattern names>`. Not retryable: the identical request will be blocked again |

The `422` is the one to handle deliberately, because it is the gateway doing its
job rather than failing. The message names the **patterns** that matched, never
the matched values — quoting the secret back would leak it to whoever triggered
the block (`enforcement.go:130`). A block only happens in enforcing mode; in
`observe` mode the same finding is recorded as what policy *would* have done and
the request proceeds, so the same input can return `200` or `422` depending on
tenant configuration you cannot see from the response.

> [!UNVERIFIED] The post-authentication rows above were not exercised live, so
> the exact body shape for each is unconfirmed. Treat the status codes as
> reliable and the envelopes as provisional until probed with a real token.

## Extension points

**Adding a provider** — implement the provider interface under
`internal/providers/`. Routing and the fallback chain operate on the interface,
so a new provider participates without touching the surfaces.

**Adding a wire surface** — translate at the boundary and converge on the shared
pipeline, as `anthropic_messages.go` and `responses_api.go` both do. Register in
the same block at `server.go:833`.

**Deliberately closed:** raw passthrough. There is no route that forwards a
request to a vendor unparsed, and adding one would bypass scanning, enforcement,
spend attribution, and routing at once. If you find yourself wanting it, the
requirement is usually "vendor feature X is unsupported" — extend the translation
layer instead.

Failure classification is also effectively closed to casual edits: `ClassifyError`
and `ClassifyFailure` live in the same file specifically so their disagreement
stays visible, and changing one without the other reintroduces the bug described
below.

## Design rationale

**Two classifiers rather than one.** The obvious design asks a single question —
"did this attempt fail?" — and a naive corollary, "4xx means do not try
elsewhere." Both classifiers exist because that rule is backwards for the two
most common cases. A `429` must not count against a vendor's health yet another
vendor has capacity; a context overflow is our request being too large for *this*
model, never the provider's fault, yet a larger-window tier serves it unchanged.
Merging the questions loses one answer or the other. They are colocated so the
distinction is visible at a glance rather than discovered later (PR #153).

**This is not hypothetical.** `ClassifyError` originally matched `429`, then
client-error patterns like `"400 "`, then defaulted to `ServerError`. A context
overflow phrased without a literal `400` fell through and counted against the
provider — meaning enough oversized prompts could eject a perfectly healthy
vendor, turning a config-shaped input into an outage. That is the exact failure
the two-axis classification exists to prevent (PR #158).

**Pre-flight context limits reroute rather than refuse.** A tidy local rejection
would make limits strictly worse than having none, since the vendor's own error
would at least have advanced the chain. Stated in that PR as: *detecting a
problem earlier must not mean recovering from it less* (PR #158).

**The chain walks at the completion boundary, not inside `Route()`.** `Route`
hands back a provider and never sees the result, so it cannot know whether an
attempt succeeded (PR #153).

**`observe` mode does not disable existing controls.** The rejected alternative
was to let policy supersede the pre-existing critical-block and redaction. That
would mean adopting the feature quietly *reduced* protection for every tenant
that had not yet configured enforcement — described in
`internal/server/enforcement.go:31` as a security feature whose rollout weakens
security, which is the wrong shape however clean the architecture.

**Enforcement decides per finding, not per request severity.** The operator's
rule is the authority: a `critical` finding for a pattern the bundle only logs
should not block merely because it is critical.

## Failure modes

Integration-time problems, in the order a new caller hits them.

**`400` with `aiqg: TAS-Auth token is malformed`** — the token lacks the
`tas_qg_live_` prefix. Reads like a body problem; is not.

**`401` while the body is also wrong** — auth precedes validation, so fixing the
token can immediately surface a second, different error. Do not assume a `401`
means the rest of the request is good.

**Parsing the wrong stream dialect** — a client written for OpenAI
`data:`/`[DONE]` events will not parse `/v1/messages` named events. Choose the
surface that matches your client, not the vendor you expect to serve it.

**Assuming the response envelope names the vendor** — it names the *surface*.
Read the `model` field to learn who served the request.

**`503 Routing failed`** — the chain was exhausted, or never engaged. The routing
decision records which, precisely so that a chain that did not engage is not
mistaken for one that is broken.

## Limits & trade-offs

**Read endpoints are unauthenticated.** `/v1/models`, `/v1/providers`, and
`/v1/capabilities` returned `200` to anonymous requests in live probes,
disclosing the model catalogue, the provider list, and the capability matrix
including context-window sizes. This is a deliberate convenience for SDK
discovery and also a public exposure — treat the catalogue as public information.

**Nothing is idempotent.** Every completion call bills and emits telemetry. There
is no request-id deduplication, so a client retry is a second charge.

**Interposition is unavoidable.** Every request is parsed and re-serialized. A
vendor feature that has no representation in the translation layer is unavailable
until the layer learns it, and there is no escape hatch.

**Version skew between deployments.** `llm-router` and `llm-router-aiqg` run
independent image tags. Behaviour verified against one is not guaranteed on the
other; see the operations document.

## Related

- OpenAPI specification: `docs/openapi.yaml` (served at `/docs`)
- Data models: `aether-shared/data-models/tas-llm-router/`
- Operations and on-call: `docs/ops/llm-router.md`
- Routing design: `aether-shared/data-models/aiqg/routing-decision.md`
