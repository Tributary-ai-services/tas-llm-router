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
verified_against: "tas-llm-router@eee4b24, 2026-08-26"
---

# LLM Router — Documentation Index

> Verified 2026-08-26 against `tas-llm-router@eee4b24`. Currency labels below come
> from each file's own front matter or status header and from `git log` on that
> file at that commit. Nothing here was checked against the running cluster.

## What this is

This directory holds the documentation for `tas-llm-router`, the gateway every TAS
service calls when it needs a commercial language model. This file is the index,
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

As of 2026-08-26 there are four tiers of currency in this directory, and they are
not interchangeable.

| Tier | Files | Last changed | What that means for you |
|---|---|---|---|
| Maintained and stamped | `ops/llm-router.md`, `dev/llm-router-api.md` | 2026-08-25, 2026-08-24 | Each carries a `verified_against` commit in its front matter and is regenerated when watched code changes. Read the stamp in the file itself. |
| Machine contract | `openapi.yaml` | 2026-08-14 | Compiled into the binary by `embed.go` and served at `/docs`, so the running gateway and this file are the same bytes. |
| Design corpus, self-dated | the fourteen `AIQG-*.md` files | 2026-06-01 to 2026-07-18 | Every one opens with its own status line. Several say design-only. Trust that line, not the file's existence. |
| Unmaintained | `index.md`, `user-guide.md`, `api-reference.md`, `admin-guide.md`, `developer-guide.md`, `security-guide.md`, `llm-invocation-package-design.md` | 2025-08-09 to 2026-08-14 | Written before the AIQG gateway existed. They describe one local process on port 8080; the deployed service listens on 8086 and runs as two separate deployments. |

The unmaintained tier has one partial exception: `user-guide.md` had an
eighteen-line gateway-authentication section added on 2026-08-14, while the rest of
it dates from 2025-08-09. Treat the file as old with one recent patch rather than
as current.

> [!UNVERIFIED] The status lines in the `AIQG-*.md` files are author-reported and
> were not re-checked against the cluster for this index.
> `AIQG-GATEKEEPER-INTEGRATION.md` declares itself design-only as of 2026-07-16,
> and boundary scanning is believed to have shipped shortly after that date; its
> header may understate what exists. Confirm capability questions against
> `ops/llm-router.md` or the code, never against a design document's own status.

## Quick start — find the document you need

Pick the row that matches why you opened this directory.

| Your situation | Go to |
|---|---|
| The service is paging you, or callers report errors | [`ops/llm-router.md`](ops/llm-router.md) |
| You are writing code that calls the gateway and need the error set, retry, and fallback semantics | [`dev/llm-router-api.md`](dev/llm-router-api.md) |
| You need exact request and response schemas | [`openapi.yaml`](openapi.yaml), or the interactive page the gateway serves at `/docs` |
| You want to know whether an AIQG capability exists before designing around it | the file's own status line, then confirm in `ops/llm-router.md` |
| You are working on caching or payload cost | [`AIQG-CACHING.md`](AIQG-CACHING.md), [`AIQG-SEMANTIC-CACHING.md`](AIQG-SEMANTIC-CACHING.md), [`AIQG-PROMPT-CACHE-CONTROL.md`](AIQG-PROMPT-CACHE-CONTROL.md) |
| You are preparing patent, freedom-to-operate, or investor material | the six analysis files grouped near the end of this page |

When a document is not on that list, the fastest way to tell whether anyone is
still maintaining it is the verification stamp. Only regenerated documents carry
one, so this command separates the maintained set from everything else without
opening a single file:

```bash
$ grep -rl --include='*.md' '^verified_against:' docs/ | sort
docs/README.md
docs/dev/llm-router-api.md
docs/ops/llm-router.md
```

Three hits out of twenty-four Markdown files. Everything the command does not
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
shipped with the running build instead of whatever is on disk.
[`../k8s/ingress-docs-airops.yaml`](../k8s/ingress-docs-airops.yaml) exposes that
page publicly at `docs.air-ops.net`, scoped to the `/docs` path precisely because
that host has no access policy in front of it. Anything you write into
`openapi.yaml` is world-readable once deployed.

## Configuration

The maintained documents are generated, and two files control that. Front matter
at the top of each one declares `doc_type`, `audience`, the reader questions the
document must answer, and `verified_against`; a document without that block is not
gated and is not refreshed. [`doc-manifest.yaml`](doc-manifest.yaml) maps code
paths to documents, so a change under `internal/server/` marks both maintained
documents stale while a change under `internal/routing/` marks only the developer
guide. Check both files before adding a document you expect to stay current.

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
- [`openapi.yaml`](openapi.yaml) — the wire contract itself, in a form you can generate clients and tests from.

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
