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
  - "Can the gateway run a different model from the one I named, and how would I know?"
depth: deep
verified_against: "tas-llm-router@552d869, 2026-09-21"
---

# LLM Router — Developer Guide

> **Verified against `tas-llm-router@552d869` on 2026-09-21** (previous
> verification: `eee4b24`, 2026-08-25). Wire behaviour and the model catalogue
> were first captured from live probes against `gateway.aiqg.tas.scharber.com`
> on 2026-08-24 and 2026-08-25; the authentication rejections, the `415`, the
> read surfaces, and `/metrics` were re-probed on 2026-09-21. Responses shown are
> real captures. Where a behaviour was read from source rather than observed, it
> says so.
>
> **The code and the cluster have diverged, and this refresh widens the gap.**
> On 2026-09-21 `kubectl get deploy -n tas-llm-router` still showed
> `llm-router-aiqg` on image `aiqg-v5.86` and `llm-router` on `aiqg-v5.75` — the
> same tags as on 2026-08-25 — and both still served the old hand-rolled
> `/metrics` exporter, which was removed at `b6070a0`. Because `b6070a0` is an
> ancestor of `eee4b24`, **nothing committed between `eee4b24` and `552d869` is
> running in the cluster**: not the model registry (an optional component that
> can rewrite the model name you send), not the in-band streaming errors (an
> error event sent inside a stream that has already started), not the corrected
> vision flag, not the fail-closed token check (rejecting every token when the
> gateway has no way to look tokens up). Each is defined properly below.
> Sections that describe such behaviour say **committed, not yet deployed**.
> Treat them as the contract you will get after the next rollout, and the
> live probes quoted beside them as the contract you get today.

## Why this exists

Tributary AI Services (TAS) is the platform this repository belongs to, and
every `TAS-*` header, `tas_qg_live_` token, and `*.tas.scharber.com` host in this
document is named after it. This gateway is the AI Quality Gateway (AIQG) ingress — the name appears
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
  `ModeObserve` with no rules (`internal/middleware/aiqg.go:1231`–`1241`). This
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
  `/metrics` (`internal/metrics/metrics.go:50`) and `pkg/aiqg/metrics.Registry`
  behind `/aiqg/metrics`. Neither registers into the library's default registerer,
  so the surface each endpoint exposes is enumerable by reading one file
- **the AIQG event stream** — the per-request record this gateway emits, distinct
  from and far richer than `/metrics`. Each completed request produces a paired
  CloudEvent carrying tenant, token, model, token counts, cost, and routing
  decision. The gateway fans them to two sinks at once (`AIQG_EMITTER_TYPE:
  "both"`, `k8s/configmap.yaml:117`): to `logrus`, and therefore to Loki, which is
  the read path the AIQG dashboard backend uses; and to the Kafka topic
  `tas.aiqg.events.v1` (`k8s/configmap.yaml:119`), consumed by a Spark aggregator
  that materializes the `aiqg.event_metrics` hypertable in TimescaleDB
  (`k8s/configmap.yaml:111`–`115`). **It is not a surface an integrator calls.**
  There is no endpoint on this gateway that reads events back. If you need your
  own spend or attribution figures, the reachable surface is the AIQG dashboard
  backend at `aiqg-dashboard-be` (`k8s/configmap.yaml:109`), not this service —
  ask the AIQG owners for access rather than expecting a route here. Since
  `d8da473` (committed, not yet deployed), a Kafka broker that is unreachable at
  startup no longer stops the gateway from serving: it drops to the log sink
  alone and raises the gauge `aiqg_emitter_degraded` to `1`
  (`internal/server/server.go:226`–`236`). Your requests are unaffected; the
  Kafka-fed aggregates miss events until a restart reconnects
- **bring your own key (BYOK)** — a tenant storing its own vendor API key in the
  gateway's encrypted credential vault, so upstream calls bill the tenant's vendor
  account rather than the TAS shared key. A **BYOK-only** tenant has also switched
  off fallback to the shared key
- **model registry** — an optional, off-by-default component (added `a3804d6`
  through `e4548af`) that discovers which models each vendor currently serves,
  caches a status per model (`active`, `deprecated`, `unavailable`), and lets
  routing rewrite the requested model name before a provider is chosen. An
  **alias** is an operator-configured name that the registry maps to a concrete
  model
- **prompt-cache breakpoint** — Anthropic's `cache_control` marker: "cache the
  prompt prefix up to and including this block". The API accepts at most four
  per request
- **strict / permissive mode** — the two authentication postures of the same
  binary. A strict ingress rejects any completion request without a gateway
  token; a permissive one lets token-less internal callers through. Which host is
  which is under Getting started
- **gatekeeper** — the older content scanner that runs on every completion
  independently of policy enforcement. It can refuse a prompt (`403 request
  blocked by content policy`) or a vendor's answer, and it is separate from the
  per-tenant policy block that returns `422`
- **route rule** — an entry in a tenant's policy bundle that matches traffic
  (by model or workflow) and can pin a provider (`provider_override`), choose a
  routing strategy, set limits, and name a fallback chain. The gateway fetches
  the resolved rule per request from the AIQG dashboard backend's
  `/internal/policy/resolve` (`pkg/aiqg/policy/policy.go:141`–`159`); rules are
  tenant data managed there, not code in this repository
- **`TAS-Policy` / `TAS-Policy-Bundle`** — optional request headers that name
  which policies (a comma-separated list) or which single named bundle should
  govern this request. Sending both is a `400`
  (`internal/middleware/aiqg_headers.go:8`–`14`)
- **in-band streaming error** — once a stream has sent its `200`, a failure can
  only be reported inside the stream, as a final error event in the surface's
  dialect. Added by #172; see error semantics
- **fail-closed token check** — since #173, a strict gateway with no configured
  way to look tokens up rejects every request, rather than accepting any
  correctly-prefixed token with a blank tenant
- **semantic cache** — a response cache that serves a stored answer to a prompt
  whose embedding is close enough to an earlier one, as opposed to the
  exact-match response cache that requires a byte-identical request. Design in
  `docs/AIQG-SEMANTIC-CACHING.md`; its `llm_router_semcache_*` series are below
- **experiment / shadow replay** — an experiment splits a tenant's traffic
  between a control and variants (a different model or provider); a shadow
  replay re-runs a sampled control request through each variant offline so a
  judge can compare them, without the caller seeing the variant's answer
  (`pkg/aiqg/experiments/experiments.go:61`–`81`, `docs/AIQG-EXPERIMENTS-RUNNER.md`)
- **breaker ejection** — the circuit breaker (`pkg/aiqg/breaker`) temporarily
  removes ("ejects") a provider from selection after consecutive errors or a
  high error rate, for a cool-down period; `/v1/breaker` reports who is ejected
  and the thresholds. A `429` does not count toward ejection
- **`#172`-style, `SEC-1`-style, and `OPS-27`-style references** — a number
  after `#` is an issue or pull request in the `tas-llm-router` GitHub
  repository; `SEC-` and `OPS-` numbers are items in the TAS
  platform backlog. They are cited so you can find the discussion, and each
  sentence that cites one also states what it changed

You reach for this instead of calling Anthropic or OpenAI directly when the call
must be governed: scanned for sensitive content before it leaves the cluster,
attributed to a tenant for spend, routed to whichever provider is cheapest or
healthiest right now, and able to survive one vendor failing without the caller
writing failover logic.

Two of those promises need qualifying against what the code does today (verified
at `552d869`):

- **Cost routing happens only when your model name does not pin a vendor.** A
  model name that exactly one provider lists — every name in the current
  catalogue — routes to that provider, full stop; the "cheapest provider" choice
  applies only to a name no provider lists, or several do (see "`model` pins the
  vendor" under data model & contracts).
- **Automatic failover today requires you to ask for it in the request.** The
  tenant-configured fallback chain is not reachable from any route (see the
  note under "How it works end to end"). What does run is the older per-request
  mechanism: send `fallback_config` with `"enabled": true` in the request body
  (`internal/types/requests.go:33`, fields at `:209`–`214`) and a failed attempt
  is retried on each other configured provider in turn
  (`internal/server/server.go:2504`–`2507`, `:2586`–`2614`); add `retry_config`
  to retry the same provider first. Without either, one vendor failure is one
  `500`. Each extra attempt may bill. Note that this loop passes your request
  on **with the same model name** and picks "every provider except the one that
  failed" (`internal/server/server.go:2691`–`2703`), so for a model only one
  vendor serves, the fallback call will most likely be rejected by the other
  vendor as well — not exercised live, so treat that consequence as inferred.

**Is using the gateway mandatory?** Not recorded. The repository's
root guidance file for coding assistants describes it as "the centralized LLM
gateway for all TAS services",
but no policy document, network policy, or egress rule in this repository or
`aether-shared/k8s-shared-infrastructure` requires TAS services to use it
instead of calling vendors directly. Ask the platform owners before treating
it as optional.

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
  scan --> reg[resolveViaRegistry<br/>alias + model fallback, optional]
  reg --> route[Route: select provider]
  route --> call[provider call]
  call --> cls{failure?}
  cls -->|classify twice| chain[walk fallback chain]
  chain --> call
  cls -->|success| render[render in caller's dialect]
```

Five abstractions carry the design.

**Surfaces** are wire dialects, not pipelines. `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses` translate at the boundary and converge on one
shared pipeline (`internal/server/server.go:934`, `internal/server/server.go:938`,
`internal/server/server.go:947`). The surface you
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
(`internal/server/server.go:982`). Telemetry is an abstraction here and not a
detail because its ordering relative to the other four is a contract, not an
implementation choice — see the middleware ordering paragraph below.

**The model registry** is a sixth, optional abstraction, committed since
`eee4b24` and not yet deployed. When `registry.enabled` is set in configuration
(`internal/config/config.go:37`–`49`; off by default, and not set in any manifest
under `k8s/`), `Route` calls `resolveViaRegistry` before it picks a strategy
(`internal/routing/router.go:249`–`255`). That step may rewrite the model name
you sent — to an alias's target, or away from a model the registry has marked
`unavailable` — and it reads only a cached status, never the vendor, so it adds
no network call to your request (`internal/routing/router.go:86`–`95`). It owns
the *name*; it does not own provider selection, which still happens afterwards
exactly as described below.

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

A request arrives at the customer ingress and passes three router-wide checks
before anything specific to its route. They are registered with `r.Use` on the
whole router (`internal/server/server.go:898`–`913`), so they run ahead of
authentication and apply to every path: the security middleware, whose request
validator rejects a disallowed method, a body over 10 MiB, or a `Content-Type`
outside `application/json` and `text/plain` with `400`
(`internal/security/validation.go:92`–`111`, `internal/config/config.go:1109`–`1115`);
the request logger; and `contentTypeMiddleware`, which rejects any `POST` or
`PUT` whose `Content-Type` is not **exactly** `application/json` with `415`
(`internal/server/server.go:1047`–`1058`). The string comparison is literal, so
`application/json; charset=utf-8` is refused — observed live on 2026-09-21. The
first check allows `text/plain` and the third refuses it, so in practice only the
bare `application/json` value reaches authentication. A fourth hook, the
OpenAPI schema validator registered at `internal/server/server.go:904`, is inert
in this binary: `ToServerConfig` never sets its configuration
(`internal/config/config.go:1003`–`1011`), so no request body is schema-checked
before authentication. A live probe on 2026-09-21 agreed — a schema-invalid body
with no token got the `401`, not a `400`.

The request is then counted before it is
authenticated. `wrapAIQG` (`internal/server/server.go:922`) composes each
completion route as `metrics.Middleware(aiqgMiddleware(handler))`, so the
metrics wrapper is the outermost layer and sees every request the gateway later
refuses. That ordering is deliberate and stated as such at
`internal/server/server.go:927`: an authentication failure is traffic, and an
exporter blind to it cannot show an auth outage. The practical consequence for
you is that a `401` you caused appears in `llm_router_requests_total` with
`provider="none"`, not as a gap.

Then `ParseHeaders`
(`internal/middleware/aiqg_headers.go:168`) lifts `TAS-Auth` and validates its
shape: it must carry the `tas_qg_live_` prefix
(`internal/middleware/aiqg_headers.go:141`), and a value that does not is
rejected as `ErrAuthMalformed` (`internal/middleware/aiqg_headers.go:148`)
**before** any authentication lookup.
That ordering is why a malformed token and an unknown token produce different
status codes — see error semantics.

The surface handler parses the body in its own dialect. `handleChatCompletion`
(`internal/server/server.go:1063`) reads OpenAI shape; `handleMessages`
(`internal/server/anthropic_messages.go:487`) reads Anthropic shape, including
top-level `system` and required `max_tokens`. Both produce the same internal
request, and both are wrapped by `wrapAIQG`, which is what attaches the
governance pipeline.

The prompt is scanned and `decideEnforcement` resolves findings against the
tenant's policy. In `observe` mode policy decides nothing and the pre-existing
controls keep running unchanged — it records what it *would* have done. Only in
enforcing mode can it block.

Routing then selects a provider and the call goes out. If the model registry is
enabled, `Route` first lets it rewrite the model name
(`internal/routing/router.go:252`), and records any rewrite on the routing
metadata (`internal/routing/router.go:290`–`298`). A route rule that pins a
provider is honoured only if that provider is permitted, configured, healthy,
and — since `6d57096` (#151), committed but not deployed — actually lists the
requested model; otherwise the pin is set aside with the reason recorded
(`internal/routing/router.go:1272`–`1293`). On failure the attempt is
classified **twice**, by two functions that deliberately disagree:
`ClassifyError` (`pkg/aiqg/breaker/breaker.go:382`) asks *should this count
against the provider?*, and `ClassifyFailure`
(`pkg/aiqg/breaker/breaker.go:533`) asks *would a different
provider do better?* A 429 must not eject a healthy vendor, yet another vendor
has capacity. A context overflow is never the provider's fault, yet a
larger-window tier serves it unchanged. If the failure class is in the tenant's
`fallback.on` list, the chain advances and the loop repeats.

> [!UNVERIFIED] **The chain walk described above does not appear to be reachable
> from any HTTP route** (new finding, 2026-09-21). The walk lives in
> `completeWithFallback` (`internal/server/fallback.go:44`), whose only caller is
> `handleNonStreamingCompletion` (`internal/server/server.go:1756`), whose only
> caller is the streaming-unsupported branch of `handleStreamingCompletion`
> (`internal/server/server.go:1889`) — and nothing calls
> `handleStreamingCompletion`. `handleChatCompletion` always dispatches to the
> older `handleStreamingCompletionWithRetry` or
> `handleNonStreamingCompletionWithRetry` (`internal/server/server.go:1407`–`1412`),
> and that path makes one attempt, retries only if the request body carried a
> `retry_config`, and tries other providers only if it carried a
> `fallback_config` (`internal/server/server.go:2494`–`2583`). Both are request
> body fields (`internal/types/requests.go:32`–`33`, copied from `extra_body` on
> the Anthropic surface at `internal/server/tas_extensions.go:34`–`39`), and no
> code fills them with a default. The bypass
> predates this refresh: `626060d` (#153) wired the walk into
> `handleNonStreamingCompletion` while the dispatch already went elsewhere, and
> no test drives the walk through a route. The tenant chain *is* still used at
> routing time, when a set-aside pin enters tier 1. Read every statement below
> about tiers advancing, `fallback.on`, and multi-hop billing as the design, and
> confirm against a live request with a configured chain before relying on it.

The response is rendered back in the dialect of the surface that was called.

On the way out, two things are recorded that you can later query. The completion
handler stamps the routing decision into `X-TAS-Router-*` response headers
(`internal/server/server.go:1806`) and, when the vendor reported usage, feeds the
same token counts and the same `clear.DollarCost` result into the metrics
registry that the spend record uses (`internal/server/server.go:1772` and
`internal/server/server.go:1774`; the fallback-walking variant repeats it at
`internal/server/server.go:1995` and `internal/server/server.go:1997`; since
`cddd372`, committed but not deployed, the streaming path does the same at
`internal/server/server.go:1872`–`1876`). Sharing
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
(`internal/middleware/aiqg.go:417`–`435`, called at
`internal/middleware/aiqg.go:155`), after which the rest of the chain resolves it
identically. The prefix is what keeps this unambiguous: a real vendor key
(`sk-…`, `sk-ant-…`) never matches and falls through untouched
(`internal/middleware/aiqg.go:146`–`148`). Whichever carrier you use, it is one
token and one auth path — the sections below say `TAS-Auth` for brevity.

`-k` below skips certificate verification: the ingress serves a certificate
from the internal `tas-ca-issuer`, which is not in a default trust store. Your
HTTP client will need the same accommodation, or the issuer's certificate.

```bash
curl -sS -k https://gateway.aiqg.tas.scharber.com/v1/models
{"object":"list","data":[{"id":"claude-haiku-4-5-20251001","object":"model","created":0,"owned_by":"anthropic"},{"id":"claude-opus-4-6","object":"model","created":0,"owned_by":"anthropic"},{"id":"claude-sonnet-4-6","object":"model","created":0,"owned_by":"anthropic"},{"id":"gpt-3.5-turbo","object":"model","created":0,"owned_by":"openai"},{"id":"gpt-4o","object":"model","created":0,"owned_by":"openai"},{"id":"gpt-4o-mini","object":"model","created":0,"owned_by":"openai"}]}
```

(Captured in full on 2026-09-21; the catalogue is unchanged from 2026-08-24.)

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
(`k8s/deployment-aiqg-strict.yaml:99`) and strict mode is what makes a missing
`TAS-Auth` a `401` (`internal/middleware/aiqg.go:174`–`178`). The other Deployment,
`llm-router`, sits behind `llm-router.tas.scharber.com`
(`k8s/ingress.yaml:32`) and runs permissive: the AIQG middleware activates only
when a request actually carries `TAS-Auth`, and internal callers without it pass
through untouched (`internal/server/server.go:240`–`243`). Both live in namespace
`tas-llm-router`. Everything in this document describes the strict host unless it
says otherwise; the same binary serves both, so the wire contract is identical
and only the auth posture differs.

**The public hostname exposes only the completion routes.** Customers outside
the cluster reach the same strict Service at `https://gateway.air-ops.net`,
whose Ingress lists the six completion `POST` paths one by one with `pathType:
Exact` and nothing else (`k8s/ingress-gateway-airops.yaml:83`–`133`; SEC-1,
commit `8bc32bd`). Every other path returns nginx's `404` before reaching the
router. Probed on 2026-09-21: `GET /v1/models` and `GET /metrics` returned `404`,
and an unauthenticated `POST /v1/chat/completions` returned the gateway's `401`.
The consequence for a stock SDK is that `client.models.list()` fails on the
public host even though it works on `gateway.aiqg.tas.scharber.com`; hard-code
the model name from this document's catalogue, or query the internal host. The
Ingress comment gives the reason: the read routes carry no authentication, and
publishing them was the exposure SEC-1 closed
(`k8s/ingress-gateway-airops.yaml:27`–`35`).

**Getting a token — you self-serve.** Issuance does not live in this repository
and is not an ops ticket. It is a first-class API on the AIQG dashboard backend
(the `aiqg-dashboard-be` repository), hosted at `https://api.aiqg.tas.scharber.com`
and surfaced in the dashboard user interface (UI) at `/tokens`. Three routes —
list (GET), issue (POST), and revoke (DELETE):

| Route | Does |
|---|---|
| `GET /api/v1/account/tokens` | List your tenant's tokens (metadata only — never the secret) |
| `POST /api/v1/account/tokens` | Issue one; body carries `source_app` and `label` |
| `DELETE /api/v1/account/tokens/:id` | Revoke one |

These are authenticated with a Keycloak JSON Web Token (JWT), not with a gateway
token, and **the tenant is taken from your JWT** — you issue for your own tenant and cannot issue
for anyone else's. The fastest route is the dashboard UI: sign in, open
`/tokens`, create one naming your `source_app`, and copy the value.

**Where the dashboard is, and how you get a login.** The dashboard UI is
`https://aiqg.tas.scharber.com` inside the network and `https://aiqg.air-ops.net`
from outside (the `host:` rules in `aiqg-ui/k8s/ingress.yaml` and `aiqg-ui/k8s/ingress-public.yaml`);
the external tester guide calls the token page "Settings → API Tokens"
(`deploy/aiqg-public/TEST-USER-GUIDE.md`, "Optional — send test traffic"). Signing in redirects to Keycloak,
realm `aether`, client `aiqg-ui` (`aiqg-ui/README.md`, "Signing in is required to see anything"). **There is no
self-registration** — the UI's `/signup` route is a placeholder
(`aiqg-ui/README.md`, "Genuinely unfinished"). An account is usable only when it exists in realm
`aether` *and* carries a `tenant_id` user attribute, which a Keycloak attribute
mapper copies into your JWT; without it every `/api/v1` call is a `403`. The
one provisioning path recorded anywhere is `deploy/aiqg-public/manage-testers.py
add <email>` in the sibling `deploy` repository, run by someone holding
realm-admin credentials for `aether`: it creates the user, grants the
`aiqg-access` role, sets `tenant_id` (a shared demo tenant by default, or
`--tenant <uuid>` / `--isolated`), and adds the address to the Cloudflare Access
allowlist for `aiqg.air-ops.net` (`aiqg-dashboard-be/README.md`, "Where that claim comes from, and what this repository does not do"). If
you were signed in before being provisioned, sign out and back in — the claim is
added only at login. Neither repository names the person who runs that script;
ask the AIQG owners.

`POST` returns `201` with the plaintext token in the response body, and that is
**the only time it is ever shown**. It is not retrievable afterwards through
`GET`, which returns metadata only; a lost value can only be revoked and
reissued. Put it into your secret store on first receipt rather than into a
scratch buffer. Issued tokens match `^tas_qg_live_[0-9a-f]{40}$`, which is what
makes the gateway's prefix check work
(`internal/middleware/aiqg_headers.go:141`). Issuance writes an audit entry
recording the actor, the `source_app`, and the token prefix.

Unauthenticated calls are rejected before anything else, which is a quick way to
confirm you are hitting the right host. Verified on 2026-08-26:

```bash
curl -sS -k https://api.aiqg.tas.scharber.com/api/v1/account/tokens
{"error":{"code":"missing_header","message":"Authorization header missing"}}
```

Note the error shape differs from the gateway's — `missing_header` here versus
`path_a_auth_required` at the gateway. If you see the former, you are talking to
the token API; the latter means the gateway.

**Nothing in *this* repository describes issuance, and that distinction matters
when you are debugging a `401`.** The gateway only ever *consumes* tokens. It
reads them from the Kubernetes Secret `llm-router-aiqg-tokens` in namespace
`tas-llm-router` via the `AIQG_TOKENS_FILE` environment variable
(`k8s/deployment-aiqg-strict.yaml:102`, loaded at
`internal/config/config.go:860`), and the copy checked into this repository ships
empty on purpose (`k8s/secret-aiqg-tokens.yaml:1`–`6`). Each entry binds a token
to a `tenant_id`, an `aiqg_account_id`, a `source_app` string, and a `suspended`
flag (`k8s/secret-aiqg-tokens.yaml:10`–`16`) — the mechanism behind "tenant
scoping is implicit and carried by the token" below. So a token that the
dashboard issued successfully can still fail at the gateway if the gateway's
copy of the list has not caught up. Token values live only in the issuing
service and in `aether-secrets`, never in this repository and never in this
document.

**That Secret is only the fallback, and production does not use it.** The
gateway prefers a `DashboardResolver` whenever `AIQG_DASHBOARD_URL` and
`AIQG_DASHBOARD_INTERNAL_AUTH_TOKEN` are both set, and falls back to the Secret
list only otherwise (`internal/server/server.go:454`–`476`). The URL is in the
ConfigMap (`k8s/configmap.yaml:109`), the shared secret comes from
`llm-router-secret`, and Loki shows both strict pods logging `AIQG token
resolver: DashboardResolver (HTTP client of aiqg-dashboard-be)` at startup on
2026-09-21. The `DashboardResolver` asks `aiqg-dashboard-be` on **every request**
(`POST /internal/auth/validate`, 2-second timeout) and keeps no cache
(`pkg/aiqg/tokens/dashboard_resolver.go:93`–`166`). So on the production
gateway **there is no propagation delay on the gateway side**: a token works on
the first request after the `POST` that issued it, and a revoked token stops
working on the next request. The "has not caught up" sentence above applies only
to a gateway running on the Secret list. Whether `aiqg-dashboard-be` itself
caches validation is not recorded in this repository. To check a new token,
send one request: `401` with `"reason":"token_unknown"` means the backend does
not know it; a `503` with `token_resolver_unavailable` means the backend could
not be reached, and is worth retrying.

**Which host to use, and the certificate.** From outside the cluster use
`https://gateway.air-ops.net` (with `base_url` `https://gateway.air-ops.net/v1`
for the OpenAI SDK), which is what the tester guide documents
(`deploy/aiqg-public/TEST-USER-GUIDE.md`) and which served a
publicly trusted certificate on 2026-09-21 — no `-k` needed. It routes only the
six completion paths. The `*.tas.scharber.com` hosts in the examples below are
the internal ones: they present a certificate from the internal `tas-ca-issuer`,
whose root certificate is the `ca.crt` key of Secret `tas-root-ca-secret` in
namespace `cert-manager` (the `tas-root-ca` Certificate and `tas-ca-issuer` ClusterIssuer in `aether-shared/k8s-shared-infrastructure/self-signed-issuer.yaml`; confirmed with `kubectl` on 2026-09-21),
also exported as `aether-shared/k8s-shared-infrastructure/tas-root-ca.crt`. Trust
that file instead of passing `-k`:
`curl --cacert aether-shared/k8s-shared-infrastructure/tas-root-ca.crt …`
(verified 2026-09-21: `200` from `/v1/providers` with no `-k`).

**A gotcha that will mislead you in a non-production cluster.** When the token
list is empty, the AIQG middleware runs a permissive no-resolver path: any
`tas_qg_live_`-shaped bearer is accepted and events are emitted with empty tenant
fields (`k8s/secret-aiqg-tokens.yaml:18`–`21`). So a made-up token can appear to
work, right up until the list is populated and the same token starts returning
`401`. If your token did not come from the token API above, assume it is not
real — a value that "works" against an empty list proves nothing.

That gotcha now applies only to the permissive `llm-router` Deployment. Since
`d15ccfc` (#173), committed but not yet deployed, a **strict** ingress with an
empty token list and no dashboard resolver fails closed: every request carrying
a well-formed token gets `401` with reason `no_resolver_configured`
(`internal/middleware/aiqg.go:234`–`243`, body at `:1057`), and the gateway logs
an error at startup saying so. The comment in `k8s/secret-aiqg-tokens.yaml:18`–`21`
still describes the old permissive behaviour and is out of date for strict mode.
The live strict gateway does have a resolver: a well-formed made-up token
returned `token_unknown` on 2026-09-21.

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
(`internal/middleware/aiqg.go:158`–`169`), and then selects the effective
upstream key itself — your stored credential if you have one, otherwise the TAS
shared key (`internal/server/server.go:1425`–`1475`). Both constructor forms are
named as the worked examples in the source comment that describes this path
(`internal/middleware/aiqg.go:149`–`150`):

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
stored credential (`internal/server/server.go:1440`–`1449`). The raw
`Authorization` header is deliberately **not** used for this: clients
historically sent placeholders there, so injecting it would break them
(`internal/server/server.go:1441`–`1445`). The `base_url` values differ by vendor
convention: the OpenAI SDK appends paths under `/v1`, the Anthropic SDK supplies
its own.

**Two things change that the SDK will not warn you about.** A malformed gateway
token returns `400`, so a stock SDK raises its bad-request exception rather than
its authentication exception — you will be looking at your request body while the
problem is your key. And both SDKs retry automatically by default, while nothing
here is idempotent: every retry is a second billed call. Set the SDK's max-retries
to 0 unless you have decided the duplicate spend is acceptable.

**A hand-rolled client has one more trap: the `Content-Type` must be exactly
`application/json`.** A client that appends a parameter —
`application/json; charset=utf-8` — gets `415` with body
`{"error":{"code":415,"message":"Content-Type must be application/json","type":"api_error"},"timestamp":…}`
before its token is even examined (observed 2026-09-21; the literal comparison is
at `internal/server/server.go:1051`).

## API reference

`docs/openapi.yaml` in this repository is the reference source of truth; where
this document and the spec disagree, the spec wins and the disagreement is a bug
worth reporting. Registration lines are cited so you can jump to the handler.

**Completion surfaces** — all `POST`, all require `TAS-Auth`, none idempotent
(each call bills and emits telemetry):

| Endpoint | Dialect | Notes |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `"stream": true` for server-sent events |
| `/v1/completions` | OpenAI | Compatibility shim — the handler delegates straight to `handleChatCompletion` (`internal/server/server.go:1744`–`1748`), so it decodes the **chat** body: `messages[]`, and a legacy `prompt` field is ignored |
| `/v1/messages` | Anthropic | Top-level `system`, **`max_tokens` required**, content block arrays, native named-event streaming |
| `/v1/messages/count_tokens` | Anthropic | Returns `{"input_tokens": N}` from the vendor's own count endpoint (`internal/server/count_tokens.go:66`); `max_tokens` not required |
| `/v1/embeddings` | OpenAI | Routes to the embeddings-capable provider. `encoding_format` is forwarded upstream (`internal/providers/openai/embeddings.go:27`), but the client library transparently decodes a base64 vendor response, so you receive float vectors either way (`internal/providers/openai/embeddings.go:15`–`17`) |
| `/v1/responses` | OpenAI Responses | Input items or string translated to messages; `output[]` / `output_text` returned |

All six are registered together in `internal/server/server.go:934`–`947`, if you
are extending rather than calling. Each is wrapped by `wrapAIQG`
(`internal/server/server.go:922`), which is also what puts the metrics middleware
around them.

**Read surfaces** — `GET`, no authentication enforced, and reachable only on the
internal hosts (the public `gateway.air-ops.net` does not route them). Every row
below was re-probed anonymously against the live gateway on 2026-08-25;
`/v1/models`, `/v1/providers`, `/v1/capabilities`, and `/v1/breaker` again on
2026-09-21:

| Endpoint | Returns |
|---|---|
| `/v1/models` | Model catalogue. **SDK-aware**: Anthropic's `{data:[{type:"model"}]}` shape when the caller sends `anthropic-version`, otherwise OpenAI's `{object:"list"}` |
| `/v1/models/{model}` | Single model, same dialect switching |
| — | Note: the Anthropic-shaped listing returns **only Anthropic models** (3 of the 6 in the catalogue, observed 2026-08-25), while the OpenAI-shaped listing returns all six |
| `/v1/providers` | `{"count":2,"providers":["openai","anthropic"]}` |
| `/v1/capabilities` | Per-provider capability matrix including `max_context_window` |
| `/v1/health`, `/health` | Provider health with per-provider `response_time_ms` |
| `/v1/breaker` | Provider-fleet circuit-breaker state: `enabled`, `targets` (ejected providers), and the breaker `config`. Since `3c7eb27` (#185), committed but not deployed, `enabled` means "ejection is on by default for a request with no tenant override" rather than "the breaker object exists", and two fields are added: `constructed` and `state` (`unavailable`, `off`, or `on`) (`internal/server/breaker_status.go:25`–`81`). Returns `500` with `breaker status unavailable: …` if the breaker store cannot be read (`internal/server/breaker_status.go:63`). The live gateway still returns the older shape without `state` |

The catalogue on 2026-08-24 was `claude-haiku-4-5-20251001`, `claude-opus-4-6`,
`claude-sonnet-4-6`, `gpt-3.5-turbo`, `gpt-4o`, `gpt-4o-mini`. Query the endpoint
rather than trusting that list — it is the authority, this document is not.

There are **no** `/v1/openai/*` or `/v1/anthropic/*` passthrough routes. Requests
are never reverse-proxied verbatim.

### Model registry admin API — committed, not yet deployed

Five routes, registered unconditionally at `internal/server/server.go:965`–`969`
and implemented in `internal/server/registry_admin.go` (added `e4548af`, closes
#6). They exist for operators managing the registry, not for completion
callers. **None is authenticated**: they are plain `api.HandleFunc` routes with
no `wrapAIQG`, like the other management endpoints, and they are absent from the
public `gateway.air-ops.net` allowlist. On 2026-09-21 the live internal gateway
returned `404` for `GET /v1/registry/status`, because the deployed image
predates them. With the registry disabled — the default — every one of them
returns `503` (`internal/server/registry_admin.go:82`–`88`).

| Route | Does | Body / response | Vendor calls |
|---|---|---|---|
| `GET /v1/registry/models` | Every registered model, grouped by provider | `{"providers":{"<provider>":[ModelInfo…]}}` | None |
| `GET /v1/registry/models/{provider}` | One provider's models | `{"provider":"…","models":[ModelInfo…]}` | None |
| `GET /v1/registry/status` | Last discovery pass | `{"enabled":true,"providers":[…],"last_sync":{"last_run","duration_ms","ok","runs","providers":{"<p>":{"discovered","active","deprecated","unavailable","duration_ms","error"}}}}` (`internal/registry/sync.go:55`–`71`) | None |
| `POST /v1/registry/sync` | Runs one discovery pass synchronously and returns the same summary | no body | **Yes** — an OpenAI list-models call, and one 1-token Anthropic Messages request per configured Anthropic model (`internal/registry/adapters/anthropic.go`, `internal/providers/anthropic/provider.go:362`–`378`) |
| `POST /v1/registry/validate` | Probes one model | request `{"provider":"…","model":"…"}`; response `{"provider","model","available":bool,"error"?}` — always `200`, even when the probe errored | **Yes** — one probe |

`ModelInfo` is the configured model record (`internal/types/capabilities.go`) plus
the registry fields `status` (`active`, `deprecated`, `unavailable`; empty means
`active`), `last_validated`, `deprecation_date`, `replacement_model`, and
`aliases`, all omitted when empty (`internal/types/capabilities.go:40`–`48`).

Errors, exhaustively, per `internal/server/registry_admin.go`. Bodies use the
standard OpenAI-style envelope described under error semantics.

| Status | Route | Message | Retryable |
|---|---|---|---|
| `503` | all five | `model registry is not enabled` | No — configuration, not load |
| `429` + `Retry-After: <seconds>` | `POST /sync` | `registry sync was run too recently; try again shortly` — one manual pass per 10 seconds, gateway-wide (`:31`, `:96`–`103`) | Yes, after the header's delay |
| `400` | `POST /validate` | `body must be {"provider":"...","model":"..."}` — unparseable body or either field empty (`:171`–`173`) | No |
| `404` | `GET /models/{provider}` | `unknown provider: <name>` (`:137`–`139`) | No |
| `500` | `GET /models/{provider}` | `failed to list models: …` (`:133`–`135`) | Yes |

`docs/openapi.yaml` lists the routes but omits that `500`; by this document's own
rule the spec is the source of truth, so treat the omission as a spec bug.

### Prompt caching controls

Anthropic bills a cached prompt prefix at a fraction of the normal input rate,
but only if the request marks where the cacheable prefix ends. The gateway
decides per request what happens to those marks, through the `TAS-Prompt-Cache`
request header (`pkg/aiqg/promptcache/mode.go:59`–`62`), applied before routing
(`internal/server/server.go:1152`, `internal/server/prompt_cache.go:30`–`75`):

- `passthrough` (alias `pass`) — forward exactly the breakpoints you sent. The
  default.
- `auto` — **discard yours** and let the gateway place up to its own: one at the
  end of the system prompt, one at the end of the last complete conversational
  turn, and lookback fillers every 15 content blocks, each only when the prefix
  clears the model's minimum cacheable size (`pkg/aiqg/promptcache/place.go:87`–`124`;
  engine added `2901a1a` and `cebbaeb`). Non-Anthropic models get none.
- `off` (aliases `none`, `disabled`) — strip every breakpoint.

An unrecognised value is logged and ignored, not rejected. Precedence is header,
then the gateway-wide `prompt_cache.default_mode` configuration value (added
`059b630`), then `passthrough` (`pkg/aiqg/promptcache/mode.go:97`–`115`). More
than four breakpoints are clamped to the earliest four, because a fifth is a
vendor `400` (`internal/server/prompt_cache.go:53`–`62`).

What survives to the vendor, since `7c9066e` (#197, committed but not deployed):
a `cache_control` on a message or on a tool definition in the OpenAI-dialect body
reaches Anthropic as an `ephemeral` breakpoint
(`internal/providers/anthropic/provider.go:521`–`528`, `:590`–`596`). Before that
commit only the system block's breakpoint was even attempted, and it serialized
to nothing. Three limits remain. A requested one-hour time-to-live cannot be
expressed with the pinned SDK and lands at the five-minute default
(`internal/providers/anthropic/provider.go:713`–`733`). On `/v1/messages`,
`cache_control` is dropped at the boundary: the inbound Anthropic wire types have
no field for it (`internal/server/anthropic_messages.go:52`–`100`), so use `auto`
there. And a request routed to OpenAI ignores breakpoints entirely, since OpenAI
caches automatically.

> [!UNVERIFIED] Per-content-part `cache_control` (inside a `content` array) is
> threaded only when the content decodes as `[]types.ContentPart`, but JSON
> decoding produces `[]interface{}` (the same mechanism that breaks vision,
> below), so a part-level breakpoint in an HTTP body is probably lost. Inferred
> from the types at `552d869`; not exercised with a request. Put breakpoints on
> the message instead.

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
| `tools` (request) | Forwarded (`internal/providers/openai/provider.go:581`) | Forwarded (`internal/providers/anthropic/provider.go:579`) |
| `tool_calls` (non-streaming response) | Forwarded (`internal/providers/openai/provider.go:633`) | Forwarded (`internal/providers/anthropic/provider.go:813`) |
| `tool_calls` (streaming response) | Forwarded (`internal/providers/openai/provider.go:690`) | **Lost** — the stream converter handles text deltas only (`internal/providers/anthropic/provider.go:271`) |
| `tool_choice` | Forwarded (`internal/providers/openai/provider.go:596`) | **Dropped** — no reference in the provider |
| legacy `functions` / `function_call` | Forwarded (`internal/providers/openai/provider.go:567`) | **Dropped** |
| `response_format: json_object` | Forwarded (`internal/providers/openai/provider.go:600`) | **Dropped** — the provider never reads `req.ResponseFormat`; it also declares `SupportsStructuredOutput()` false (`internal/providers/anthropic/provider.go:458`) |
| `response_format: json_schema` | **Type sent, schema not** (`internal/providers/openai/provider.go:606`) | **Dropped** |
| `temperature`, `top_p`, `stop` | Forwarded (`internal/providers/openai/provider.go:542`, `:553`) | Forwarded (`internal/providers/anthropic/provider.go:560`, `:564`, `:568`) |
| `seed`, `presence_penalty`, `frequency_penalty` | Forwarded (`internal/providers/openai/provider.go:556`–`564`) | **Dropped** (no vendor equivalent) |
| `max_tokens` | Forwarded (`internal/providers/openai/provider.go:550`) | Forwarded; **defaults to 1024 when unset** (`internal/providers/anthropic/provider.go:557`) |
| `cache_control` (message, tool) | Ignored — OpenAI caches automatically | Forwarded since `7c9066e`, committed but not deployed — see prompt caching above |
| `top_k` | Not representable | Not representable — absent from `ChatRequest` entirely |
| `n`, `logprobs`, `logit_bias`, `user`, `stream_options` | Not representable — absent from `ChatRequest` | Not representable |
| Image / vision content | See below | See below |

Three of those need more than a table cell.

**`json_schema` is the sharpest edge**, because it fails rather than degrades. The
OpenAI provider copies the `type` string but never assigns the schema itself —
the branch that would do it is a debug log
(`internal/providers/openai/provider.go:606`–`610`). A request asking for
`{"type":"json_schema"}` therefore reaches OpenAI declaring a schema-constrained
response and supplying no schema. Use `json_object` and validate the result
yourself, or constrain the output with tools instead.

**Vision does not work on any surface at present.** The two providers convert
message content with a Go type switch over `msg.Content`. The OpenAI provider's
switch has arms for `string` and `[]types.ContentPart` and no default
(`internal/providers/openai/provider.go:493`–`518`); the Anthropic provider's
default arm stringifies with `fmt.Sprintf("%v", content)`
(`internal/providers/anthropic/provider.go:702`–`709`), and its
`[]types.ContentPart` arm skips image parts with the comment "Skip image parts
for now" (`internal/providers/anthropic/provider.go:693`). The problem is that
`[]types.ContentPart` is not the type that arrives: `Content` is declared
`interface{}` (`internal/types/requests.go:43`), and content that has been
through JSON decoding arrives as `[]interface{}` of maps — which this repository
states in its own comment at `internal/server/server.go:2361`. `/v1/messages`
reaches the same decoder, because `handleMessages` re-marshals the translated
request and hands it to the shared OpenAI handler
(`internal/server/anthropic_messages.go:498`–`509`, decoded at
`internal/server/server.go:1065`). Send text-only requests until this is fixed.

**Do not take `/v1/capabilities` as the authority on vision.** Both providers
advertised `SupportsVision: true` in their capability matrix at `eee4b24`, and a
live probe on 2026-08-25 returned `"supports_vision":true` for every model. That
flag described the **vendor's** capability, not this gateway's translation
layer, which drops image parts on both paths as described above. Commit
`a9f160a` (#174) changed the provider-level flag to report the gateway's
effective capability: both providers now return `SupportsVision: false`
(`internal/providers/anthropic/provider.go:91`,
`internal/providers/openai/provider.go:89`). That fix is committed but not
deployed — the probe on 2026-09-21 still returned the provider-level
`"supports_vision":true` — and it does not reach the **per-model**
`supports_vision` entries inside `supported_models`, which are read from model
configuration (`configs/config.yaml:81`, `:90`, `:99`) and still say `true`. The
Anthropic entry also still lists `supported_image_formats`. Today neither flag
can be trusted for vision; after the next rollout the provider-level flag can,
and the per-model one still cannot.

> [!UNVERIFIED] The vision finding is a code-path reading, not an executed
> request — no valid token was available to send a multimodal body through. The
> types and the missing default arm are verified at `eee4b24` and unchanged at
> `552d869`; the end-to-end consequence (an image request losing its text as
> well, on the OpenAI path) is inferred from them. Confirm with a real request
> before filing or relying on it.

> [!UNVERIFIED] Streaming responses served by OpenAI appear never to carry token
> usage: the provider builds the upstream request without `StreamOptions`
> (`internal/providers/openai/provider.go:539`–`544`) and a search of the
> repository at `552d869` still finds no `StreamOptions` or `include_usage`
> anywhere, which is what OpenAI requires before it emits usage on a stream.
> Since `cddd372` the streaming path feeds `tokens_total` and `cost_total`
> whenever a stream reports usage, so this gap now decides whether an
> OpenAI-served stream is counted at all. Not confirmed against a live stream.

### Telemetry surfaces

**Short answer: on the running images, trust none of the `/metrics` series.
After the next deploy, the table below applies.** Both deployed images
(`aiqg-v5.86`, `aiqg-v5.75`) still serve the old hand-rolled exporter, whose
values are derived from the clock rather than from traffic (see design
rationale). Scraped from `gateway.aiqg.tas.scharber.com` on 2026-09-21, it
emits these families: `llm_router_requests_total`, `llm_router_tokens_total`,
`llm_router_cost_total`, `llm_router_errors_total`,
`llm_router_auth_attempts_total`, `llm_router_blocked_requests_total`,
`llm_router_provider_health`, `llm_router_active_connections`,
`llm_router_rate_limit_hits_total`, `llm_router_rate_limit_usage`,
`llm_router_security_score` (a constant `85`), `llm_router_threat_level`,
`llm_router_security_events_total`, `llm_router_validation_failures_total`,
`llm_router_input_sanitized_total`, `llm_router_audit_events_total`, and
`llm_router_active_api_keys`. Several names match the new table, but the values
behind them are not measurements, and there is no `request_duration_seconds`.
Everything below describes the code at `552d869`, including the
streaming-fed token and cost figures.

Two scrape endpoints, both `GET`, both unauthenticated, both served straight from
a registry through `promhttp` with no handwritten formatting in between:

| Endpoint | Registry | Registered at |
|---|---|---|
| `/metrics` | `internal/metrics.Registry` — router telemetry | `internal/server/server.go:982` |
| `/aiqg/metrics` | `pkg/aiqg/metrics.Registry` — AIQG event and token counters | `internal/server/server.go:986` |

Treat `/metrics` as an interface, not an implementation detail: dashboards and
alerts are callers of it, and the series below are what they may depend on. Every
name is prefixed `llm_router_`.

| Series | Type | Labels | Fed by |
|---|---|---|---|
| `requests_total` | counter | `provider`, `method`, `status_code` | `internal/metrics/middleware.go:74` |
| `request_duration_seconds` | histogram | `provider`, `method` | `internal/metrics/middleware.go:75` |
| `active_connections` | gauge | none | `internal/metrics/middleware.go:61` |
| `tokens_total` | counter | `provider`, `type` (`input`/`output`) | `internal/server/server.go:1772`, `internal/server/server.go:1995`; streaming at `internal/server/server.go:1873` |
| `cost_total` | counter | `provider`, `model` | `internal/server/server.go:1774`, `internal/server/server.go:1997`; streaming at `internal/server/server.go:1875` |
| `blocked_requests_total` | counter | `direction` (`inbound`/`outbound`) | `internal/server/enforcement.go:127` |
| `provider_health` | gauge | `provider` | `internal/server/server.go:403`, collected at scrape time |
| `errors_total` | counter | `provider`, `error_type` | `internal/server/server.go:1759`, `internal/server/server.go:1929` — `error_type` is always `completion_failed` |
| `auth_attempts_total` | counter | `result` | `internal/middleware/aiqg.go:206`, `:247`, `:1017`, `:1061`, `:1157` |
| `rate_limit_hits_total` | counter | `tier` | `internal/security/ratelimit.go:277` — only when the rate limiter is enabled. It is off in `configs/config.yaml:147`, and the `RATE_LIMIT_ENABLED` key in `k8s/configmap.yaml:39` is read by no Go code; the strict gateway sent no `X-RateLimit-*` headers on 2026-09-21 |
| `semcache_lookups_total` | counter | `outcome` | `internal/server/semcache_metrics.go:18`–`41` |
| `semcache_top_similarity` | histogram | `verdict` (`passed`/`rejected`) | same |
| `semcache_rejections_total` | counter | `reason` | same |
| `registry_sync_duration_seconds`, `registry_sync_total` | histogram, counter | `provider`; `provider`, `result` | registry sync engine — only with the registry enabled |
| `model_fallback_total`, `model_alias_resolution_total` | counter | `from`, `to`; `alias`, `resolved` | `internal/server/registry_admin.go:46`–`57` |
| `model_validation_total` | counter | `provider`, `model`, `result` | `internal/server/registry_admin.go:183` |
| `model_status` | gauge (always `1`) | `provider`, `model`, `status` | collected at scrape time, registered only with the registry enabled (`cmd/llm-router/main.go:214`) |

Everything from `errors_total` down changed or appeared after `eee4b24` and is
committed but not deployed: `1d89669` and `2f8994c` (#170, #175) wired the
three counters that had no call site, `8e641ca` (#219) added the semantic-cache
series, and `9e2653e` (#6) the registry series.

**`/aiqg/metrics`** is a separate registry defined in
`pkg/aiqg/metrics/metrics.go` (declared at `:27`, registered at `:387`), plus
the semantic-cache judge series in `internal/server/semjudge.go`. At `552d869`
it holds: event emission (`aiqg_events_emitted_total`,
`aiqg_emit_duration_seconds`, `aiqg_emitter_degraded`); traffic
(`aiqg_requests_total`, `aiqg_request_tier_total`, `aiqg_scan_findings_total`);
evaluation spend (`aiqg_shadow_replays_total`, `aiqg_shadow_tokens_total`,
`aiqg_shadow_truncated_total`, `aiqg_judge_calls_total`,
`aiqg_judge_tokens_total`, `aiqg_unbilled_spend_usd_total`,
`aiqg_unpriced_eval_calls_total`, `aiqg_judge_excluded_total`,
`aiqg_eval_credential_source_total`, `aiqg_eval_events_total`,
`aiqg_eval_events_failed_total`); prompt caching
(`aiqg_prompt_cache_requests_total`, `aiqg_prompt_cache_read_tokens_total`,
`aiqg_prompt_cache_creation_tokens_total`, `aiqg_prompt_cache_savings_usd_total`);
and `aiqg_semcache_judge_*`. None carries a tenant label; per-tenant figures
live in the AIQG event stream. The deployed image, scraped on 2026-09-21, exposed
only `aiqg_requests_total` and twelve `aiqg_semcache_judge_*` families, because
a counter appears only once something increments it.

Label value sets, so a query can be written without guessing. `provider` is a
provider name (`openai`, `anthropic` on the current fleet) or the literal `none`;
`method` is the HTTP method, in practice always `POST` because only the
completion routes are counted; `status_code` is the decimal status as a string;
`type` is `input` or `output` (`internal/metrics/metrics.go:303`, `:306`);
`direction` is `inbound` or `outbound` (`internal/metrics/metrics.go:136`);
`tier` has one producer, which always writes `default`. `result` on
`auth_attempts_total` is `success`, `malformed`, `unknown`, `suspended`, or
`missing` (`internal/metrics/metrics.go:121`–`126`); the fail-closed
`no_resolver_configured` rejection and a resolver outage increment none of them.
`outcome` on `semcache_lookups_total` is `semantic_hit`, `shadow_hit`,
`miss_rejected` (a candidate was found and thrown out), or `miss_no_candidate`
(nothing close was stored). `provider_health` is `1`
for healthy and `0` for anything else, with "healthy" meaning the router's own
health record reads exactly `healthy` (`internal/server/server.go:406`).
`request_duration_seconds` uses explicit buckets, not the library defaults —
0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120 seconds
(`internal/metrics/metrics.go:77`), stretched past a normal web service level
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
(`internal/server/server.go:934`–`947`). Every `GET` — `/v1/models`,
`/v1/models/{model}`, `/v1/providers`, `/v1/providers/{name}`, `/v1/capabilities`,
`/v1/health`, `/v1/health/{name}`, `/v1/breaker`, the three `GET
/v1/registry/*` routes, `/health`, and `/metrics` itself — is registered without
it (`internal/server/server.go:949`–`982`) and contributes nothing. `POST
/v1/routing/decision`, `POST /v1/registry/sync`, and `POST /v1/registry/validate`
are unwrapped too. A panel titled
"total gateway traffic" built on `requests_total` therefore measures completions
only, and silently excludes all read traffic. Excluding `/metrics` is deliberate:
a fifteen-second scrape would otherwise dominate the counter
(`internal/metrics/middleware.go:56`–`58`). Excluding the rest is a consequence
of that same decision, not a separate one.

**An unobserved counter emits no line at all.** `client_golang` writes a
`CounterVec` child only once something increments it, so a freshly restarted pod
serves a scrape with no `requests_total` in it — not a zero. Alerts must handle
the absent case (`absent()` or `or vector(0)`) rather than assuming a zero
baseline. Since `1d89669` (#170) a few counters are exempt because they are
seeded with explicit zeros at startup: every `auth_attempts_total` result,
`rate_limit_hits_total{tier="default"}`, `errors_total` for `openai` and
`anthropic`, and all four `semcache_lookups_total` outcomes
(`internal/metrics/metrics.go:236`–`256`). `requests_total`, `tokens_total`, and
`cost_total` are not seeded.

**The three counters that used to have no producer now have one — in the
committed code.** At `eee4b24`, `errors_total`, `auth_attempts_total`, and
`rate_limit_hits_total` were declared and registered
(`internal/metrics/metrics.go:225`–`227`) with no call site anywhere. At
`552d869` each is incremented at the sites in the table above. Two limits
remain: `errors_total` counts only a completion that failed after routing, not
refusals such as a `402` or `422`; and `rate_limit_hits_total` stays at zero
while the rate limiter is off. On the deployed images none of this applies —
they serve the old exporter.

Those combine into four distinct ways a query returns no rows, worth
distinguishing before you conclude the gateway is idle: the series is fed only
by a feature that is off (the registry counters and histograms), or registered
only when that feature is on (`model_status`); the series
has a producer but nothing has incremented it on this pod yet; you are querying
`GET` traffic that is never counted; or —
the fourth, and the least visible — `provider_health` failed to register.
`RegisterProviderHealth` is called once per `NewServer` and a duplicate
registration is swallowed as `prometheus.AlreadyRegisteredError`
(`internal/server/server.go:410`). That tolerance is what lets a second
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
> or later before treating the labels as deployed. Re-checked 2026-09-21: still
> `aiqg-v5.86`, still the old exporter (`llm_router_security_score{service="llm-router"} 85`).

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
(`internal/routing/router.go:541`–`545`, `:560`–`571`). That path resolves the
model to its provider and routes there, and if that provider is unhealthy it
**errors rather than substituting another vendor**
(`internal/routing/router.go:630`–`641`) — you get `503 Routing failed`, never a
silent cross-vendor swap. Concrete catalogue names behave this way:
`claude-sonnet-4-6` is advertised only by Anthropic, `gpt-4o` only by OpenAI.

The hint case is when the name is *not* unambiguous — advertised by no registered
provider, or by more than one. `isSpecificProviderRequested` counts matches and
requires exactly one (`internal/routing/router.go:570`), so zero matches falls
through to cost-optimized routing, which selects the cheapest healthy provider
and **ignores `req.Model` entirely** for that choice
(`internal/routing/router.go:664`–`675`). A misspelled or unknown model name does
not error — it silently becomes "whatever is cheapest".

**With the model registry enabled, the name you sent may not be the name that
runs** (committed, not yet deployed; off by default). `resolveViaRegistry` runs
before `determineStrategy`, so everything above applies to the *rewritten* name
(`internal/routing/router.go:95`–`142`). It does three things and only three:

- An **alias** — a name in the operator's `registry.aliases` map, applied after
  each sync (`internal/registry/sync.go:212`–`227`) — becomes its target model.
- A known model whose cached status is **`unavailable`** is replaced by a
  fallback from the same provider: its configured `replacement_model` if that is
  active, otherwise **the alphabetically first active model** that provider
  offers (`internal/registry/registry.go:219`–`244`). That can be a larger and
  pricier model than the one you asked for. The `registry.fallback.enabled` and
  `registry.fallback.strategy` settings are carried into the sync engine's
  configuration but read by nothing (`internal/registry/sync.go:22`–`25`), so a
  fallback happens whenever the registry is on.
- A **`deprecated`** model is served unchanged; only a warning is logged.

A model the registry has never discovered passes through untouched. When the
name changes, the routing metadata records it: `original_model` and
`resolved_model` on an alias, plus `fallback_used: true` and
`fallback_reason: "model_unavailable"` on a fallback (`internal/types/responses.go:114`–`121`,
stamped at `internal/routing/router.go:290`–`298`). Those fields travel in the
`router_metadata` object of an OpenAI-dialect JSON body; there is no response
header for them. What you can see on every surface is `X-TAS-Router-Model`,
which carries the model that actually ran, and `X-TAS-Router-Fallback-Used:
true` — the **same** header a fallback-chain hop sets, so on its own it does not
tell you which of the two happened (`internal/server/server.go:1814`–`1823`).
To detect a substitution portably, compare `X-TAS-Router-Model` with the model
you sent. That header is the only per-response signal on every surface, and it
is also on the unstable list under compatibility guarantees, so treat a missing
header as "unknown" rather than as "unchanged", and do not fail a request over
it. If you need a guarantee instead of a signal, send a concrete model name and
ask the operators whether the registry is enabled — with it off, which is the
default, the registry rewrites nothing, leaving only the fallback-chain
substitution described next.

Two things can still move a pinned request after selection. A resolved route rule
may pin a provider itself, and that pin is preferred over the strategy though it
is not absolute — an unusable pin falls through to normal selection and is
recorded as not honoured (`internal/routing/router.go:257`–`263`). "Unusable"
has four causes: tenant constraints deny the provider, it is not configured, it
is unhealthy, or — added by `6d57096` (#151), committed but not deployed — its
configured model list is non-empty and does not include the requested model
(`internal/routing/router.go:1272`–`1293`). Before that fourth check, a pin to a
vendor that could not serve the model went through anyway and surfaced as an
opaque `500`. A set-aside pin enters the tenant's fallback chain at tier 1 if
one is configured, otherwise normal strategy selection, and the reason is
appended to the routing decision (`internal/routing/router.go:1382`–`1394`). And the
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

**Evaluation calls about your traffic can bill your key.** The gateway makes
calls on its own initiative: an LLM judge that scores a sample of responses, and
shadow replays that re-run a sampled request through an experiment's other
variants to compare them. Until `8425da4` (#214) those always used the gateway's
configured vendor key. In the committed code, not yet deployed, the key is chosen
per evaluation, after routing picks the vendor
(`internal/server/eval_credentials.go:27`–`54`):

| Your credential state for that vendor | Evaluation call |
|---|---|
| A stored BYOK key | **Billed to your key** |
| No stored key, shared-key fallback allowed | Billed to the TAS shared key |
| No stored key, BYOK-only | **Skipped** — not made at all, and counted in `aiqg_judge_excluded_total{reason="byok_only_no_key"}` on `/aiqg/metrics` |
| Credential lookup failed | Billed to the configured key |

Shadow replays run at roughly twice the cost of the request they shadow, and
since `333365e` (#213) the rate is declared per experiment as the guardrail
`shadow_eval_pct` (0–100) beside `max_traffic_pct`: absent inherits the
gateway-wide default, and an explicit `0` refuses
(`pkg/aiqg/experiments/experiments.go:63`–`81`). If you run experiments and hold
a BYOK key, that field is your control over evaluation spend. Each evaluation
also emits its own AIQG event since `d06d2eb` (#212), attributed to your tenant,
marked synthetic so it is excluded from usage and quality aggregates, recorded
against the path `/internal/aiqg/eval` — not a route anyone can call — and
linked to the response it scored through `parent_step_id`
(`internal/server/eval_attribution.go:70`–`164`). That is how evaluation spend on
your key can be reconciled against your own traffic in the dashboard backend.

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
headers, which are a TAS convenience and not part of either vendor dialect —
and the same goes for `X-TAS-Stream-Fallback` and the registry fields
`original_model`, `resolved_model`, and `fallback_reason`, all added in this
range with no stability statement; the
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

**Two envelopes, chosen by surface.** Every gateway-written error outside the
authentication layer uses one of two shapes. The standard one is
`{"error":{"message":"…","type":"api_error","code":<status as a number>},"timestamp":<unix seconds>}`
(`internal/server/server.go:2927`–`2941`). On `/v1/messages` the shared pipeline
switches to Anthropic's `{"type":"error","error":{"type":"<type>","message":"…"}}`,
with the type mapped from the status — `400` → `invalid_request_error`, `401` →
`authentication_error`, `402`/`403` → `permission_error`, `404` →
`not_found_error`, `413` → `request_too_large`, `429` → `rate_limit_error`, any
other `5xx` → `api_error` (`internal/server/anthropic_messages.go:562`–`593`).
The authentication layer writes its own bodies, shown in its table below. The
standard envelope was observed live on 2026-09-21 through the `415` below,
which uses the same writer.

**Checks that run before authentication**, on every route and every surface,
always in the standard envelope — so a stock Anthropic SDK receives a
non-Anthropic body for these:

| Condition | Status | Body | Retryable | Caller action |
|---|---|---|---|---|
| `POST`/`PUT` whose `Content-Type` is not exactly `application/json` (a `; charset=…` suffix counts as different) | `415` | `{"error":{"code":415,"message":"Content-Type must be application/json","type":"api_error"},"timestamp":…}` — observed 2026-09-21 (`internal/server/server.go:1047`–`1058`) | No | Send the bare value |
| Security validator: `Content-Type` other than `application/json`/`text/plain`, body over 10 MiB, or a method outside the configured list | `400` | `{"error":{"code":400,"details":["Content-Type application/xml not allowed"],"message":"Request validation failed","type":"validation_error"},"timestamp":…}` — observed 2026-09-21; `details` lists every rule that failed (`internal/security/validation.go:92`–`111`, `:235`–`250`) | No | Fix the named rule |
| Rate limit exceeded — **only if the rate limiter is enabled, which it is not** (see telemetry) | `429` + `Retry-After` | `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":429,"retry_after":N},"timestamp":…}` (`internal/security/ratelimit.go:276`–`293`) | Yes, after the header | Back off |

Auth is resolved **before** body validation, so a bad token masks every body
error until it is fixed. Rows 1–3 and their `/v1/messages` variants were
observed live on 2026-09-21; the rest are read from
`internal/middleware/aiqg.go`. Every `401` also carries the header
`WWW-Authenticate: TAS realm="aiqg"`. On `/v1/messages` each row renders in
Anthropic's envelope instead, as noted — error rendering follows the surface.

| Condition | Status | Body | Retryable | Caller action |
|---|---|---|---|---|
| No gateway token in `TAS-Auth`, `Authorization`, or `x-api-key` (strict host) | `401` | `{"error":{"code":"path_a_auth_required","message":"AIQG ingress requires both TAS-Auth and Authorization headers; TAS-Auth is missing","missing_header":"TAS-Auth","docs":"https://docs.tas.scharber.com/aiqg/auth"}}`. On `/v1/messages`: `{"type":"error","error":{"type":"authentication_error","message":"AIQG ingress requires authentication; TAS-Auth is missing"}}` (`internal/middleware/aiqg.go:1156`–`1176`). Ignore "both … headers": `Authorization` has been optional since BYOK (`internal/middleware/aiqg.go:183`–`190`) | No | Supply a token |
| Token missing the `tas_qg_live_` prefix | **`400`** | `{"error":{"code":"aiqg_header_invalid","message":"aiqg: TAS-Auth token is malformed"}}`; Anthropic `invalid_request_error` on `/v1/messages` | No | Fix the token format — the shape check runs before the lookup (`internal/middleware/aiqg_headers.go:141`) |
| Well-formed but unrecognized token | `401` | `{"error":{"code":"path_a_auth_required","message":"AIQG ingress requires a recognized TAS-Auth token","reason":"token_unknown","docs":"https://docs.tas.scharber.com/aiqg/auth"}}`. On `/v1/messages`: `{"type":"error","error":{"type":"authentication_error","message":"AIQG ingress requires a recognized TAS-Auth token"}}` (`internal/middleware/aiqg.go:1016`–`1033`) | No | Use a provisioned token |
| Token resolves to a **suspended** account | `403` | `{"error":{"code":"account_suspended","message":"AIQG account is currently suspended; contact support"}}` — Anthropic-shaped `permission_error` on `/v1/messages` (`internal/middleware/aiqg.go:1073`–`1079`) | No | Contact support. The token is genuine, so this is not a credential problem; retrying or reissuing will not clear it (`internal/middleware/aiqg.go:224`–`225`) |
| Both `TAS-Policy` and `TAS-Policy-Bundle` sent | `400` | `{"error":{"code":"aiqg_header_invalid","message":"aiqg: TAS-Policy and TAS-Policy-Bundle are mutually exclusive"}}`; Anthropic `invalid_request_error` on `/v1/messages` (`internal/middleware/aiqg_headers.go:147`, written at `internal/middleware/aiqg.go:1182`–`1198`) | No | Send one |
| `TAS-Source-App` longer than 128 characters | `400` | same shape, `"message":"aiqg: TAS-Source-App exceeds 128 characters"` (`internal/middleware/aiqg_headers.go:136`, `:149`) | No | Shorten it |
| The token resolver's backend is unreachable | `503` | `{"error":{"code":"token_resolver_unavailable","message":"AIQG token resolver is temporarily unavailable; retry"}}`; Anthropic `api_error` on `/v1/messages` (`internal/middleware/aiqg.go:1087`–`1095`) | **Yes**, with backoff — nothing was attempted | Retry |
| Strict ingress with no token resolver at all — **committed, not yet deployed** (#173) | `401` | `{"error":{"code":"path_a_auth_required","message":"AIQG ingress is not accepting tokens (no token resolver configured)","reason":"no_resolver_configured","docs":"https://docs.tas.scharber.com/aiqg/auth"}}`; Anthropic `authentication_error` with the same message on `/v1/messages` (`internal/middleware/aiqg.go:1042`–`1058`) | No — an operator misconfiguration | Report it; your token is not the problem |

*History:* until the 2026-09-21 refresh this table gave the unknown-token body
(`"reason":"token_unknown"`) for the missing-token row too, and a separate
"Same, on `/v1/messages`" row. Neither matched the code at `eee4b24` or the
live gateway; both rows are corrected above from code and live probes.
`missing_header` is present only on the missing-token `401`, `reason` only on the
unknown-token one — branch on `code` and those two fields, never on `message`.

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
| `400` | `internal/server/server.go:1066` | `Invalid JSON: …` — the OpenAI-dialect body did not decode | No | Never unchanged. Fix the body |
| `403` | `internal/server/server.go:1240` | `request blocked by content policy` — the gatekeeper's inbound scan blocked the prompt. Distinct from the policy-enforcement `422` below | No — refused before the vendor call | Never. The identical request is blocked again |
| `500` | `internal/server/server.go:1234` | `content scan failed` — the inbound scanner errored and the gatekeeper is configured to fail closed | No | Yes, with backoff |
| `402` | `internal/server/server.go:1472` | `provider_key_required: no stored <vendor> credential for this account and shared-key fallback is disabled` | No — refused before any vendor call | Never. Store a credential or enable shared fallback; the identical request fails identically |
| `422` | `internal/server/enforcement.go:134` | **Blocked by policy** — body `blocked by policy: <pattern names>` | No — refused before the vendor call | Never. The identical request is blocked again |
| `503` | `internal/server/server.go:1375` | `Routing failed: …` — the router could not **select** a provider | No — this is selection failing, before any attempt | Yes, with backoff. Nothing was spent |
| `500` | `internal/server/server.go:1760`, `internal/server/server.go:1930` | `Completion failed: …` — the attempt, **including every fallback hop**, failed | **Yes, potentially several times** | Only deliberately. Each prior attempt that reached a vendor may already be billed |
| `500` | `internal/server/server.go:2462` | `Streaming failed: …` — a `"stream": true` request could not open a stream with the chosen provider or any `fallback_config` provider, before any byte was sent | Possibly, if a vendor accepted and then failed | Only deliberately |
| `403` | `internal/server/server.go:1961` | `response blocked by content policy` — the vendor **answered** and the outbound scan blocked the answer (non-streaming only) | **Yes** — the vendor call completed | Rarely useful: the same prompt tends to produce a blocked answer again |
| `500` | `internal/server/server.go:1949` | `response content scan failed` — the vendor answered, the outbound scanner errored, and the gatekeeper fails closed | **Yes** | Yes, knowing it bills again |
**Not emitted at `552d869`: `413` and `429` on the completion routes.** Earlier
versions of this table listed `413 Request entity too large` and `429 Rate
limited`, citing `internal/server/anthropic_messages.go:581` and `:583`. Those
lines are the status-to-type table the Anthropic error renderer uses, not places
that send a status, and no completion path writes either code: a vendor `413` or
`429` arrives as `500 Completion failed` with the vendor's status inside the
message (see below). An oversized body above 10 MiB gets the security
validator's `400`, not a `413`. The only `429`s in the service are the registry
sync limiter and the disabled rate limiter. Handle `413`/`429` defensively if
your client already does, but do not expect them.

Surface-specific errors, from each handler before it joins the shared pipeline:

| Surface | Status | Message | Envelope | Source |
|---|---|---|---|---|
| `/v1/messages` | `400` | `could not read request body`, or the translation error (for example a missing `model` or `max_tokens`) | Anthropic `invalid_request_error` | `internal/server/anthropic_messages.go:488`–`497` |
| `/v1/messages` | `500` | `internal translation error` | Anthropic `api_error` | `internal/server/anthropic_messages.go:498`–`501` |
| `/v1/responses` | `400` | `could not read request body`, or the translation error | standard | `internal/server/responses_api.go:323`–`329` |
| `/v1/responses` | `500` | `internal translation error` | standard | `internal/server/responses_api.go:333` |
| `/v1/embeddings` | `400` | `Invalid JSON: …`, `field 'model' is required`, `field 'input' is required` | standard | `internal/server/embeddings.go:34`–`43` |
| `/v1/embeddings` | `501` | `no configured provider supports embeddings` | standard | `internal/server/embeddings.go:48` |
| `/v1/embeddings` | `502` | `Embeddings failed: …` — the vendor call failed | standard | `internal/server/embeddings.go:65` |
| `/v1/messages/count_tokens` | `400` | `could not read request body`, or the parse error | Anthropic `invalid_request_error` | `internal/server/count_tokens.go:34`–`40` |
| `/v1/messages/count_tokens` | `501` | `no configured provider supports token counting` | Anthropic `api_error` | `internal/server/count_tokens.go:45` |
| `/v1/messages/count_tokens` | `502` | `count_tokens failed: …` | Anthropic `api_error` | `internal/server/count_tokens.go:60` |

Retry the `502`s with backoff; the `400`s and `501`s fail identically every time.

**Errors from path matching and the read routes**, all observed on 2026-09-21
unless a source is given:

| Condition | Status | Body |
|---|---|---|
| Any path not in the public allowlist, on `gateway.air-ops.net` (including every `GET` route) | `404` | nginx's default web page (not JSON), `<title>404 Not Found</title>` — the request never reaches the router (`k8s/ingress-gateway-airops.yaml:83`–`133`) |
| A path or method the router does not register, on the internal hosts — for example `GET /v1/chat/completions`, or `/v1/registry/*` on today's images | `404` | plain text `404 page not found` (the router library's default; there is no `405`) |
| `GET /v1/models/{model}` for a model no provider lists | `404` | `{"error":{"code":404,"message":"Model nope-model not found","type":"api_error"},"timestamp":…}` — also when the Anthropic SDK asks for a non-Anthropic model, since that listing holds Anthropic models only (`internal/server/server.go:2783`–`2808`) |
| `GET /v1/providers/{name}` or `/v1/health/{name}` for an unknown provider | `404` | `Provider <name> not found`, standard envelope (`internal/server/server.go:2817`, `:2873`) |
| `GET /v1/breaker` when the breaker store cannot be read | `500` | `breaker status unavailable: …`, standard envelope (`internal/server/breaker_status.go:63`) |

None of these is retryable except the breaker `500`.

**Every row above assumes a status was sent at all.** Streaming responses commit
to `200` before the first chunk, so none of this table applies to them: a vendor
that dies mid-stream produces no status, no error body, and a normal stream
terminator. The only signal available to a streaming client is the
`finish_reason` (OpenAI) or `stop_reason` (Anthropic) on the final chunk —
treat absent or empty as a failure. See "Streaming has no error channel" below
for why, and note that this gap is tracked as issue #172 rather than settled.


**Mid-stream failure, by version:**

| Version | What the client receives after a vendor dies mid-stream |
|---|---|
| **Deployed images** (`aiqg-v5.86`, `aiqg-v5.75`; they predate `76fa529`) | Status `200` already sent; the stream stops and ends with the normal terminator — `data: [DONE]` on the OpenAI surfaces, `content_block_stop`/`message_stop` on `/v1/messages`. No error text anywhere. The only signal is a missing or empty `finish_reason`/`stop_reason` |
| **Committed at `552d869`** (#172) | Status `200`; a final error event in the surface's dialect, with no normal completion event after it on `/v1/messages` and `/v1/responses`. Shapes are under "Streaming has no error channel" below |

Code written against both — check for an error event *and* for a missing
`finish_reason`/`stop_reason` — works before and after the rollout.

**`503` and `500` are the pair people get backwards, and the doc used to as
well.** `503 Routing failed` comes from `s.router.Route()` returning an error
(`internal/server/server.go:1375`) — the router could not pick a provider at all,
so no vendor was contacted and nothing was spent. Chain exhaustion is the *other*
one: `attemptCompletionWithRetryAndFallback` returning an error surfaces as
`500 Completion failed` (`internal/server/server.go:1930`), logged internally as
`All completion attempts failed`. By definition several vendor calls may have
been made and billed before you saw it. Retrying a `503` is cheap; retrying a
`500` re-runs the whole chain and pays for it again. How many calls sit behind
a `500` on the live path depends on the note under "How it works end to end":
one, unless your request body carried `retry_config` or `fallback_config`.

The other `503` in the codebase (`internal/server/server.go:2917`) belongs to
`POST /v1/routing/decision`, a dry-run endpoint that returns the routing decision
without completing anything. It is not a completion error and never bills. Its
body is decoded as a completion request, so a malformed one gets `400 Invalid
JSON: …` (`internal/server/server.go:2904`). The registry admin routes add two
more `503`s and their own errors, listed in their section.

**No `Retry-After` header is emitted on any of these.** The only `Retry-After`
in the service is written by the internal rate limiter
(`internal/security/ratelimit.go:278`), which is a separate middleware from the
gateway path documented here. Use your own bounded exponential backoff; do not
wait for a header that will not arrive. Since `e4548af` the word "only" above is
no longer literally true: `POST /v1/registry/sync` also writes `Retry-After`
(`internal/server/registry_admin.go:101`). It is an operator route, so for
completion traffic the advice stands.

**The retry decision, in one place.** Every status in this section falls into
one of three groups.

- **Retry with backoff; nothing was spent:** `503 token_resolver_unavailable`,
  `503 Routing failed`, `500 content scan failed`, `502` from `/v1/embeddings`
  or `/v1/messages/count_tokens`, `500 failed to list models` and `429` (after
  `Retry-After`) on the registry routes.
- **Retry only deliberately; a vendor may already have billed:** `500
  Completion failed`, `500 Streaming failed`, `500 response content scan
  failed`, `403 response blocked by content policy`, and a streamed response
  that ended in an error event.
- **Never retry unchanged; the same request fails the same way:** every `400`,
  `401`, `402`, `404`, `415`, `422`, and `501`, `403 account_suspended`, `403
  request blocked by content policy`, and `503 model registry is not enabled`.

**What the gateway does with a `429` is determinable, and it does count it.**
Both classifiers treat rate limiting as a real attempt: `ClassifyError` returns
the non-ejecting `RateLimited` outcome, so it does not count against the
provider's health (`pkg/aiqg/breaker/breaker.go:391`), while `ClassifyFailure`
returns `FailureRateLimited` with `eligible = true`
(`pkg/aiqg/breaker/breaker.go:549`), which makes it fallback-eligible. If the
tenant's `fallback.on` includes it, the chain advances, `AttemptCount` increments
(`internal/server/fallback.go:156`), and a second vendor is called and billed. So
a single `429` you observe may sit behind more than one upstream attempt. (This
is the chain walk that the note under "How it works end to end" could not trace
to a route; on the live path a vendor `429` becomes a `500` after one attempt
unless your body asked for retries or fallback.)

> [!UNVERIFIED] Whether the *vendor* meters a rate-limited or oversized request
> as billable is the vendor's policy; no code in this gateway records it either
> way. The gateway's own accounting is described above — this marker covers only
> the vendor's side, which you should confirm against your vendor contract.

**Every 5xx from the attempt chain arrives as `500`, whatever the vendor said.**
This is a verified source reading, not an assumption: `handleNonStreamingCompletion`
and its retrying variant both render any chain failure as
`500 Completion failed: <wrapped error>` (`internal/server/server.go:1760`,
`internal/server/server.go:1930`), and no code path maps an upstream `502` or
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
> Update 2026-09-21: the envelopes are now read from the two writers named at
> the top of this section, and the standard one was observed live through the
> `415`; what remains unexercised is each post-authentication message string.

### Streaming has no error channel

Every status above assumes the response headers have not been sent yet. Once a
stream starts they cannot be used, and the streaming path has no substitute.

This subsection describes the **deployed** images. At `eee4b24`,
`handleStreamingCompletion` wrote `200` and the server-sent events (SSE) headers
before it read the first chunk, then ranged over the provider's chunk channel and
called `done()` when the channel closed. The chunk type carried no error field
and the encoder interface had exactly two methods, `writeChunk` and `done`.
There was nowhere for a mid-stream failure to go. A vendor that dies halfway
through closes the channel, and the stream terminates with the ordinary
terminator — `data: [DONE]` on the OpenAI surfaces
(`internal/server/anthropic_messages.go:655`) or the normal
`content_block_stop`/`message_stop` sequence on `/v1/messages`
(`internal/server/anthropic_messages.go:844`).

**A truncated stream is therefore byte-indistinguishable from a complete one at
the protocol level.** Your client must decide from the payload: check
`finish_reason` (OpenAI) or `stop_reason` (Anthropic) on the final chunk and
treat absent or empty as a failure, rather than trusting that `[DONE]` arrived.

**Committed, not yet deployed: the stream now carries a terminal error event**
(`76fa529`, #172). The status is still `200` — the live streaming path writes it
before the first chunk (`internal/server/server.go:2483`) — but a provider that
fails mid-stream now sends one last chunk marked as an error instead of closing
silently (`internal/types/responses.go:64`–`83`; OpenAI at
`internal/providers/openai/provider.go:182`–`189`, Anthropic at
`internal/providers/anthropic/provider.go:172`–`176` and `:207`–`211`).
`streamChunks` turns that chunk into the dialect's error event, records
`finish_reason` as `error` on the AIQG event, and does **not** send the normal
terminator (`internal/server/server.go:1838`–`1865`). The encoder interface
gained a third method, `writeError`, for this
(`internal/server/anthropic_messages.go:600`–`606`). What arrives on the wire:

| Surface | Terminal error |
|---|---|
| `/v1/chat/completions`, `/v1/completions` | `data: {"error":{"message":"…","type":"upstream_stream_error","code":"","param":null}}` then `data: [DONE]` (`internal/server/anthropic_messages.go:665`–`679`) |
| `/v1/messages` | `event: error` with `{"type":"error","error":{"type":"upstream_stream_error","message":"…"}}`, and no `message_stop` (`internal/server/anthropic_messages.go:890`–`899`) |
| `/v1/responses` | `event: error` with `{"type":"error","code":"","message":"…","param":null}`, and no `response.completed` (`internal/server/responses_api.go:548`–`553`) |

On the OpenAI surfaces `[DONE]` still follows the error object, so keep
checking for an `error` key in every `data:` frame rather than treating `[DONE]`
as success. `TestStreamEncoders_WriteError`
(`internal/server/anthropic_messages_test.go:316`) pins these shapes. Two gaps
remain: a vendor connection that ends with a clean end-of-file before the
message is complete still looks like success, and a stream the client has
already abandoned gets no error frame, since the provider gives up when the
request context is cancelled. Keep the `finish_reason`/`stop_reason` check.

### Client disconnect and transport failures

If your client times out locally or the connection drops, you never see a status
at all — but the request does not stop there, and the outcome is worth
understanding before you set an aggressive client timeout.

The request context flows unchanged from the HTTP handler down into the vendor
call: `completeWithFallback` takes `ctx := r.Context()`
(`internal/server/fallback.go:50`) and hands it to `attempt`, which passes it
straight to `provider.ChatCompletion(ctx, req)`
(`internal/server/fallback.go:194`); the retrying path that live requests
actually take does the same, passing `r.Context()` down to the same call
(`internal/server/server.go:1926`, `:2562`). Go's HTTP server cancels that context when
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
> model, measure it rather than trusting this paragraph. Since 2026-09-21 there
> is a second reason to measure: the tier walk itself may not run on live
> requests (see "How it works end to end"). On the path that does run, a
> caller-supplied `retry_config` stops at its next backoff wait once the context
> is cancelled (`internal/server/server.go:2553`–`2558`).

One more streaming surprise: if the provider does not support streaming at all,
the request silently becomes non-streaming. `StreamCompletion` returning an error
falls through to `handleNonStreamingCompletion`
(`internal/server/server.go:1883`–`1891`), so a request that set `"stream": true`
can come back as a single ordinary JSON body with no SSE framing. A client that
assumes SSE because it asked for SSE will fail to parse a perfectly successful
response. Branch on the response `Content-Type`, not on what you requested.
Since `76fa529` that branch also sets the response header
`X-TAS-Stream-Fallback: true` (`internal/server/server.go:1888`).

> [!UNVERIFIED] The fallback in the paragraph above sits in
> `handleStreamingCompletion`, which no route calls (see the note under "How it
> works end to end"). On the live dispatch, a stream that cannot be opened goes
> through `attemptStreamingWithFallback` and ends as `500 Streaming failed: …`
> rather than as a JSON body (`internal/server/server.go:2459`–`2463`,
> `:2513`–`2529`). New finding, 2026-09-21; not exercised live. Branching on
> `Content-Type` stays correct either way.

## Extension points

**Adding a provider** — implement the provider interface under
`internal/providers/`. Routing and the fallback chain operate on the interface,
so a new provider participates without touching the surfaces. The interface is
`providers.LLMProvider` (`internal/providers/interfaces.go:10`–`17`), six
methods: `GetCapabilities()`, `GetProviderName()`, `ChatCompletion(ctx, req)`,
`StreamCompletion(ctx, req)` returning a chunk channel, `EstimateCost(req)`, and
`HealthCheck(ctx)`. Optional capabilities are separate interfaces the server
type-asserts for — `EmbeddingProvider` for `/v1/embeddings`, `TokenCounter` for
`count_tokens`, plus `FunctionCallingProvider`, `VisionProvider`,
`StructuredOutputProvider`, `BatchProvider`, and `AssistantProvider`
(`internal/providers/interfaces.go:20`–`63`). Providers are not discovered:
each is constructed and passed to `router.RegisterProvider` by name in
`cmd/llm-router/main.go:345`–`357`, so a new one needs a line there and a config
block. Three obligations
arrived with this range. `StreamCompletion` must send a final chunk with `Error`
set when the upstream stream breaks, rather than closing the channel, or your
provider reintroduces the silent truncation #172 removed — copy the `select` on
`ctx.Done()` the existing providers use (`internal/providers/openai/provider.go:182`–`189`).
Report **effective** capabilities, what a request through this gateway can
actually do, not what the vendor advertises (#174). And fill in
`SupportedModels`: a pinned route now checks it, and an empty list is read as
"cannot tell" and lets any pin through (`internal/routing/router.go:586`–`605`).

**Making a provider visible to the model registry** — build an adapter
satisfying `ProviderAdapter` (`internal/registry/adapters/interface.go:23`),
usually over a small lister or prober interface your provider implements, as
`OpenAIModelLister` (`ListModelIDs`) and `AnthropicModelProber` (`ProbeModel`)
do (`internal/registry/adapters/interface.go:39`, `:50`). Register it in
`setupRegistry` (`cmd/llm-router/main.go:161`–`227`). A probe must return
`(false, nil)` only for a definitive "no such model" and an error for anything
transient, because a transient failure reported as "unavailable" would trigger
model substitution for real traffic (`internal/providers/anthropic/provider.go:353`–`391`).
The router depends only on the four-method `ModelRegistry` interface
(`internal/routing/router.go:63`–`73`), so an alternative registry can be
substituted there without importing `internal/registry`.

**Adding a wire surface** — translate at the boundary and converge on the shared
pipeline, as `internal/server/anthropic_messages.go` and
`internal/server/responses_api.go` both do. Register in the same block at
`internal/server/server.go:934`, wrapped in `wrapAIQG`; that wrapper is what
gives the new surface both governance and telemetry, so a route registered
outside it is silently uncounted. A new streaming dialect implements
`streamEncoder` — now three methods, including `writeError`, which must be
terminal (`internal/server/anthropic_messages.go:600`–`606`) — and is driven by
the shared `streamChunks` loop, not a copy of it.

**Adding a metric** has one rule that matters more than the mechanics: declare it
in `internal/metrics/metrics.go` and add it to the `MustRegister` call at
`internal/metrics/metrics.go:219` **in the same change that adds its call site**.
A series with no call site is exactly the shape of the bug this package replaced,
and three of them existed here until #170 and #175 wired them — the point of the
registry being one enumerable file is that a reviewer can see the gap. If the
label values are known and bounded, add them to `seed()` as well
(`internal/metrics/metrics.go:242`), so the series reads zero from pod start
rather than absent. For anything derived from
state that already lives somewhere else, register a collector that reads it at
scrape time instead of mirroring it into a gauge; `RegisterProviderHealth`
(`internal/metrics/metrics.go:288`) is the worked example, and its comment
explains why a mirrored gauge is the same failure arrived at honestly.
`RegisterModelStatus` (`internal/metrics/metrics.go:438`) is the second.

Two mechanical constraints on that path. `RegisterProviderHealth` returns rather
than panics on a duplicate registration, and the caller tolerates
`prometheus.AlreadyRegisteredError` specifically so a second `NewServer` in one
process — which the tests do — is not fatal (`internal/server/server.go:410`).
If you add another scrape-time collector, match that handling or the second
server construction fails. And anything wrapping a `http.ResponseWriter` in the
request path must forward `Flush`, as `statusRecorder` does
(`internal/metrics/middleware.go:39`): the streaming handlers type-assert to
`http.Flusher`, and a wrapper that does not implement it breaks server-sent
events without failing anything.

**The other things you might want to change, and where each lives:**

| Behaviour | Extension point? | Where |
|---|---|---|
| Routing strategies | **Code change, closed set.** Four constants — `cost_optimized`, `performance`, `round_robin`, `specific` (`internal/routing/router.go:145`–`152`) — dispatched by a `switch` (`:608`–`627`). A new one means a new constant, a new `route…` function, and a case. Per request, a caller chooses only through `optimize_for`; `determineStrategy` never picks `round_robin` itself (`:541`–`556`) | `internal/routing/router.go` |
| Per-tenant strategy, provider pin, limits, chain | **Data, not code.** Route rules are resolved per request from `aiqg-dashboard-be` (`pkg/aiqg/policy/policy.go:141`–`159`) and applied by `routeBySelection` (`internal/routing/selection.go:81`) and `routeWithPin`. Change them in the dashboard, not here | `aiqg-dashboard-be` |
| Policy patterns and bundles | **Data, outside this repository.** Which patterns a tenant enables, and the action for each, arrive in the resolved bundle from the same dashboard endpoint; the gateway only reads them (`internal/middleware/aiqg.go:1231`–`1241`) | `aiqg-dashboard-be` |
| Scanner rules (what counts as a finding) | **Outside this repository.** Scanning is the `Gatekeeper` module, imported as `github.com/Tributary-ai-services/Gatekeeper/pkg/scan` (`internal/server/enforcement.go:13`) and resolved to the sibling checkout by a `replace` directive (`go.mod:23`). New detectors are added there | `Gatekeeper` repository |
| Enforcement actions | **Closed.** `decideEnforcement` knows exactly `log` (and ungoverned — ignored), `block`, and any other value treated as redact; the strongest wins (`internal/server/enforcement.go:64`–`93`). A new action means changing that function and the outcome types in `go-aiqg-resilience` | `internal/server/enforcement.go` |

**Deliberately closed:** raw passthrough. There is no route that forwards a
request to a vendor unparsed, and adding one would bypass scanning, enforcement,
spend attribution, and routing at once. If you find yourself wanting it, the
requirement is usually "vendor feature X is unsupported" — extend the translation
layer instead.

Failure classification is also effectively closed to casual edits: `ClassifyError`
and `ClassifyFailure` live in the same file specifically so their disagreement
stays visible, and changing one without the other reintroduces the bug described
below.

**Deliberately closed: authentication on the management and registry routes.**
None of `/v1/providers`, `/v1/capabilities`, `/v1/breaker`, `/v1/routing/decision`,
or `/v1/registry/*` passes through `wrapAIQG`. When SEC-1 closed their public
exposure it chose the Ingress, not the handler, as the gate — "no reliance on the
app's own middleware" (`k8s/ingress-gateway-airops.yaml:14`–`18`) — and the
public host lists only the completion paths, with the rule that a read route is
added "only with an auth decision attached" (`k8s/ingress-gateway-airops.yaml:27`–`30`).
If you add an operator route, keep it off the public allowlist; exposing one
means making that auth decision first.

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
(`internal/metrics/metrics.go:258`–`264`). The cost is that the function runs on
every scrape, which is why it is documented as needing to be cheap and
concurrency-safe (`internal/metrics/metrics.go:285`).

**Enforcement decides per finding, not per request severity.** The operator's
rule is the authority: a `critical` finding for a pattern the bundle only logs
should not block merely because it is critical.

**The model registry changes names only for models it has discovered, and never
calls a vendor on the request path.** The obvious alternative — validate the
requested model against the vendor before routing — adds a network round trip
to every request and turns a vendor blip into a routing failure. Instead a
background sync engine writes a status and `Route` reads the cached value;
unknown names pass through untouched, and a nil registry is the default, so the
feature cannot change routing for anyone until an operator turns it on
(commit `55ba24b`, #5; `internal/routing/router.go:86`–`95`). A deprecated model
is served as asked rather than swapped, because it still answers and a silent
substitution would surprise the caller; only `unavailable` triggers a fallback.
The same reasoning shapes discovery: a transient probe error leaves a model's
status unchanged instead of marking it unavailable, so a vendor outage during a
sync cannot cascade into substitutions (`internal/registry/adapters/anthropic.go`).

**A pin the provider cannot serve degrades rather than fails.** Rejecting the
request would have been simpler, and it is what the code will eventually do. It
was rejected for now because failing outright "would turn a momentary provider
blip into a tenant outage for every rule that has not yet configured failover"
(`internal/routing/router.go:1375`–`1381`); #151 extended the same treatment to
a model the pinned vendor does not list, which had surfaced as an opaque `500`.
An empty model list is deliberately read as "cannot tell", so a provider
configured without a list keeps its working pins (`internal/routing/router.go:586`–`593`).

**A BYOK-only tenant's evaluations are skipped, not failed and not charged to
the shared key.** Failing would be wrong because no customer is waiting on an
evaluation; charging the shared key would override an explicit "never use the
shared key" in order to run something the tenant did not ask for. Skipping is
the only outcome consistent with what was declared, and it is counted so the
missing coverage is visible (`internal/server/eval_credentials.go:43`–`48`).
Shadow-replay spend follows the same consent logic: the rate moved from one
gateway-wide switch into each experiment's guardrails, and it deliberately does
not take the minimum of the two, because the global default is `0` and a
minimum would make every declared rate inert (commit `333365e`, #213).

**Prompt caching defaults to passthrough, not auto.** Automatic placement would
help callers who mark breakpoints badly, but a 30-day measurement on 2026-08-20
found only 2 of 16 probe measurements with any prefix reuse and a mean
cacheable prefix of 613 tokens, against per-model minimums of 1024 to 4096. That
was judged too thin to justify rewriting every caller's breakpoints by default,
so `auto` stays opt-in per request or per gateway until a route with real prefix
volume can be measured (`pkg/aiqg/promptcache/mode.go:43`–`54`).

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

**`503 Routing failed: …`** — the router could not *select* a provider, so no
vendor was called (`internal/server/server.go:1375`). The text after the colon
names the reason; the common ones, from `internal/routing/router.go`, are `provider <name> is not
healthy` (you named a model only that provider serves, and it is down, `:640`);
`no healthy providers available` (`:667`); `no providers support required
features` (`:673`); or `no provider satisfies this tenant's routing constraints`
(`:1421`). Retry with backoff. Exhausting retries or fallbacks is a different
error, `500 Completion failed`. (Until 2026-09-21 this entry said a `503` meant
"the chain was exhausted, or never engaged"; the code does not support that.)

**`415 Content-Type must be application/json`** on a request that looks right —
your client appended `; charset=utf-8` or another parameter. Send the bare
value. Observed 2026-09-21.

**A `401` from the strict gateway saying `no_resolver_configured`** (after the
next rollout) — the operators' token list is empty. Nothing about your token is
wrong; report it.

**`404` on `GET /v1/models` from `gateway.air-ops.net`** — the public host routes
only the six completion paths. Hard-code the model name, or list models from
the internal host.

**`503 model registry is not enabled`** from a `/v1/registry/*` route — the
registry is off, which is the default. Nothing else on the gateway depends on
it.

**The response names a different model from the one you sent** — with the
registry on, an alias was resolved or an `unavailable` model was replaced. Read
`X-TAS-Router-Model`, and `router_metadata.original_model` /
`fallback_reason` on an OpenAI-dialect JSON body.

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
the customer-facing one. On 2026-09-21 `llm-router-aiqg` still answered `1` on
the same image. The same probe doubles as a test for everything this document
marks "committed, not yet deployed": while it answers `1`, none of that is live.
(The internal host still serves `/metrics`; the public `gateway.air-ops.net`
returns `404` for it.)

**`llm_router_tokens_total` and `llm_router_cost_total` under-count your
traffic** — they are fed only from the non-streaming completion paths
(`internal/server/server.go:1772` and `internal/server/server.go:1995`). A
`"stream": true` request is counted in `requests_total` and timed in
`request_duration_seconds`, but contributes no tokens and no cost. If your
integration streams, these two series are not a spend figure; the AIQG event
stream (defined under vocabulary) is, and you read it through the dashboard
backend rather than through this gateway. That was the code at `eee4b24`. It
is not the deployed behaviour either: the deployed images serve the old
exporter, whose token and cost numbers are not measurements at all (see
telemetry). Since `cddd372` (#171), committed but not deployed, a stream that reports usage
feeds both series (`internal/server/server.go:1872`–`1876`). Anthropic streams
report usage in their final chunk; OpenAI streams, per the marker under feature
support, appear never to. So after the next rollout the under-count narrows to
OpenAI-served streams and to Anthropic streams that broke before their final
chunk.

**A stream ends with an error event instead of a normal end** (after the next
rollout) — the vendor failed mid-stream. The status line still says `200`. Treat
the response as failed, and expect that the tokens generated before the break
were billed — the code comment says as much (`internal/server/server.go:1870`–`1871`).

## Limits & trade-offs

**Read endpoints are unauthenticated.** `/v1/models`, `/v1/providers`, and
`/v1/capabilities` returned `200` to anonymous requests in live probes,
disclosing the model catalogue, the provider list, and the capability matrix
including context-window sizes. This is a deliberate convenience for SDK
discovery and also a public exposure — treat the catalogue as public information.

**`/metrics` is unauthenticated too, and it now carries real numbers.** It is
registered on the root router without `wrapAIQG`
(`internal/server/server.go:982`), so anyone who can reach the endpoint reads it.
Before `b6070a0` that exposed fiction; afterwards it exposes real request rates,
real token volumes, and real dollar cost per provider and model. The series carry
no tenant label, so this is aggregate rather than per-customer disclosure, but
treat the endpoint as sensitive in a way it was not before and confirm your
deployment restricts the path at the ingress rather than relying on obscurity.
The public hosts now do (SEC-1, SEC-23: `k8s/ingress-gateway-airops.yaml`,
`k8s/ingress-llm-airops.yaml`); `gateway.aiqg.tas.scharber.com` does not, and
served `/metrics` anonymously on 2026-09-21.

**Two registry routes spend money without authentication** (committed, not yet
deployed, and only with the registry enabled). `POST /v1/registry/sync` sends a
billable one-token request per configured Anthropic model, and `POST
/v1/registry/validate` one per call. The sync route is limited to one pass per
ten seconds gateway-wide; validate has no limit at all
(`internal/server/registry_admin.go:28`–`31`, `:163`–`189`). Both are reachable
by anyone who can reach an internal host.

**A registry fallback can pick a pricier model.** Without a configured
`replacement_model`, the fallback for an `unavailable` model is the
alphabetically first active model of the same provider, whatever it costs, and
the `registry.fallback.*` settings do not change that
(`internal/registry/registry.go:219`–`244`).

**Metrics are process-local and reset on restart.** The registry lives in
process memory (`internal/metrics/metrics.go:50`), so every counter starts at
zero on a new pod and each replica reports only its own share. Aggregate across
replicas in the query, and expect `rate()` — not the raw counter — to be the
thing that survives a rollout.

**Nothing is idempotent.** Every completion call bills and emits telemetry, and
there is no request-id deduplication: a search of the repository at `eee4b24`
finds no idempotency key, no replay cache, and no dedup check on the completion
path. A supplied `id` is not consulted for replay — when absent the handler
mints a fresh one from the clock (`internal/server/server.go:1070`–`1072`) and
otherwise passes yours through as a label. A client retry is a second charge,
which is why the SDK max-retries advice under Getting started matters.

**Interposition is unavoidable.** Every request is parsed and re-serialized. A
vendor feature that has no representation in the translation layer is unavailable
until the layer learns it, and there is no escape hatch.

**Version skew between deployments.** `llm-router` and `llm-router-aiqg` run
independent image tags — observed on 2026-08-25 as `aiqg-v5.75` and `aiqg-v5.86`
respectively, eleven releases apart. Behaviour verified against one is not
guaranteed on the other; see the operations document, and use the exporter probe
under failure modes to establish what a given pod is actually running. Unchanged
on 2026-09-21, when both tags were read again with `kubectl`. Commit `000394a`
(OPS-27) now records each Deployment's real tag in its manifest
(`k8s/deployment.yaml:59`, `k8s/deployment-aiqg-strict.yaml:71`) instead of a
shared `latest`.

## Related

- OpenAPI specification: `docs/openapi.yaml` (served at `/docs`)
- Data models: `aether-shared/data-models/tas-llm-router/`
- Operations and on-call: `docs/ops/llm-router.md`
- Routing design: `aether-shared/data-models/aiqg/routing-decision.md`
- Metric definitions and the reasoning behind them: `internal/metrics/metrics.go`
- Metric guarantees expressed as tests: `internal/metrics/metrics_test.go`
- Prompt-cache design: `docs/AIQG-PROMPT-CACHE-CONTROL.md`
- Model registry: `internal/registry/` (and its tests), admin handlers in `internal/server/registry_admin.go`
- Public ingress allowlists and the reasoning behind them: `k8s/ingress-gateway-airops.yaml`, `k8s/ingress-llm-airops.yaml`
