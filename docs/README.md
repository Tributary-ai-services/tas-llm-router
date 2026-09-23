---
doc_type: readme
audience: "Engineer who needs a document about this gateway and does not know which one — on-call, integrating, or evaluating the product"
assumes: ["what an LLM gateway does", "reading Markdown in a git checkout or on GitHub"]
answers:
  - "Where do I look when the service is paging me?"
  - "Which document tells me how to call the gateway and what its errors mean?"
  - "Which documents here are current, and which are historical?"
  - "Where is the machine-readable request and response contract, and where is it served?"
  - "What are all the AIQG files, and which reader are they for?"
  - "How do I tell a maintained document from an abandoned one without reading it?"
  - "What lives outside this directory that I should know about?"
depth: standard
verified_against: "tas-llm-router@552d869, 2026-09-21"
---

# LLM Router — Documentation Index

> Verified 2026-09-21 against `tas-llm-router@552d869`. Currency labels below come
> from each file's own front matter or status header and from `git log` on that
> file at that commit. One claim was checked against the running service: the
> OpenAPI spec served at `/docs/openapi.yaml`, fetched read-only on 2026-09-21.
> Nothing else here was checked against the cluster.

## What this is

This directory holds the documentation for `tas-llm-router`, the gateway every Tributary
AI Services (TAS) service calls when it needs a commercial language model. This file is the index,
and its only job is to send you to the right document in one hop.

It is not an overview of the service and it deliberately does not summarise
anything it links — a paraphrase here would be a second copy to keep in step, and
it would rot first. If you want to know what the gateway does before picking a
document, the repository's top-level `../README.md` is the shorter read.

The directory mixes two collections that serve very different readers. One is the
operations and developer documentation for the service that is deployed today:
maintained, stamped with the commit it was verified against, and regenerated when
the code it describes moves. The other is the design, prior-art, and positioning
corpus for the AI Quality Gateway (AIQG) product built on top of the gateway, and
a substantial part of it describes work that was specified but never built.
Reaching for the second collection when you wanted the first is the specific
mistake this page exists to prevent, so the two are kept apart below rather than
listed together by filename.

## Status & scope

As of 2026-09-21 there are four tiers of currency in this directory, and they are
not interchangeable.

Two terms in the table need defining first. A document's **watched code** is the
list of source paths that [`doc-manifest.yaml`](doc-manifest.yaml) assigns to it;
when a commit touches one of those paths after the commit the document was last
verified against, the document is marked stale and **regenerated** — rewritten by
the documentation tooling against the new code and re-stamped. Only documents
listed in that manifest get this treatment. The Configuration section below gives
the mapping.

| Tier | Files | Last changed | What that means for you |
|---|---|---|---|
| Maintained and stamped | `ops/llm-router.md`, `dev/llm-router-api.md`, `concept/routing.md` | 2026-08-26, 2026-08-26, 2026-08-27 | Each carries a `verified_against` commit in its front matter and is regenerated when watched code changes. The stamps differ — `eee4b24` for the first two, `897e441` plus live captures for the routing guide — so read the stamp in the file itself. |
| Machine contract | `openapi.yaml` | 2026-09-09 | Compiled into the binary by `embed.go` and served at `/docs`, so a running gateway serves the bytes of the commit it was built from — not necessarily this checkout. On 2026-09-21 the deployed copy was one revision behind; see How it fits. |
| Design corpus, self-dated | the fourteen `AIQG-*.md` files | 2026-06-01 to 2026-09-15 | Every one opens with its own status line. Several say design-only, and at least two of those lines are now out of date (below). Treat the line as a hint about intent, and confirm what exists in `ops/llm-router.md` or the code. |
| Unmaintained | `index.md`, `user-guide.md`, `api-reference.md`, `admin-guide.md`, `developer-guide.md`, `security-guide.md`, `llm-invocation-package-design.md` | 2025-08-09 to 2026-08-14 | Written before the AIQG gateway existed. They describe one local process on port 8080; the deployed service listens on 8086 and runs as two separate deployments in namespace `tas-llm-router`: `llm-router` (`k8s/deployment.yaml`) and the strict AIQG gateway `llm-router-aiqg` (`k8s/deployment-aiqg-strict.yaml`). |

The unmaintained tier has one partial exception. `user-guide.md` had an
eighteen-line section, "Authentication (AIQG gateway)", added on 2026-08-14; the
rest of it dates from 2025-08-09. **Verdict: historical.** The one newer section
was accurate when it was added, but it carries no stamp and nothing regenerates
it, so confirm anything in it against `dev/llm-router-api.md` before relying on it.

### Verdict for each AIQG file

The design corpus is neither maintained nor uniformly out of date, so each file
gets its own verdict below. "Consistent with code" means the packages the file's
status line names exist at `552d869`; it does not mean the feature is deployed or
switched on. Four terms recur: **Gatekeeper** is a separate Tributary content-scanning
and redaction library that the router imports in `internal/gatekeeper/gatekeeper.go`;
**boundary scanning** means scanning prompts on the way into the gateway and
responses on the way out (that file tags each scan `llm_input` or `llm_output`);
**`tas-mcp`** is the TAS Model Context Protocol (MCP) federation server, a sibling
repository; and the **judge path** is the gateway asking a second model to score
a response it has already served (`internal/server/judge.go`).

| File | Verdict | Why |
|---|---|---|
| `AIQG-EXTENSION.md` | Historical | The founding spec, a "Draft v1.0" dated 2026-05-31. Read it for original intent; what was built is in the ops and dev documents. |
| `AIQG-AGENT-FLOW-ATTRIBUTION.md` | Partly current | Its header says "Partially shipped": header and baggage plumbing shipped 2026-06-11, consistent with `internal/middleware/aiqg_headers.go` and `pkg/aiqg/linkage/`. Its inferred-attribution section is marked design-only; whether that is still true is unknown — confirm against `internal/middleware/`. |
| `AIQG-AGENT-IDENTITY-RESEARCH.md` | Historical | A prior-art survey of outside standards, not a description of this code. It records the landscape as of 2026-06. |
| `AIQG-CACHING.md` | Current per its header | Says exact-match stages C1–C3 are implemented; `pkg/aiqg/responsecache/` exists. |
| `AIQG-SEMANTIC-CACHING.md` | Current per its header | Says shadow mode is live and serving is off; `pkg/aiqg/semcache/` exists. Whether shadow mode is on in the cluster today is unknown — confirm in `ops/llm-router.md`. |
| `AIQG-PROMPT-CACHE-CONTROL.md` | Partly stale | Its opening line still says a design for review and not implemented. Its rollout table, edited 2026-09-06 and 2026-09-07, marks P0 through P2 done — the first three rollout stages: measure prefix reuse, pass vendor cache directives through, and place them automatically. The code exists (`pkg/aiqg/promptcache/`, the `TAS-Prompt-Cache` header in `pkg/aiqg/promptcache/mode.go`). Trust the table over the opening line; P3 and P4 are unbuilt. |
| `AIQG-EXPERIMENTS-RUNNER.md` | Partly stale | Also opens with a not-implemented claim, yet its 2026-09-15 edit documents the judge path and a metric that `pkg/aiqg/metrics/metrics.go` exports, and `pkg/aiqg/experiments/` exists. How much of the full runner is built is unknown — confirm against `pkg/aiqg/experiments/`. |
| `AIQG-GATEKEEPER-INTEGRATION.md` | Partly stale | Declares itself design-only as of 2026-07-16, but `internal/gatekeeper/` exists at `552d869`, imports Gatekeeper's scan and pipeline packages, and has redaction tests. Read it for the design; do not read it as proof that scanning is absent. |
| The six patent, freedom-to-operate, and investor files | Historical by design | Point-in-time analyses dated 2026-06-12 to 2026-06-19 for counsel and investors. They do not track code. `AIQG-PATENT-SCOUT-AUTOLEARNING.md` marks some features shipped in the separate `aiqg-ui` repository; that is unknown here — confirm in `aiqg-ui`. |

> [!UNVERIFIED] The status lines in the `AIQG-*.md` files are author-reported and
> were not re-checked against the cluster for this index. Where a verdict above
> says "consistent with code" or "the code exists", that was checked in the
> repository at `552d869`, not in either deployment. Whether boundary scanning,
> prompt-cache placement, semantic-cache shadow mode, or the judge path is
> switched on in `llm-router` or `llm-router-aiqg` was not checked. Confirm
> capability questions against `ops/llm-router.md` or the code, never against a
> design document's own status.

## Quick start — find the document you need

Pick the row that matches why you opened this directory.

| Your situation | Go to |
|---|---|
| The service is paging you, or callers report errors | [`ops/llm-router.md`](ops/llm-router.md) |
| You are writing code that calls the gateway and need the error set, retry, and fallback semantics | [`dev/llm-router-api.md`](dev/llm-router-api.md) |
| A request went to a vendor or model you did not expect, or you are configuring routing for a tenant | [`concept/routing.md`](concept/routing.md) |
| You need exact request and response schemas | [`openapi.yaml`](openapi.yaml), or the interactive page the gateway serves at `/docs` |
| You want to know whether an AIQG capability exists before designing around it | `ops/llm-router.md` or the code; the design file's own status line may be stale |
| You are working on caching or payload cost | [`AIQG-CACHING.md`](AIQG-CACHING.md), [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md), [`AIQG-PROMPT-CACHE-CONTROL.md`](AIQG-PROMPT-CACHE-CONTROL.md) |
| You are preparing patent, freedom-to-operate, or investor material | the six analysis files grouped near the end of this page |

When a document is not on that list, the fastest way to tell whether anyone is
still maintaining it is the verification stamp. Only regenerated documents carry
one, so this command separates the maintained set from everything else without
opening a single file:

```bash
$ grep -rl --include='*.md' '^verified_against:' docs/ | sort
docs/README.md
docs/concept/routing.md
docs/dev/llm-router-api.md
docs/ops/llm-router.md
```

Four hits out of twenty-five Markdown files. Everything the command does not
print is either a self-dated design document or unmaintained, and the tiers above
say which.

## How it fits

These documents describe one service, not the platform. Contracts that cross a
service boundary — identifiers, tenant scoping, event shapes shared with the
dashboard and the Spark jobs — are owned by `aether-shared/data-models/`, and
`dev/llm-router-api.md` links the specific model pages rather than restating them.
The customer-facing caching and payload-reduction write-ups live in the `tas-aiqg`
repository; the files here are the engineering originals those were drawn from,
which is why the two sets overlap in subject and differ in audience.

Two of these files are published rather than merely committed. `openapi.yaml` is
embedded in the binary by [`embed.go`](embed.go), so `/docs` serves the spec that
shipped with the running build instead of whatever is on disk. The routes are
registered in `internal/server/swagger.go`: `/docs` for the interactive page, and
`/docs/openapi.yaml` and `/docs/openapi.json` for the raw spec.
[`../k8s/ingress-docs-airops.yaml`](../k8s/ingress-docs-airops.yaml) exposes that
page publicly at `docs.air-ops.net`, scoped to the `/docs` path precisely because
that host has no access policy in front of it. Anything you write into
`openapi.yaml` is world-readable once deployed.

The embedding also means the file in this checkout can be ahead of what callers
see. The spec's last change, on 2026-09-09, added five dynamic model registry
admin paths under `/v1/registry/` (`models`, `models/{provider}`, `status`,
`sync`, `validate`); `dev/llm-router-api.md` does not cover them, so the spec is
their only reference here. On 2026-09-21 the deployed spec did not include them —
it was byte-identical to the revision before that change — so the running build
predates it:

```bash
$ curl -sS https://docs.air-ops.net/docs/openapi.yaml | grep -c '/v1/registry'
0
$ grep -c '/v1/registry' docs/openapi.yaml
5
```

`https://llm-router.tas.scharber.com/docs/openapi.yaml` returned the same bytes.
When the two counts match, the deployed contract has caught up with this file.

## Configuration

The maintained documents are generated, and two files control that. Front matter
at the top of each one declares `doc_type`, `audience`, the reader questions the
document must answer, and `verified_against`; a document without that block is not
gated and is not refreshed. [`doc-manifest.yaml`](doc-manifest.yaml) maps code
paths to documents, so a change under `internal/server/` marks all three
maintained documents stale, a change under `internal/routing/` marks the developer
guide and the routing guide but not the ops document, and any change under `docs/`
marks this index. Check both files before adding a document you expect to stay
current.

Credentials appear here by location only. Both deployments mount the
`llm-router-aiqg-tokens` Secret in the `tas-llm-router` namespace
(`../k8s/secret-aiqg-tokens.yaml` is the template that names it) and read provider
keys from `llm-router-secret`; the values themselves live in the
`aether-secrets` repository. `../.env.example` lists the variables a local run
expects without supplying any of them. No document in this directory should ever
contain a credential value.

## Where to go next

### Start here

- [`ops/llm-router.md`](ops/llm-router.md) — on-call reference: health signals, restart and rollback costs, and failure modes with the literal error strings.
- [`dev/llm-router-api.md`](dev/llm-router-api.md) — integrating or extending the gateway: surfaces, auth, the full error set, and what may change without warning.
- [`concept/routing.md`](concept/routing.md) — for callers configuring routing: why a request reached the vendor it did, which routing controls take effect, and how to trace one decision after the fact.
- [`openapi.yaml`](openapi.yaml) — the wire contract itself, in a form you can generate clients and tests from; also the only reference here for the `/v1/registry/` admin endpoints.

### AI Quality Gateway — design and specification

Each file states its own status and date on its first lines. Read that before treating any of them as a specification of what runs.

- [`AIQG-EXTENSION.md`](AIQG-EXTENSION.md) — the founding architecture spec for adding AIQG to this repository without breaking existing callers.
- [`AIQG-AGENT-FLOW-ATTRIBUTION.md`](AIQG-AGENT-FLOW-ATTRIBUTION.md) — grouping gateway calls into the agent, run, and step that produced them.
- [`AIQG-AGENT-IDENTITY-RESEARCH.md`](AIQG-AGENT-IDENTITY-RESEARCH.md) — the prior-art survey that the attribution design rests on.
- [`AIQG-EXPERIMENTS-RUNNER.md`](AIQG-EXPERIMENTS-RUNNER.md) — running a fraction of live traffic on a variant config, and the guardrails that would gate it.
- [`AIQG-CACHING.md`](AIQG-CACHING.md) — exact-match response caching: keying, accounting, and interaction with experiments.
- [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md) — near-duplicate cache hits, the verification cascade, and the calibration tooling.
- [`AIQG-PROMPT-CACHE-CONTROL.md`](AIQG-PROMPT-CACHE-CONTROL.md) — passing vendor prompt-cache directives through a typed request struct that currently discards them.
- [`AIQG-GATEKEEPER-INTEGRATION.md`](AIQG-GATEKEEPER-INTEGRATION.md) — where prompt scanning and redaction attach, across this repository, `tas-mcp`, and Gatekeeper.

### Patent, freedom-to-operate, and investor material

Written for counsel and for fundraising, not for engineering. Each disclaims being legal advice in its own header, and each has a rendered `.pdf` beside it.

- [`AIQG-PATENT-ANALYSIS.md`](AIQG-PATENT-ANALYSIS.md) — which parts of the design are plausibly novel and which are prior art.
- [`AIQG-PROVISIONAL-BRIEF.md`](AIQG-PROVISIONAL-BRIEF.md) — six filing candidates, drafted for a patent attorney to work from.
- [`AIQG-FTO-CLAIM-READ.md`](AIQG-FTO-CLAIM-READ.md) — four granted patents read against the attribution design, technique by technique.
- [`AIQG-PATENT-SCOUT-AUTOLEARNING.md`](AIQG-PATENT-SCOUT-AUTOLEARNING.md) — two further candidates covering experiment prioritisation and adaptation.
- [`AIQG-PATENT-ML-IN-LOOP.md`](AIQG-PATENT-ML-IN-LOOP.md) — the argument for anchoring model-in-the-loop claims to signals only a gateway has.
- [`AIQG-INVESTOR-ANALYSIS.md`](AIQG-INVESTOR-ANALYSIS.md) — how the pieces are positioned as one product for an outside audience.

### Superseded and unmaintained

Kept for history. Where any of these disagrees with the maintained set, the maintained set wins.

- [`index.md`](index.md) — an earlier index of this same directory, superseded by this file.
- [`user-guide.md`](user-guide.md) — calling the API by hand and from vendor SDKs; one 2026-08-14 section, the rest from 2025.
- [`api-reference.md`](api-reference.md) — hand-written endpoint reference, replaced by `openapi.yaml` and `dev/llm-router-api.md`.
- [`admin-guide.md`](admin-guide.md) — install, configure, and deploy as a standalone process; predates the Kubernetes deployments.
- [`security-guide.md`](security-guide.md) — the original auth, rate-limiting, and audit-logging design.
- [`developer-guide.md`](developer-guide.md) — local setup and how to add a provider, from the original code layout.
- [`llm-invocation-package-design.md`](llm-invocation-package-design.md) — design notes for the separate `llm-invocation` client library.

### Elsewhere in the repository

- [`../README.md`](../README.md) — the repository landing page.
- [`../llm-router-waf-design.md`](../llm-router-waf-design.md) — the original Web Application Firewall (WAF) design document the service grew from.
- [`../CLAUDE.md`](../CLAUDE.md) — repository conventions for agents and contributors working in this tree.
- [`doc-manifest.yaml`](doc-manifest.yaml) and [`embed.go`](embed.go) — the two machine-read files in this directory.
