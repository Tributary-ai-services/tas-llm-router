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
  - "What does `/metrics` expose, and which series can I build a query on?"
  - "What are the compatibility guarantees — what may change without warning?"
  - "Where do I add behaviour, and what is deliberately closed?"
  - "Why is it designed this way rather than the obvious alternative?"
depth: deep
verified_against: "tas-llm-router@eee4b24, 2026-08-25"
---

# LLM Router — Developer Guide

> **Verified against `tas-llm-router@eee4b24` on 2026-08-25.** Wire behaviour and
> the model catalogue were captured from live probes against
> `gateway.aiqg.tas.scharber.com` on 2026-08-24 and re-probed on 2026-08-25;
> responses shown are real captures. Where a behaviour was read from source
> rather than observed, it says so.
>
> **The `/metrics` surface changed at `b6070a0` and the deployed image predates
> it.** `llm-router-aiqg` was running `aiqg-v5.86` on 2026-08-25 and still served
> the old hand-rolled exporter. The telemetry section below describes the
> committed behaviour and flags exactly which parts are not yet observable in the
> cluster.

## Why this exists

This gateway is the AI Quality Gateway (AIQG) ingress — the name appears
throughout the codebase (`wrapAIQG`, `aiqg_headers.go`), in error codes, and in
the deployment name `llm-router-aiqg`. AIQG is the governance layer; the router
is what it governs.

**Vocabulary used below**, defined here because none of it is guessable:

- **surface** — a wire dialect (`/v1/chat/completions`, `/v1/messages`), not a pipeline
- **finding** — one result from scanning a prompt: a matched pattern with a severity
- **bundle** — the set of patterns a tenant's policy enables, and what each should do
- **tier** — a named routing target in a fallback chain: a `(provider, model)`
  pair at an operator-assigned position. Tiers are walked in position order
  (`internal/routing/chain.go:77`–`89`), and "advancing a tier" means retrying
  against the next one — **with that tier's model substituted into your request**
  (`internal/server/fallback.go:154`), not with your original model carried
  forward. A tier naming a provider the tenant is denied is skipped rather than
  attempted (`internal/routing/chain.go:83`)
- **`fallback.on`** — the tenant's list of failure classes that are allowed to
  advance the chain; a class absent from it stops the request instead
  (`internal/routing/chain.go:57`–`62`, applied at
  `internal/server/fallback.go:123`). **A caller cannot read their own tenant's
  value from this gateway** — no endpoint exposes it. What you can read is the
  outcome after the fact: when a chain declines to advance, the reason is written
  onto the routing decision in plain language, such as `fallback not attempted:
  <class> is not in this rule's fallback.on`
  (`internal/server/fallback.go:124`–`125`)
- **`observe` / enforcing mode** — the per-tenant switch deciding whether a policy
  finding blocks. In `observe` the outcome is computed and recorded but discarded,
  so the request proceeds; only in enforcing mode does it become a `422`
  (`internal/server/enforcement.go:58`–`60`). **`observe` is the default**: when
  no bundle resolves for a request, `ResolvedEnforcement` returns
  `ModeObserve` with no rules (`internal/middleware/aiqg.go:1190`–`1200`). This
  is the single switch that decides whether identical input returns `200` or
  `422`, and you cannot see its value from a response
- **`clear.DollarCost`** — the gateway's pricing function, not a vendor invoice.
  It looks the `(vendor, model)` pair up in a static pricing table and computes
  `(prompt_tokens/1000 × input_rate) + (completion_tokens/1000 × output_rate)`,
  returning `ok=false` for a model it has no pricing for
  (`pkg/clear/cost.go:75`–`82`). It is the single cost figure in the service:
  both `llm_router_cost_total` and the spend record call it, which is why they
  cannot disagree — and why an unpriced model is absent from both rather than
  recorded as free
- **Path A** — the customer-facing ingress, which is why an authentication failure
  there carries the error code `path_a_auth_required`
- **registry** — a Prometheus `client_golang` collector set. This service runs two
  of them, deliberately kept separate: `internal/metrics.Registry` behind
  `/metrics` (`internal/metrics/metrics.go:48`) and `pkg/aiqg/metrics.Registry`
  behind `/aiqg/metrics`. Neither registers into the library's default registerer,
  so the surface each endpoint exposes is enumerable by reading one file
- **the AIQG event stream** — the per-request record this gateway emits, distinct
  from and far richer than `/metrics`. Each completed request produces a paired
  CloudEvent carrying tenant, token, model, token counts, cost, and routing
  decision. The gateway fans them to two sinks at once (`AIQG_EMITTER_TYPE:
  "both"`, `k8s/configmap.yaml:114`): to `logrus`, and therefore to Loki, which is
  the read path the AIQG dashboard backend uses; and to the Kafka topic
  `tas.aiqg.events.v1` (`k8s/configmap.yaml:116`), consumed by a Spark aggregator
  that materializes the `aiqg.event_metrics` hypertable in TimescaleDB
  (`k8s/configmap.yaml:108`–`112`). **It is not a surface an integrator calls.**
  There is no endpoint on this gateway that reads events back. If you need your
  own spend or attribution figures, the reachable surface is the AIQG dashboard
  backend at `aiqg-dashboard-be` (`k8s/configmap.yaml:106`), not this service —
  ask the AIQG owners for access rather than expecting a route here

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
  sdk[Caller / stock SDK] --> met[metrics.Middleware<br/>count + latency + in-flight]
  met --> mw[middleware.ParseHeaders<br/>TAS-Auth]
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

Five abstractions carry the design.

**Surfaces** are wire dialects, not pipelines. `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses` translate at the boundary and converge on one
shared pipeline (`internal/server/server.go:857`, `internal/server/server.go:861`,
`internal/server/server.go:870`). The surface you
called determines only how the request is parsed and how the response and errors
are rendered — **not** which vendor serves it. An Anthropic-dialect request can
be cost-routed to OpenAI and comes back shaped as Anthropic.

**Routing** selects a provider per request from the tenant's configuration.

**The fallback chain** advances through named tiers when an attempt fails in a
way another provider could survive. It is walked at the completion boundary, not
inside `Route()`, because only that layer knows whether an attempt succeeded.

**Enforcement** decides per finding what a scan result means — allow, redact, or
block (`internal/server/enforcement.go:64`).

**Telemetry** is an HTTP middleware plus a registry. `metrics.Middleware`
(`internal/metrics/middleware.go:59`) wraps every completion route and records
count, latency, and in-flight depth; the completion handlers themselves add token
and cost samples. The registry it writes into is served verbatim at `/metrics`
(`internal/server/server.go:898`). Telemetry is an abstraction here and not a
detail because its ordering relative to the other four is a contract, not an
implementation choice — see the middleware ordering paragraph below.

What these abstractions do **not** own is as load-bearing as what they do. The
surfaces own no policy and no vendor choice; they translate and render. Routing
owns provider selection but not retry — it hands back a provider and never learns
whether the attempt worked, which is precisely why the chain lives elsewhere.
Enforcement owns the verdict but not the scan: findings arrive already produced,
and policy only decides what they mean for this tenant. Telemetry owns no
decision at all: nothing in the request path reads a metric, so a registry that
is wrong misleads a human and changes no behaviour — which is exactly why a wrong
one survived in production for months. Tenant identity comes from outside the
gateway entirely.

## How it works end to end

A request arrives at the customer ingress and is counted before it is
authenticated. `wrapAIQG` (`internal/server/server.go:845`) composes each
completion route as `metrics.Middleware(aiqgMiddleware(handler))`, so the
metrics wrapper is the outermost layer and sees every request the gateway later
refuses. That ordering is deliberate and stated as such at
`internal/server/server.go:850`: an authentication failure is traffic, and an
exporter blind to it cannot show an auth outage. The practical consequence for
you is that a `401` you caused appears in `llm_router_requests_total` with
`provider="none"`, not as a gap.

Then `ParseHeaders`
(`internal/middleware/aiqg_headers.go:170`) lifts `TAS-Auth` and validates its
shape: it must carry the `tas_qg_live_` prefix
(`internal/middleware/aiqg_headers.go:141`), and a value that does not is
rejected as `ErrAuthMalformed` (`internal/middleware/aiqg_headers.go:148`)
**before** any authentication lookup.
That ordering is why a malformed token and an unknown token produce different
status codes — see error semantics.

The surface handler parses the body in its own dialect. `handleChatCompletion`
(`internal/server/server.go:979`) reads OpenAI shape; `handleMessages`
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
against the provider?*, and `ClassifyFailure`
(`pkg/aiqg/breaker/breaker.go:533`) asks *would a different
provider do better?* A 429 must not eject a healthy vendor, yet another vendor
has capacity. A context overflow is never the provider's fault, yet a
larger-window tier serves it unchanged. If the failure class is in the tenant's
`fallback.on` list, the chain advances and the loop repeats.

The response is rendered back in the dialect of the surface that was called.

On the way out, two things are recorded that you can later query. The completion
handler stamps the routing decision into `X-TAS-Router-*` response headers
(`internal/server/server.go:1715`) and, when the vendor reported usage, feeds the
same token counts and the same `clear.DollarCost` result into the metrics
registry that the spend record uses (`internal/server/server.go:1681` and
`internal/server/server.go:1683`; the fallback-walking variant repeats it at
`internal/server/server.go:1855` and `internal/server/server.go:1857`). Sharing
one cost call is the point: `llm_router_cost_total` and the billing record are
computed from the same numbers, so they cannot drift apart. Only then does the
metrics middleware, unwinding outermost, read the provider back off the response
header to label the request counter (`internal/metrics/middleware.go:69`) — which
is why the provider label is resolved after the handler returns rather than up
front, and why it reads `none` whenever routing never chose one.

## Getting started

Every completion endpoint requires a gateway token carrying the `tas_qg_live_`
prefix (`internal/middleware/aiqg_headers.go:141`). Read endpoints do not — see
the exposure note under limits.

**Three headers carry that same token**, which is what lets a stock SDK work
without custom headers: `TAS-Auth`, `Authorization: Bearer` (with or without the
scheme), and `x-api-key`. When `TAS-Auth` is absent, `recoverGatewayToken` checks
the other two and lifts the value into `TAS-Auth` if it matches the prefix
(`internal/middleware/aiqg.go:400`–`418`, called at
`internal/middleware/aiqg.go:154`), after which the rest of the chain resolves it
identically. The prefix is what keeps this unambiguous: a real vendor key
(`sk-…`, `sk-ant-…`) never matches and falls through untouched
(`internal/middleware/aiqg.go:146`–`148`). Whichever carrier you use, it is one
token and one auth path — the sections below say `TAS-Auth` for brevity.

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

**Which deployment you are talking to.** `gateway.aiqg.tas.scharber.com` is the
Ingress for the `llm-router-aiqg` Service on port 8086
(`k8s/ingress-aiqg-strict.yaml:41`, `:48`, `:50`), which fronts the
**strict-mode** Deployment — `AIQG_STRICT` is set on it
(`k8s/deployment-aiqg-strict.yaml:82`) and strict mode is what makes a missing
`TAS-Auth` a `401` (`internal/middleware/aiqg.go:174`). The other Deployment,
`llm-router`, sits behind `llm-router.tas.scharber.com`
(`k8s/ingress.yaml:32`) and runs permissive: the AIQG middleware activates only
when a request actually carries `TAS-Auth`, and internal callers without it pass
through untouched (`internal/server/server.go:188`–`191`). Both live in namespace
`tas-llm-router`. Everything in this document describes the strict host unless it
says otherwise; the same binary serves both, so the wire contract is identical
and only the auth posture differs.

**Getting a token.** You do not self-serve. Tokens are provisioned by ops into
the Kubernetes Secret `llm-router-aiqg-tokens` in namespace `tas-llm-router`,
which the binary reads through the `AIQG_TOKENS_FILE` environment variable
(`k8s/deployment-aiqg-strict.yaml:85`, loaded at
`internal/config/config.go:840`). The Secret in this repository ships empty on
purpose and carries the provisioning note: ops applies the populated version
out-of-band from a vault, or via one of the secret-encryption tools the note
names — sealed-secrets, external-secrets, or `sops`
(`k8s/secret-aiqg-tokens.yaml:1`–`6`). Each entry binds a token to a
`tenant_id`, an `aiqg_account_id`, a `source_app` string, and a `suspended` flag
(`k8s/secret-aiqg-tokens.yaml:10`–`16`) — which is the mechanism behind "tenant
scoping is implicit and carried by the token" below. To request one, ask the AIQG
owners for an entry naming your `source_app`; the values themselves live only in
`aether-secrets` and in the cluster Secret, never in this repository and never in
this document.

> [!UNVERIFIED] No self-service provisioning API, request form, or named owner
> for token issuance appears anywhere in this repository — only the Secret and
> the note that ops applies it out-of-band. The escalation path is therefore
> stated as "ask the AIQG owners" rather than as a procedure. Confirm the current
> route with the service owner.

**A gotcha that will mislead you in a non-production cluster.** When the token
list is empty, the AIQG middleware runs a permissive no-resolver path: any
`tas_qg_live_`-shaped bearer is accepted and events are emitted with empty tenant
fields (`k8s/secret-aiqg-tokens.yaml:18`–`21`). So a made-up token can appear to
work, right up until the list is populated and the same token starts returning
`401`. If your token was never issued to you by a person, assume it is not real.

Expect your first failure to be an authentication one, and expect it to be
confusing: the gateway validates token *shape* before it authenticates, so a
token with the wrong prefix returns `400` rather than `401`. Confirm the prefix
before debugging anything else in your request.

**Pointing a stock SDK at it.** A stock SDK can populate only its own vendor's
credential slot and has no way to set a custom header, which is exactly the
problem the three-carrier rule above solves. Put the gateway token in the SDK's
`api_key` slot; the gateway lifts it out of `Authorization: Bearer` (OpenAI SDK)
or `x-api-key` (Anthropic SDK) by prefix, **deletes that header so the
`tas_qg_live_` secret is never forwarded to the vendor**
(`internal/middleware/aiqg.go:158`–`167`), and then selects the effective
upstream key itself — your stored credential if you have one, otherwise the TAS
shared key (`internal/server/server.go:1341`–`1384`). Both constructor forms are
named as the worked examples in the source comment that describes this path
(`internal/middleware/aiqg.go:148`–`150`):

```python
OpenAI(api_key="tas_qg_live_…", base_url="https://gateway.aiqg.tas.scharber.com/v1")
Anthropic(api_key="tas_qg_live_…", base_url="https://gateway.aiqg.tas.scharber.com")
```

If you would rather hold the vendor key yourself, send the real vendor key in the
`TAS-Upstream-Authorization` header and the gateway token in `TAS-Auth`. That
header is parsed but never logged (`internal/middleware/aiqg_headers.go:49`,
lifted at `:173`), is stripped from the request before it reaches the vendor
along with every other `TAS-*` header
(`internal/middleware/aiqg_headers.go:309`), and takes precedence over any
stored credential (`internal/server/server.go:1352`–`1361`). The raw
`Authorization` header is deliberately **not** used for this: clients
historically sent placeholders there, so injecting it would break them
(`internal/server/server.go:1353`–`1357`). The `base_url` values differ by vendor
convention: the OpenAI SDK appends paths under `/v1`, the Anthropic SDK supplies
its own.

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
| `/v1/completions` | OpenAI | Compatibility shim — the handler delegates straight to `handleChatCompletion` (`internal/server/server.go:1654`–`1658`), so it decodes the **chat** body: `messages[]`, and a legacy `prompt` field is ignored |
| `/v1/messages` | Anthropic | Top-level `system`, **`max_tokens` required**, content block arrays, native named-event streaming |
| `/v1/messages/count_tokens` | Anthropic | Returns `{"input_tokens": N}` from the vendor's own count endpoint (`internal/server/count_tokens.go:66`); `max_tokens` not required |
| `/v1/embeddings` | OpenAI | Routes to the embeddings-capable provider. `encoding_format` is forwarded upstream (`internal/providers/openai/embeddings.go:27`), but the client library transparently decodes a base64 vendor response, so you receive float vectors either way (`internal/providers/openai/embeddings.go:15`–`17`) |
| `/v1/responses` | OpenAI Responses | Input items or string translated to messages; `output[]` / `output_text` returned |

All six are registered together in `internal/server/server.go:857`–`870`, if you
are extending rather than calling. Each is wrapped by `wrapAIQG`
(`internal/server/server.go:845`), which is also what puts the metrics middleware
around them.

**Read surfaces** — `GET`, no authentication enforced. Every row below was
re-probed anonymously against the live gateway on 2026-08-25:

| Endpoint | Returns |
|---|---|
| `/v1/models` | Model catalogue. **SDK-aware**: Anthropic's `{data:[{type:"model"}]}` shape when the caller sends `anthropic-version`, otherwise OpenAI's `{object:"list"}` |
| `/v1/models/{model}` | Single model, same dialect switching |
| — | Note: the Anthropic-shaped listing returns **only Anthropic models** (3 of the 6 in the catalogue, observed 2026-08-25), while the OpenAI-shaped listing returns all six |
| `/v1/providers` | `{"count":2,"providers":["openai","anthropic"]}` |
| `/v1/capabilities` | Per-provider capability matrix including `max_context_window` |
| `/v1/health`, `/health` | Provider health with per-provider `response_time_ms` |

The catalogue on 2026-08-24 was `claude-haiku-4-5-20251001`, `claude-opus-4-6`,
`claude-sonnet-4-6`, `gpt-3.5-turbo`, `gpt-4o`, `gpt-4o-mini`. Query the endpoint
rather than trusting that list — it is the authority, this document is not.

There are **no** `/v1/openai/*` or `/v1/anthropic/*` passthrough routes. Requests
are never reverse-proxied verbatim.

### Feature support, and what the translation layer drops

"Every request is parsed and re-serialized" has a consequence worth stating
concretely, because it decides whether you can ship: a vendor feature survives
only if the internal `ChatRequest` has a field for it
(`internal/types/requests.go:8`–`39`) **and** the provider you are routed to
forwards that field. Both halves are needed, and the second half is where the
surprises are.

**The hazard is routing asymmetry.** The surface you call does not choose the
vendor, so a request that works today because it was served by OpenAI can lose a
feature tomorrow when cost routing sends the identical body to Anthropic. Nothing
warns you: unsupported fields are dropped silently, not rejected.

| Feature | Served by OpenAI | Served by Anthropic |
|---|---|---|
| `tools` (request) | Forwarded (`internal/providers/openai/provider.go:549`) | Forwarded (`internal/providers/anthropic/provider.go:511`) |
| `tool_calls` (non-streaming response) | Forwarded (`internal/providers/openai/provider.go:601`) | Forwarded (`internal/providers/anthropic/provider.go:686`) |
| `tool_calls` (streaming response) | Forwarded (`internal/providers/openai/provider.go:658`) | **Lost** — the stream converter handles text deltas only (`internal/providers/anthropic/provider.go:253`) |
| `tool_choice` | Forwarded (`internal/providers/openai/provider.go:564`) | **Dropped** — no reference in the provider |
| legacy `functions` / `function_call` | Forwarded (`internal/providers/openai/provider.go:535`) | **Dropped** |
| `response_format: json_object` | Forwarded (`internal/providers/openai/provider.go:568`) | **Dropped** — the provider never reads `req.ResponseFormat`; it also declares `SupportsStructuredOutput()` false (`internal/providers/anthropic/provider.go:398`) |
| `response_format: json_schema` | **Type sent, schema not** (`internal/providers/openai/provider.go:574`) | **Dropped** |
| `temperature`, `top_p`, `stop` | Forwarded (`internal/providers/openai/provider.go:510`, `:521`) | Forwarded (`internal/providers/anthropic/provider.go:492`, `:496`, `:500`) |
| `seed`, `presence_penalty`, `frequency_penalty` | Forwarded (`internal/providers/openai/provider.go:524`–`532`) | **Dropped** (no vendor equivalent) |
| `max_tokens` | Forwarded (`internal/providers/openai/provider.go:518`) | Forwarded; **defaults to 1024 when unset** (`internal/providers/anthropic/provider.go:489`) |
| `top_k` | Not representable | Not representable — absent from `ChatRequest` entirely |
| `n`, `logprobs`, `logit_bias`, `user`, `stream_options` | Not representable — absent from `ChatRequest` | Not representable |
| Image / vision content | See below | See below |

Three of those need more than a table cell.

**`json_schema` is the sharpest edge**, because it fails rather than degrades. The
OpenAI provider copies the `type` string but never assigns the schema itself —
the branch that would do it is a debug log
(`internal/providers/openai/provider.go:574`–`578`). A request asking for
`{"type":"json_schema"}` therefore reaches OpenAI declaring a schema-constrained
response and supplying no schema. Use `json_object` and validate the result
yourself, or constrain the output with tools instead.

**Vision does not work on any surface at present.** The two providers convert
message content with a Go type switch over `msg.Content`. The OpenAI provider's
switch has arms for `string` and `[]types.ContentPart` and no default
(`internal/providers/openai/provider.go:461`–`486`); the Anthropic provider's
default arm stringifies with `fmt.Sprintf("%v", content)`
(`internal/providers/anthropic/provider.go:621`–`628`), and its
`[]types.ContentPart` arm skips image parts with the comment "Skip image parts
for now" (`internal/providers/anthropic/provider.go:612`). The problem is that
`[]types.ContentPart` is not the type that arrives: `Content` is declared
`interface{}` (`internal/types/requests.go:43`), and content that has been
through JSON decoding arrives as `[]interface{}` of maps — which this repository
states in its own comment at `internal/server/server.go:2221`. `/v1/messages`
reaches the same decoder, because `handleMessages` re-marshals the translated
request and hands it to the shared OpenAI handler
(`internal/server/anthropic_messages.go:498`–`509`, decoded at
`internal/server/server.go:981`). Send text-only requests until this is fixed.

**Do not take `/v1/capabilities` as the authority on vision.** Both providers
advertise `SupportsVision: true` in their capability matrix
(`internal/providers/anthropic/provider.go:83`,
`internal/providers/openai/provider.go:83`), and the Anthropic entry even lists
`SupportedImageFormats` (`internal/providers/anthropic/provider.go:89`). A live
probe on 2026-08-25 returned `"supports_vision":true` for every model. That flag
describes the **vendor's** capability, not this gateway's translation layer,
which drops image parts on both paths as described above. The capability matrix
and the code disagree, and the code is what runs.

> [!UNVERIFIED] The vision finding is a code-path reading, not an executed
> request — no valid token was available to send a multimodal body through. The
> types and the missing default arm are verified at `eee4b24`; the end-to-end
> consequence (an image request losing its text as well, on the OpenAI path) is
> inferred from them. Confirm with a real request before filing or relying on it.

> [!UNVERIFIED] Streaming responses served by OpenAI appear never to carry token
> usage: the provider builds the upstream request without `StreamOptions`
> (`internal/providers/openai/provider.go:507`–`512`) and a search of the
> repository at `eee4b24` finds no `StreamOptions` or `include_usage` anywhere,
> which is what OpenAI requires before it emits usage on a stream. This is
> consistent with `tokens_total` and `cost_total` being fed only from the
> non-streaming paths, but it was not confirmed against a live stream.

### Telemetry surfaces

Two scrape endpoints, both `GET`, both unauthenticated, both served straight from
a registry through `promhttp` with no handwritten formatting in between:

| Endpoint | Registry | Registered at |
|---|---|---|
| `/metrics` | `internal/metrics.Registry` — router telemetry | `internal/server/server.go:898` |
| `/aiqg/metrics` | `pkg/aiqg/metrics.Registry` — AIQG event and token counters | `internal/server/server.go:902` |

Treat `/metrics` as an interface, not an implementation detail: dashboards and
alerts are callers of it, and the series below are what they may depend on. Every
name is prefixed `llm_router_`.

| Series | Type | Labels | Fed by |
|---|---|---|---|
| `requests_total` | counter | `provider`, `method`, `status_code` | `internal/metrics/middleware.go:74` |
| `request_duration_seconds` | histogram | `provider`, `method` | `internal/metrics/middleware.go:75` |
| `active_connections` | gauge | none | `internal/metrics/middleware.go:61` |
| `tokens_total` | counter | `provider`, `type` (`input`/`output`) | `internal/server/server.go:1681`, `internal/server/server.go:1855` |
| `cost_total` | counter | `provider`, `model` | `internal/server/server.go:1683`, `internal/server/server.go:1857` |
| `blocked_requests_total` | counter | `direction` (`inbound`/`outbound`) | `internal/server/enforcement.go:127` |
| `provider_health` | gauge | `provider` | `internal/server/server.go:338`, collected at scrape time |
| `errors_total` | counter | `provider`, `error_type` | declared `internal/metrics/metrics.go:111`; **no call site** |
| `auth_attempts_total` | counter | `result` | declared `internal/metrics/metrics.go:122`; **no call site** |
| `rate_limit_hits_total` | counter | `tier` | declared `internal/metrics/metrics.go:142`; **no call site** |

Label value sets, so a query can be written without guessing. `provider` is a
provider name (`openai`, `anthropic` on the current fleet) or the literal `none`;
`method` is the HTTP method, in practice always `POST` because only the
completion routes are counted; `status_code` is the decimal status as a string;
`type` is `input` or `output` (`internal/metrics/metrics.go:210`, `:213`);
`direction` is `inbound` or `outbound` (`internal/metrics/metrics.go:131`);
`tier` has no producer, so it has no observed values. `provider_health` is `1`
for healthy and `0` for anything else, with "healthy" meaning the router's own
health record reads exactly `healthy` (`internal/server/server.go:341`).
`request_duration_seconds` uses explicit buckets, not the library defaults —
0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120 seconds
(`internal/metrics/metrics.go:75`), stretched past a normal web service level
objective (SLO) because a
model call routinely takes seconds and a chain that walks two providers takes
longer.

### Scoping a query

Prometheus discovers these pods by annotation — `prometheus.io/scrape`,
`prometheus.io/port: "8086"`, `prometheus.io/path: "/metrics"`
(`k8s/deployment.yaml:18`–`20` and `k8s/deployment-aiqg-strict.yaml:40`–`42`) —
and attaches the target labels that scope every query. Observed on the live
Prometheus on 2026-08-25: `job="llm-router"`, `namespace="tas-llm-router"`,
`instance="<pod-ip>:8086"`, and `service` carrying either `llm-router` or
`llm-router-aiqg`. **`service` is the label that separates the two
deployments**, and it is the one to scope on:

```promql
sum by (provider) (
  rate(llm_router_requests_total{job="llm-router", service="llm-router-aiqg"}[5m])
)
```

A latency quantile needs the histogram's bucket dimension and the same scoping:

```promql
histogram_quantile(0.95,
  sum by (le, provider) (
    rate(llm_router_request_duration_seconds_bucket{service="llm-router-aiqg"}[5m])
  )
)
```

Four contracts a query author needs and cannot read off the tables above.

**`service` is Prometheus's label, not the exporter's — and the exporter's own
copy is about to disappear.** The old handler stamped a constant
`service="llm-router"` onto every sample it produced. Because Prometheus already
attaches a `service` target label, that constant collides and is renamed on
ingest, which is why the live series currently carry **both**
`service="llm-router-aiqg"` (the target label, correct) and
`exported_service="llm-router"` (the exporter's constant, useless — it reads
`llm-router` on both deployments). The new registry stamps no constant labels at
all, so `exported_service` vanishes after the deploy while `service` keeps
working. A query selecting on `service` is safe; one selecting on
`exported_service` breaks.

**Only the completion routes are counted.** `metrics.Middleware` is applied
through `wrapAIQG`, which wraps only the six `POST` completion surfaces
(`internal/server/server.go:857`–`870`). Every `GET` — `/v1/models`,
`/v1/models/{model}`, `/v1/providers`, `/v1/providers/{name}`, `/v1/capabilities`,
`/v1/health`, `/v1/health/{name}`, `/v1/breaker`, `/health`, and `/metrics`
itself — is registered without it (`internal/server/server.go:872`–`898`) and
contributes nothing. `POST /v1/routing/decision` is unwrapped too. A panel titled
"total gateway traffic" built on `requests_total` therefore measures completions
only, and silently excludes all read traffic. Excluding `/metrics` is deliberate:
a fifteen-second scrape would otherwise dominate the counter
(`internal/metrics/middleware.go:56`–`58`). Excluding the rest is a consequence
of that same decision, not a separate one.

**An unobserved counter emits no line at all.** `client_golang` writes a
`CounterVec` child only once something increments it, so a freshly restarted pod
serves a scrape with no `requests_total` in it — not a zero. Alerts must handle
the absent case (`absent()` or `or vector(0)`) rather than assuming a zero
baseline.

**The bottom three rows are registered but never incremented.** `errors_total`,
`auth_attempts_total`, and `rate_limit_hits_total` are declared and registered
(`internal/metrics/metrics.go:158`–`160`) and no code in the repository calls
them; a repository-wide search at `eee4b24` finds only the declarations, the
registration, and the tests. Because of the rule above they are therefore absent
from every scrape, which is the honest outcome — but do not read their absence as
"no errors" or "no rate limiting". Wiring them is open work, not a guarantee.

Those combine into four distinct ways a query returns no rows, worth
distinguishing before you conclude the gateway is idle: the series is one of the
three with no producer; the series has a producer but nothing has incremented it
on this pod yet; you are querying `GET` traffic that is never counted; or —
the fourth, and the least visible — `provider_health` failed to register.
`RegisterProviderHealth` is called once per `NewServer` and a duplicate
registration is swallowed as `prometheus.AlreadyRegisteredError`
(`internal/server/server.go:345`). That tolerance is what lets a second
`NewServer` in one process work, but it also means any registration failure of
that class is discarded without a log line, and the gauge is then absent
from the registry with nothing anywhere reporting why. Absent `provider_health`
means "not registered", never "unhealthy" — unhealthy is the value `0`.

Scraping `/metrics` looks like this. The capture below is from the registry at
`eee4b24` after one served request, one rejected request, one policy block, and
one priced completion; buckets are elided:

```text
# HELP llm_router_requests_total Completion requests by serving provider, method, and response status.
# TYPE llm_router_requests_total counter
llm_router_requests_total{method="POST",provider="anthropic",status_code="200"} 1
llm_router_requests_total{method="POST",provider="none",status_code="401"} 1
# HELP llm_router_cost_total Cumulative cost in USD by provider and model.
# TYPE llm_router_cost_total counter
llm_router_cost_total{model="claude-sonnet-4-6",provider="anthropic"} 0.0123
# HELP llm_router_blocked_requests_total Requests blocked by policy enforcement, by direction.
# TYPE llm_router_blocked_requests_total counter
llm_router_blocked_requests_total{direction="inbound"} 1
# HELP llm_router_request_duration_seconds End-to-end request latency including scanning, routing, and fallback.
# TYPE llm_router_request_duration_seconds histogram
llm_router_request_duration_seconds_sum{method="POST",provider="anthropic"} 1.4
llm_router_request_duration_seconds_count{method="POST",provider="anthropic"} 1
```

Eight series that the old exporter published no longer exist, and a test fails if
one is reintroduced without instrumentation behind it
(`internal/metrics/metrics_test.go:48`): `llm_router_security_score`,
`llm_router_threat_level`, `llm_router_active_api_keys`,
`llm_router_input_sanitized_total`, `llm_router_validation_failures_total`,
`llm_router_security_events_total`, `llm_router_audit_events_total`, and
`llm_router_rate_limit_usage`. A dashboard panel bound to any of them renders
empty after the deploy that carries `b6070a0`. The `client_ip` label on
`requests_total` is gone for the same kind of reason, stated under design
rationale.

> [!UNVERIFIED] The exposition capture above was produced by exercising the
> `eee4b24` registry directly, not by scraping a deployed pod. `llm-router-aiqg`
> was still running image `aiqg-v5.86` on 2026-08-25 and served the old
> hand-rolled output — including an `llm_router_cost_total` reading over $8.9M
> that no request produced. Confirm the shape against a pod built from `b6070a0`
> or later before treating the labels as deployed.

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

**`model` pins the vendor when it is unambiguous — which in practice means it
pins.** This is the contract that makes the feature-support table actionable, so
it is worth stating exactly. `determineStrategy` checks the requested model
first: if **exactly one** registered provider advertises it,
`isSpecificProviderRequested` returns true and the strategy becomes `specific`
(`internal/routing/router.go:435`–`439`, `:454`–`465`). That path resolves the
model to its provider and routes there, and if that provider is unhealthy it
**errors rather than substituting another vendor**
(`internal/routing/router.go:503`–`514`) — you get `503 Routing failed`, never a
silent cross-vendor swap. Concrete catalogue names behave this way:
`claude-sonnet-4-6` is advertised only by Anthropic, `gpt-4o` only by OpenAI.

The hint case is when the name is *not* unambiguous — advertised by no registered
provider, or by more than one. `isSpecificProviderRequested` counts matches and
requires exactly one (`internal/routing/router.go:464`), so zero matches falls
through to cost-optimized routing, which selects the cheapest healthy provider
and **ignores `req.Model` entirely** for that choice
(`internal/routing/router.go:537`–`548`). A misspelled or unknown model name does
not error — it silently becomes "whatever is cheapest".

Two things can still move a pinned request after selection. A resolved route rule
may pin a provider itself, and that pin is preferred over the strategy though it
is not absolute — an unusable pin falls through to normal selection and is
recorded as not honoured (`internal/routing/router.go:161`–`167`). And the
fallback chain, if the tenant has one configured, replaces both provider **and**
model with the tier's own (`internal/server/fallback.go:154`). Absent a
configured chain there is no fallback at all: `completeWithFallback` returns the
error immediately when `chain.Configured()` is false
(`internal/server/fallback.go:81`–`83`).

**No header pins a vendor per request.** The `TAS-*` header set is parsed
exhaustively at `internal/middleware/aiqg_headers.go:96`–`103` and contains no
provider or vendor selector. Routing hints travel in the body instead —
`optimize_for`, `required_features`, and `max_cost`
(`internal/types/requests.go:26`–`29`) — and the Anthropic surface accepts the
same fields through the SDK's `extra_body`
(`internal/server/anthropic_messages.go:64`–`66`). The practical rule: **name a
concrete model from `/v1/models` and you control the vendor; leave it vague and
the router chooses.**

**Streaming preserves the caller's dialect.** OpenAI surfaces emit
`data:` / `[DONE]` server-sent events; `/v1/messages` emits Anthropic's named
events. A client written against one will not parse the other.

**Tenant scoping is implicit and carried by the token.** Nothing in the request
body identifies the tenant; it is resolved from `TAS-Auth`. A body field that
looks like it selects an account does not, and spend is attributed to whoever
owns the token. When copying a working request between environments, the token
is the part that changes meaning.

### Compatibility guarantees: there are none stated

**No stability policy exists in this repository.** There is no CHANGELOG, no
VERSION file, no deprecation window, no stability tiers, and no support policy.
`docs/openapi.yaml` declares `version: 1.0.0` (`docs/openapi.yaml:36`) and that
number has no documented meaning — it is not tied to a release process, and the
deployed images are tagged on a separate `aiqg-v<major>.<minor>` sequence with no published
mapping back to commits. The `/metrics` change documented here is itself the
worked example: eight series were removed in a single commit with no deprecation
period, which is a reasonable decision on the merits and also demonstrates the
absence of a policy that would have required one.

This is a real answer, not a missing section: **treat every part of this surface
as unversioned and subject to change without notice**, and pin accordingly.

What is worth relying on, in descending order of safety:

- **The vendor dialects.** `/v1/chat/completions` and `/v1/messages` are
  constrained by the OpenAI and Anthropic wire formats, which this service must
  keep matching for stock SDKs to work at all. That external pressure is a
  stronger guarantee than anything stated here.
- **What the tests assert.** A behaviour with a named test is one someone
  intended to keep. `internal/metrics/metrics_test.go` is the model: the removed
  series, the absent `client_ip` label, `http.Flusher` passthrough, and
  implicit-200 recording are each pinned by a test, so they will not change
  silently.
- **`docs/openapi.yaml`**, which the API reference section already names as the
  source of truth over this document.

What to treat as unstable and not branch on: the `X-TAS-Router-*` response
headers, which are a TAS convenience and not part of either vendor dialect; the
`code` and `reason` strings inside error bodies; and the text of the
`422` body, which is assembled by string concatenation
(`internal/server/enforcement.go:134`–`135`) and has no schema. If you must react
to a policy block, branch on the `422` status, not on its message.

> [!UNVERIFIED] No architecture decision record (ADR), pull-request discussion,
> or design document in this repository
> states a compatibility or deprecation policy. The absence is reported as
> observed, not as a decision anyone recorded. If a policy exists elsewhere in
> TAS, it is not linked from here — confirm with the service owner before
> treating any of the above as guaranteed.

## Error semantics

Auth is resolved **before** body validation, so a bad token masks every body
error until it is fixed. All four rows below were verified with live probes on
2026-08-24.

| Condition | Status | Body | Retryable | Caller action |
|---|---|---|---|---|
| No `TAS-Auth` header | `401` | `{"error":{"code":"path_a_auth_required","message":"AIQG ingress requires a recognized TAS-Auth token","reason":"token_unknown"}}` | No | Supply a token |
| Token missing the `tas_qg_live_` prefix | **`400`** | `{"error":{"code":"aiqg_header_invalid","message":"aiqg: TAS-Auth token is malformed"}}` | No | Fix the token format — the shape check runs before the lookup (`internal/middleware/aiqg_headers.go:141`) |
| Well-formed but unrecognized token | `401` | same as row 1, `"reason":"token_unknown"` | No | Use a provisioned token |
| Token resolves to a **suspended** account | `403` | `{"error":{"code":"account_suspended","message":"AIQG account is currently suspended; contact support"}}` — Anthropic-shaped `permission_error` on `/v1/messages` (`internal/middleware/aiqg.go:1033`–`1039`) | No | Contact support. The token is genuine, so this is not a credential problem; retrying or reissuing will not clear it (`internal/middleware/aiqg.go:220`–`221`) |
| Same, on `/v1/messages` | `401` | `{"type":"error","error":{"type":"authentication_error","message":"AIQG ingress requires a recognized TAS-Auth token"}}` | No | Note the **Anthropic-shaped envelope** — error rendering follows the surface |

The second row is the sharp edge: **a bad credential returns `400`, not `401`**,
because a malformed value never reaches authentication. An integrator debugging
a `400` will look at their request body and find nothing wrong with it.

Post-authentication statuses, read from source rather than observed (a valid
token was not available for probing). **The "Billed" column is the one that
decides your retry policy**, because nothing here is idempotent and there is no
request-id deduplication: a retry of an already-billed failure is a second
charge, not a free correction.

| Status | Source | Meaning | Billed | Retry |
|---|---|---|---|---|
| `402` | `internal/server/server.go:1384` | `provider_key_required: no stored <vendor> credential for this account and shared-key fallback is disabled` | No — refused before any vendor call | Never. Store a credential or enable shared fallback; the identical request fails identically |
| `422` | `internal/server/enforcement.go:134` | **Blocked by policy** — body `blocked by policy: <pattern names>` | No — refused before the vendor call | Never. The identical request is blocked again |
| `503` | `internal/server/server.go:1291` | `Routing failed: …` — the router could not **select** a provider | No — this is selection failing, before any attempt | Yes, with backoff. Nothing was spent |
| `500` | `internal/server/server.go:1669`, `internal/server/server.go:1796` | `Completion failed: …` — the attempt, **including every fallback hop**, failed | **Yes, potentially several times** | Only deliberately. Each prior attempt that reached a vendor may already be billed |
| `413` | `internal/server/anthropic_messages.go:581` | Request entity too large | Unclear — see below | No. Shrink the request |
| `429` | `internal/server/anthropic_messages.go:583` | Rate limited | Unclear — see below | Yes, with backoff |

**`503` and `500` are the pair people get backwards, and the doc used to as
well.** `503 Routing failed` comes from `s.router.Route()` returning an error
(`internal/server/server.go:1291`) — the router could not pick a provider at all,
so no vendor was contacted and nothing was spent. Chain exhaustion is the *other*
one: `attemptCompletionWithRetryAndFallback` returning an error surfaces as
`500 Completion failed` (`internal/server/server.go:1796`), logged internally as
`All completion attempts failed`. By definition several vendor calls may have
been made and billed before you saw it. Retrying a `503` is cheap; retrying a
`500` re-runs the whole chain and pays for it again.

The other `503` in the codebase (`internal/server/server.go:2789`) belongs to
`POST /v1/routing/decision`, a dry-run endpoint that returns the routing decision
without completing anything. It is not a completion error and never bills.

**No `Retry-After` header is emitted on any of these.** The only `Retry-After`
in the service is written by the internal rate limiter
(`internal/security/ratelimit.go:275`), which is a separate middleware from the
gateway path documented here. Use your own bounded exponential backoff; do not
wait for a header that will not arrive.

**What the gateway does with a `429` is determinable, and it does count it.**
Both classifiers treat rate limiting as a real attempt: `ClassifyError` returns
the non-ejecting `RateLimited` outcome, so it does not count against the
provider's health (`pkg/aiqg/breaker/breaker.go:391`), while `ClassifyFailure`
returns `FailureRateLimited` with `eligible = true`
(`pkg/aiqg/breaker/breaker.go:549`), which makes it fallback-eligible. If the
tenant's `fallback.on` includes it, the chain advances, `AttemptCount` increments
(`internal/server/fallback.go:156`), and a second vendor is called and billed. So
a single `429` you observe may sit behind more than one upstream attempt.

> [!UNVERIFIED] Whether the *vendor* meters a rate-limited or oversized request
> as billable is the vendor's policy; no code in this gateway records it either
> way. The gateway's own accounting is described above — this marker covers only
> the vendor's side, which you should confirm against your vendor contract.

**Every 5xx from the attempt chain arrives as `500`, whatever the vendor said.**
This is a verified source reading, not an assumption: `handleNonStreamingCompletion`
and its retrying variant both render any chain failure as
`500 Completion failed: <wrapped error>` (`internal/server/server.go:1669`,
`internal/server/server.go:1796`), and no code path maps an upstream `502` or
`504` onto the response status. The vendor's own status survives only as text
inside that message. Do not parse it — it is a `fmt.Errorf` chain, not a
contract. Branch on `500` and treat the message as a log line.

The `422` is the one to handle deliberately, because it is the gateway doing its
job rather than failing. The message names the **patterns** that matched, never
the matched values — quoting the secret back would leak it to whoever triggered
the block (`internal/server/enforcement.go:132`). Each one also increments
`llm_router_blocked_requests_total` labelled by direction
(`internal/server/enforcement.go:127`), so a spike in `422`s you cause is visible
to the operator as inbound blocks rather than as generic errors. A block only
happens in enforcing mode; in
`observe` mode the same finding is recorded as what policy *would* have done and
the request proceeds, so the same input can return `200` or `422` depending on
tenant configuration you cannot see from the response.

> [!UNVERIFIED] The post-authentication rows above were not exercised live, so
> the exact body shape for each is unconfirmed. Treat the status codes as
> reliable and the envelopes as provisional until probed with a real token.

### Streaming has no error channel

Every status above assumes the response headers have not been sent yet. Once a
stream starts they cannot be used, and the streaming path has no substitute.

`handleStreamingCompletion` writes `200` and the server-sent events (SSE) headers
before it reads the
first chunk (`internal/server/server.go:1765`), then ranges over the provider's
chunk channel and calls `done()` when the channel closes
(`internal/server/server.go:1772`–`1784`). The chunk type carries no error field
(`internal/types/responses.go:60`) and the encoder interface has exactly two
methods, `writeChunk` and `done` (`internal/server/anthropic_messages.go:600`).
There is nowhere for a mid-stream failure to go. A vendor that dies halfway
through closes the channel, and the stream terminates with the ordinary
terminator — `data: [DONE]` on the OpenAI surfaces
(`internal/server/anthropic_messages.go:652`) or the normal
`content_block_stop`/`message_stop` sequence on `/v1/messages`
(`internal/server/anthropic_messages.go:822`).

**A truncated stream is therefore byte-indistinguishable from a complete one at
the protocol level.** Your client must decide from the payload: check
`finish_reason` (OpenAI) or `stop_reason` (Anthropic) on the final chunk and
treat absent or empty as a failure, rather than trusting that `[DONE]` arrived.

### Client disconnect and transport failures

If your client times out locally or the connection drops, you never see a status
at all — but the request does not stop there, and the outcome is worth
understanding before you set an aggressive client timeout.

The request context flows unchanged from the HTTP handler down into the vendor
call: `completeWithFallback` takes `ctx := r.Context()`
(`internal/server/fallback.go:50`) and hands it to `attempt`, which passes it
straight to `provider.ChatCompletion(ctx, req)`
(`internal/server/fallback.go:194`). Go's HTTP server cancels that context when
the client goes away, so **the upstream vendor call is cancelled too** — the
gateway does not keep generating tokens for a caller that left.

The sharp edge is what happens next. The resulting error stringifies as
`context canceled`, and `ClassifyFailure` matches that alongside real timeouts
and returns `FailureTimeout` with `eligible = true`
(`pkg/aiqg/breaker/breaker.go:554`). A cancelled request is therefore
**fallback-eligible**: if the tenant has a chain configured and `timeout` is in
its `fallback.on`, the gateway will advance a tier and call another vendor on
behalf of a client that has already hung up. The disconnect that saved you one
call can cost you the next one.

> [!UNVERIFIED] Whether that second attempt actually completes and bills depends
> on whether the cancelled context reaches the new provider before its request is
> issued — a race this reading cannot settle, and one no test in the repository
> covers. The classification and the eligibility are verified; the billing
> consequence is inferred. If aggressive client timeouts matter to your cost
> model, measure it rather than trusting this paragraph.

One more streaming surprise: if the provider does not support streaming at all,
the request silently becomes non-streaming. `StreamCompletion` returning an error
falls through to `handleNonStreamingCompletion`
(`internal/server/server.go:1741`–`1746`), so a request that set `"stream": true`
can come back as a single ordinary JSON body with no SSE framing. A client that
assumes SSE because it asked for SSE will fail to parse a perfectly successful
response. Branch on the response `Content-Type`, not on what you requested.

## Extension points

**Adding a provider** — implement the provider interface under
`internal/providers/`. Routing and the fallback chain operate on the interface,
so a new provider participates without touching the surfaces.

**Adding a wire surface** — translate at the boundary and converge on the shared
pipeline, as `internal/server/anthropic_messages.go` and
`internal/server/responses_api.go` both do. Register in the same block at
`internal/server/server.go:857`, wrapped in `wrapAIQG`; that wrapper is what
gives the new surface both governance and telemetry, so a route registered
outside it is silently uncounted.

**Adding a metric** has one rule that matters more than the mechanics: declare it
in `internal/metrics/metrics.go` and add it to the `MustRegister` call at
`internal/metrics/metrics.go:152` **in the same change that adds its call site**.
A series with no call site is exactly the shape of the bug this package replaced,
and three of them already exist here — the point of the registry being one
enumerable file is that a reviewer can see the gap. For anything derived from
state that already lives somewhere else, register a collector that reads it at
scrape time instead of mirroring it into a gauge; `RegisterProviderHealth`
(`internal/metrics/metrics.go:195`) is the worked example, and its comment
explains why a mirrored gauge is the same failure arrived at honestly.

Two mechanical constraints on that path. `RegisterProviderHealth` returns rather
than panics on a duplicate registration, and the caller tolerates
`prometheus.AlreadyRegisteredError` specifically so a second `NewServer` in one
process — which the tests do — is not fatal (`internal/server/server.go:345`).
If you add another scrape-time collector, match that handling or the second
server construction fails. And anything wrapping a `http.ResponseWriter` in the
request path must forward `Flush`, as `statusRecorder` does
(`internal/metrics/middleware.go:39`): the streaming handlers type-assert to
`http.Flusher`, and a wrapper that does not implement it breaks server-sent
events without failing anything.

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
`internal/server/enforcement.go:34` as a security feature whose rollout weakens
security, which is the wrong shape however clean the architecture.

**A stuck exporter would have been better than the one that shipped.** Until
`b6070a0` the `/metrics` handler built the exposition format with `fmt.Sprintf`
and derived nearly every value from `time.Now().Unix() / 10`. The rejected
framing is the intuitive one — that fake numbers carry no information, a
placeholder to be replaced later. They are worse than that, and the direction of
the harm is the whole argument. A counter that is stuck yields `rate() == 0`,
which looks broken and gets investigated. A counter derived from the clock yields
a plausible constant: dashboards showed a steady ~0.8 req/s and a
"traffic has stopped" alert could not fire, on a service serving no traffic at
all. Both scrape jobs reported healthy throughout. On a gateway whose stated
purpose is cost attribution, `llm_router_cost_total` climbed roughly $0.05 every
ten seconds — about $13k/month of spend nobody incurred. The reasoning is
preserved in the package doc at `internal/metrics/metrics.go:5`–`18` rather than
only in the commit, because it is the argument that keeps the next well-meaning
placeholder out.

**The eight dead series were deleted, not reimplemented.** They had no data
source anywhere outside the mock handler. The alternative — keep emitting them
with plausible constants until real instrumentation arrives — loses on the same
ground: a dashboard renders a hardcoded security score of 85 as a measurement, so
the fake value is not neutral, it actively asserts something false. Building the
instrumentation behind them is feature work rather than metrics plumbing, so it
was left undone and visible instead of done badly and hidden
(`internal/metrics/metrics.go:26`–`35`). `TestNoFabricatedSeries`
(`internal/metrics/metrics_test.go:48`) makes the deletion durable, and
`TestCountersDoNotAdvanceWithoutTraffic` (`internal/metrics/metrics_test.go:21`)
scrapes twice with no traffic between and fails if anything moved, so a
reintroduced clock-derived value fails the build rather than the on-call.

**Dropping `client_ip` from `requests_total` costs a debugging affordance and
buys a bounded series count.** Per-caller attribution is genuinely useful, and
with five hardcoded addresses the label was free. Against real traffic every
distinct caller address becomes a new time series, which is the standard
cardinality failure. Per-caller attribution already exists in the AIQG event
stream (defined under vocabulary), where it is keyed by tenant rather than by
network address, so the capability was not lost — only its cheapest and worst
implementation. Note where that leaves you as an integrator: per-caller figures
are real but live behind the dashboard backend, not behind any endpoint on this
gateway.

**Metrics wrap outside the AIQG middleware, not inside.** Wrapping inside would
count only requests that survived authentication, which reads as the tidier
boundary: metrics about "real" traffic. It is the wrong side. An authentication
outage then presents as traffic disappearing from the exporter, indistinguishable
from a quiet period, and the old exporter's blindness to rejected requests was
part of what let it look healthy for so long. Requests refused at the gate are
counted with `provider="none"` so that auth noise never inflates a vendor's
apparent traffic (`internal/metrics/middleware.go:49`–`51`).

**Provider health is collected at scrape time rather than mirrored into a
gauge.** A mirrored gauge must be updated from every path where health changes,
and any path that forgets leaves the metric asserting a stale value forever —
the same class of failure as the mock handler, reached by accident rather than
by construction. Reading
`router.GetHealthStatus()` inside `Collect` cannot drift
(`internal/metrics/metrics.go:165`–`171`). The cost is that the function runs on
every scrape, which is why it is documented as needing to be cheap and
concurrency-safe (`internal/metrics/metrics.go:192`).

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

**A query against `/metrics` returns nothing where it used to return numbers** —
work down the four zero-row causes listed under the telemetry section: a series
with no producer, a series nothing has incremented on this pod yet, `GET` traffic
that is never counted, or an unregistered `provider_health`. Before any of them,
though, rule out the pod serving the *old* exporter, in which case the numbers
are fabricated rather than absent. A query carrying `exported_service` also
breaks after the change, while one carrying `service` does not.

**Which exporter a pod is serving** is not answerable from the image tag: tags
are release-shaped (`aiqg-v5.86`), not commit-shaped, and this repository
publishes no tag-to-commit mapping. Ask the endpoint instead. The old exporter
emits `llm_router_security_score`, which the new one deliberately cannot
(`internal/metrics/metrics_test.go:48`); the new one emits
`llm_router_request_duration_seconds`, which never existed before. Either
direction settles it:

```bash
curl -sS -k https://gateway.aiqg.tas.scharber.com/metrics \
  | grep -c '^llm_router_security_score'
1
```

A count of `1` means the old hand-rolled exporter — every number on that endpoint
is derived from wall-clock time. `0` means `b6070a0` or later. On 2026-08-25 both
Deployments answered `1`: `llm-router-aiqg` on `aiqg-v5.86` and `llm-router` on
`aiqg-v5.75`. Neither has the fix yet, so the skew affects both hosts, not only
the customer-facing one.

**`llm_router_tokens_total` and `llm_router_cost_total` under-count your
traffic** — they are fed only from the non-streaming completion paths
(`internal/server/server.go:1681` and `internal/server/server.go:1855`). A
`"stream": true` request is counted in `requests_total` and timed in
`request_duration_seconds`, but contributes no tokens and no cost. If your
integration streams, these two series are not a spend figure; the AIQG event
stream (defined under vocabulary) is, and you read it through the dashboard
backend rather than through this gateway.

## Limits & trade-offs

**Read endpoints are unauthenticated.** `/v1/models`, `/v1/providers`, and
`/v1/capabilities` returned `200` to anonymous requests in live probes,
disclosing the model catalogue, the provider list, and the capability matrix
including context-window sizes. This is a deliberate convenience for SDK
discovery and also a public exposure — treat the catalogue as public information.

**`/metrics` is unauthenticated too, and it now carries real numbers.** It is
registered on the root router without `wrapAIQG`
(`internal/server/server.go:898`), so anyone who can reach the endpoint reads it.
Before `b6070a0` that exposed fiction; afterwards it exposes real request rates,
real token volumes, and real dollar cost per provider and model. The series carry
no tenant label, so this is aggregate rather than per-customer disclosure, but
treat the endpoint as sensitive in a way it was not before and confirm your
deployment restricts the path at the ingress rather than relying on obscurity.

**Metrics are process-local and reset on restart.** The registry lives in
process memory (`internal/metrics/metrics.go:48`), so every counter starts at
zero on a new pod and each replica reports only its own share. Aggregate across
replicas in the query, and expect `rate()` — not the raw counter — to be the
thing that survives a rollout.

**Nothing is idempotent.** Every completion call bills and emits telemetry, and
there is no request-id deduplication: a search of the repository at `eee4b24`
finds no idempotency key, no replay cache, and no dedup check on the completion
path. A supplied `id` is not consulted for replay — when absent the handler
mints a fresh one from the clock (`internal/server/server.go:986`–`988`) and
otherwise passes yours through as a label. A client retry is a second charge,
which is why the SDK max-retries advice under Getting started matters.

**Interposition is unavoidable.** Every request is parsed and re-serialized. A
vendor feature that has no representation in the translation layer is unavailable
until the layer learns it, and there is no escape hatch.

**Version skew between deployments.** `llm-router` and `llm-router-aiqg` run
independent image tags — observed on 2026-08-25 as `aiqg-v5.75` and `aiqg-v5.86`
respectively, eleven releases apart. Behaviour verified against one is not
guaranteed on the other; see the operations document, and use the exporter probe
under failure modes to establish what a given pod is actually running.

## Related

- OpenAPI specification: `docs/openapi.yaml` (served at `/docs`)
- Data models: `aether-shared/data-models/tas-llm-router/`
- Operations and on-call: `docs/ops/llm-router.md`
- Routing design: `aether-shared/data-models/aiqg/routing-decision.md`
- Metric definitions and the reasoning behind them: `internal/metrics/metrics.go`
- Metric guarantees expressed as tests: `internal/metrics/metrics_test.go`
