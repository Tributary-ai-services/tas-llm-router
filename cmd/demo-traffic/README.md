# demo-traffic — AIQG demo data generator

Generates synthetic **AIQG response-event** log lines for a set of agent
traffic types and pushes them to Loki so the AIQG dashboard's CLEAR,
cost, latency, tag, and avoidable-cost panels light up with believable
multi-agent traffic — and reports the **potential savings** AIQG would
surface.

It does **not** call any vendor LLM. Per-step inputs are sampled from
eight workflow profiles and scored by the **real** `pkg/clear` scorer
(`clear.Compute`) using the **real** pricing table (`clear.DollarCost`),
so demo CLEAR scores and dollar costs match what the live gateway would
produce for the same inputs. The emitted JSON matches the field shape of
`pkg/aiqg/events/emitter.go` exactly, so the dashboard reads it
identically to a real gateway line.

Traffic is generated as **agent flows**: each demo agent runs flows whose
steps span workflow types, sharing a `flow_id` / `conversation_id` and
linked via `step_id` / `parent_step_id` — so the per-agent rollup and the
flow drill-down (trace waterfall) both have real structure. See
[`docs/AIQG-AGENT-FLOW-ATTRIBUTION.md`](../../docs/AIQG-AGENT-FLOW-ATTRIBUTION.md)
for the identity model these fields implement.

## Demo agents and their flows

| Agent | Flow shape (step → workflow_type) | Multi-turn |
|---|---|---|
| **Research Orchestrator** | planner(`agentic`) → 2–4× retrieve(`rag`) → synthesize(`summarization`) | no |
| **Coding Copilot** | plan(`agentic`) → generate(`code_generation`) → ~40% fix-on-fail(`code_generation`) | up to 2 |
| **Support Bot** | classify(`classification_extraction`) → answer(`single_turn_qa`) | 2–4 turns share one `conversation_id` |
| **Data Extractor** | 3–6 sibling extraction steps(`classification_extraction`) under a root | no |

A configurable number of **inferred / unattributed** flows are also
emitted: `identity_source=inferred`, `flow_id` present (as if
reconstructed from the session), but **no** `agent_id` / `agent_name` —
representing uninstrumented traffic. Named flows carry
`identity_source=header`, with a fraction `trace` (flow id arrived via a
W3C `traceparent`: 32-hex `flow_id`, 16-hex `step_id`). `agent_id` is
stable per agent name across runs/seeds.

Agent-flow fields stamped on every step (promoted top-level for LogQL):
`agent_id`, `agent_name`, `agent_version`, `conversation_id`, `flow_id`,
`step_id`, `parent_step_id`, `flow_step_seq`, `identity_source`.

## Workflow profiles (one per step)

| Profile | workflow_type | Story it tells |

| Profile | workflow_type | Story it tells |
|---|---|---|
| `basic_qa` | single_turn_qa | clean, low-cost baseline |
| `conversational` | single_turn_qa | growing history, reliability dips, hedging |
| `rag` | rag | large retrieved context → **bloat** |
| `mcp_agentic` | agentic | tool loops, retries, **refusals**, latency tails |
| `summarization` | summarization | large in / small out, truncation |
| `code_generation` | code_generation | regenerate-on-bad-code, malformed output |
| `classification_extraction` | classification_extraction | high-volume cheap, JSON-conformance failures |
| `multi_agent_orchestration` | agentic | fan-out token amplification → **headline savings** |

**Cross-cutting attributes** — compliance (PII / credentials / injection
/ safety), vague input, and hedging — are sprinkled across every profile
at tunable rates rather than being separate types. Each profile also has
a *signature matcher* that's guaranteed to fire on the first event of
every pass, so a single small pass still lights up every dashboard panel
(tags, NIST, Assurance, all four avoidable-cost categories).

## Usage

```bash
# One pass to the default Loki, default aiqg-demo tenant, then exit.
go run ./cmd/demo-traffic

# Inspect the flows without pushing.
go run ./cmd/demo-traffic --dry-run --flows-per-agent 2

# Bigger pass (6 flows per agent + 4 unattributed flows).
go run ./cmd/demo-traffic --flows-per-agent 6 --inferred-flows 4

# Keep emitting one pass every 30s (Ctrl-C to stop).
go run ./cmd/demo-traffic --interval 30s

# Dial up the compliance/savings story for a demo.
go run ./cmd/demo-traffic --flows-per-agent 10 --compliance-rate 0.2 --vague-rate 0.2 --hedging-rate 0.2
```

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--loki-url` | `https://loki.tas.scharber.com` | Loki base URL (push API at `/loki/api/v1/push`) |
| `--tenant-id` | aiqg-demo tenant | `tenant_id` stamped on every event (dashboard filters on it) |
| `--account-id` | aiqg-demo account | `aiqg_account_id` |
| `--flows-per-agent` | `4` | flows per demo agent per pass (each flow emits several step-events) |
| `--inferred-flows` | `3` | unattributed flows per pass (`identity_source=inferred`, no agent id) |
| `--interval` | `0` | if `>0`, loop forever emitting one pass per interval |
| `--seed` | `0` | RNG seed for reproducible runs (0 = time-based) |
| `--spread` | `90` | seconds to spread a pass's events over, ending at now |
| `--dry-run` | `false` | print lines to stdout instead of pushing |
| `--insecure` | `true` | skip TLS verify (TAS Loki uses the internal `tas-ca-issuer` CA) |
| `--org-id` | `""` | optional `X-Scope-OrgID` header for multi-tenant Loki |
| `--compliance-rate` | `0.08` | fraction carrying a compliance finding |
| `--vague-rate` | `0.12` | fraction carrying a vague-input finding |
| `--hedging-rate` | `0.12` | fraction carrying a hedging finding |
| `--error-rate` | `0.02` | fraction that failed upstream (`vendor_error`) |

The run prints a summary: total events, total cost, **potential savings**
(avoidable spend) and %, plus a per-profile breakdown with average CLEAR.

## Verifying the dashboard lit up

The generator's tenant must match the account you view the dashboard as.
These are the dashboard's actual queries (`internal/handlers/metrics.go`
and `avoidable_cost.go`), runnable directly against Loki:

```bash
TENANT="a689c0b2-02ca-46d1-9916-f9a30c00222a"   # aiqg-demo
LOKI="https://loki.tas.scharber.com"
END=$(date +%s); START=$((END-900))
q() { curl -skS -G "$LOKI/loki/api/v1/query_range" \
  --data-urlencode "query=$1" \
  --data-urlencode "start=${START}000000000" --data-urlencode "end=${END}000000000" \
  --data-urlencode "step=5m"; echo; }

# CLEAR composite (avg_over_time) — /metrics/clear
q "avg_over_time({namespace=\"tas-llm-router\"} |= \"aiqg response event\" | json | tenant_id=\"$TENANT\" | unwrap clear_composite [5m])"

# Total cost USD — /metrics/cost
q "sum_over_time({namespace=\"tas-llm-router\"} |= \"aiqg response event\" | json | tenant_id=\"$TENANT\" | unwrap total_cost_usd [5m])"

# p95 latency — /metrics/latency
q "quantile_over_time(0.95, {namespace=\"tas-llm-router\"} |= \"aiqg response event\" | json | tenant_id=\"$TENANT\" | unwrap end_to_end_ms [5m])"

# Avoidable-cost: Hedging category — /metrics/cost avoidable breakdown
q "sum_over_time({namespace=\"tas-llm-router\"} |= \"aiqg response event\" | json | tenant_id=\"$TENANT\" | (tag_aiqg_hallucination_hedge=~\"[1-9].*\" or tag_aiqg_repetition=~\"[1-9].*\") | unwrap total_cost_usd [5m])"

# Per-agent cost rollup — future /api/v1/metrics/agents (agent-flow attribution)
q "sum by (agent_id) (sum_over_time({namespace=\"tas-llm-router\"} |= \"aiqg response event\" | json | tenant_id=\"$TENANT\" | agent_id!=\"\" | unwrap total_cost_usd [15m]))"

# identity_source split (header / trace / inferred) — named vs guessed flows
q "sum by (identity_source) (count_over_time({namespace=\"tas-llm-router\"} |= \"aiqg response event\" | json | tenant_id=\"$TENANT\" | identity_source!=\"\" [15m]))"
```

Or open the dashboard (`aiqg.tas.scharber.com`) as `aiqg-demo` and watch
the panels populate over the last 15m–1h.

## How it stays in sync with production

- **Scores & cost**: computed by importing `pkg/clear` — no duplicated
  scoring math. If the scorer or pricing changes, demo data tracks it.
- **Field shape**: the emitted keys mirror `pkg/aiqg/events/emitter.go`.
  The `tag_<id>` hyphen→underscore sanitization matches the emitter and
  the dashboard. If the emitter adds/renames a promoted field, update
  `synth.go`'s field map and `catalog.go`'s matcher list.
- **Avoidable categories**: `avoidablePatternIDs` in `synth.go` mirrors
  the four categories in `aiqg-dashboard-be/internal/handlers/avoidable_cost.go`.

## Notes

- **TimescaleDB**: a fresh demo tenant has no TimescaleDB rows, so the
  CLEAR/cost/latency panels fall back to Loki (where this data lives).
  The tag and avoidable-cost panels are Loki-only regardless. No Kafka,
  Spark, or TimescaleDB writes are needed.
- **Backdating**: each pass spreads events across the last `--spread`
  seconds. Loki rejects very old timestamps by default, so generate near
  "now"; use `--interval` to build history forward over time.
