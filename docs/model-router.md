# Model Router

`internal/llm` is the only approved backend boundary for direct LLM provider calls.
Callers pass a semantic slot such as `classification-light` or `chat`; the router
resolves the active `model_slot_binding` row, enforces endpoint privacy, checks the
slot budget, normalizes params, invokes the private provider client, and records a
`model_invocation` row for every attempt.

Flowbee's deterministic core still must not import this package. Production agent
execution through `flowbee work`, `flowbee fleet`, and `flowbee up` enters the
router at `internal/worker.runAgentHeartbeatIO`: the worker passes an
`llm.AgentCommand` payload to `llm.Call`, the router resolves the slot binding,
enforces policy, invokes the private CLI provider, and ledgers the attempt.

## Seed Inventory

The production inventory in this checkout is limited to Flowbee's agent-backed
LLM paths. Russell's historical OpenRouter call sites
(`defaultEntitySynthesisModel`, inputflow policy defaults, quicktask vision,
embeddings defaults, and similar constants) are not present in this repository, so
they are not represented by guessed or closest-lane seed rows. The full V1 slot
taxonomy is still validated in code and schema, but slots with no current direct
backend provider call are intentionally left unseeded until their concrete
production assignment is imported or implemented. Missing bindings fail closed
instead of falling back to a hardcoded model.

| Existing call site | Existing model constant or runtime lane | New slot key | Seeded `model_id` | Seeded provider pins | Seeded params | Expected behavior change |
| --- | --- | --- | --- | --- | --- | --- |
| Worker build/author/resolution harness | Sonnet build lane from `flowbee fleet` / `flowbee up` | `drafting-complex` | `sonnet` | required `anthropic`, routing disabled | `{}` | none |
| Worker review harness | Opus review lane from `flowbee fleet` / `flowbee up` | `judge` | `opus` | required `anthropic`, routing disabled | `{}` | none |

If a later import brings Russell's direct OpenRouter call sites into this repo, add
a new migration from that concrete inventory before those calls are routed through
`internal/llm`; do not seed placeholders for absent call sites.

## Budget Accounting

Budget enforcement uses exact ledgered month-to-date spend from `model_invocation`.
The router does not invent a pre-call estimate when the request lacks one; it hard
stops once ledgered spend reaches the slot cap, and records successful provider
usage after the call. This preserves current provider defaults and avoids changing
model behavior by adding synthetic token estimates.

## Benchmark Gate

`model_slot_binding.benchmark_verdict_ref` stores the verdict that justified an
active binding when evidence exists. `llm.ValidateBenchmarkGate` allows a row
update that keeps the effective model/provider/prompt tuple unchanged, but only
when the candidate slot matches the binding being updated. A tuple change requires
a fresh passing `model_benchmark_verdict` for the same candidate slot/area.
Nightly incumbent smoke results can be recorded in `model_incumbent_smoke`; the
table has no trigger or path that mutates active bindings.
