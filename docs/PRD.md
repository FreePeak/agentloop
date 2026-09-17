# agentloop — Product Requirements Document

**Status:** draft for review · **Version:** 0.3.0 · **Date:** 2026-09-18
**Repo:** `github.com/FreePeak/agentloop` (branch `docs/prd-agentloop-service`, no commits yet)
**Canonical architecture:** [`design.md`](../design.md) — this PRD is the status/scope SoT and summarizes its decisions; it never duplicates its detail.

> The product is a **service and a contract**, not an app: agentloop runs bounded, budgeted, observable **agent loops** on behalf of other products, and the loop is bounded and metered by construction rather than by convention. Architecture source: *The 0→1 Loop Engineering Playbook (2026 Edition)* (ch. 1–13, App. A–G), applied with one rule — **every number in the book is a prior to calibrate against our own evals, never a spec.** The book's own review says its thresholds are asserted, not derived; §11 makes the calibration loop the product.

---

## 1. Problem & goals

A request–response service cannot do work where the next step depends on what the current step discovers — debugging, research, multi-tool operations. The naive LLM loop that fills the gap fails expensively: infinite iteration, cost explosion, drift, and *success that is expensive and wrong* (`design.md` §1).

**agentloop** is one Go service that takes a goal + budget and returns an answer, a structured handoff, or an explicit escalation. What it sells is not the loop; every team writes one in a week. What it sells is the **containment and the measurements around it**: a hard ceiling, an idempotent write path, a per-action meter, a trajectory you can re-run, and an eval suite that gates every change.

The book's domain chapters say this explicitly (Ch.14–17): in coding, research, business-process and creative agents alike, *the bottleneck is never generation — it is context management, verification and the integration surface*. A service that only calls a model is a wrapper; the parts that pay are the context budget, the verification stage and the audit trail. That, not the ReAct loop, is what v1 builds and what the licence of the code should reflect.

### 1.1 Goals

| # | Goal | Playbook ref |
|---|---|---|
| G1 | One canonical loop: `observe → reason → act → evaluate`, with **evaluation as a distinct phase** and an explicit owner for exhaustion (`escalate_to_human`) | Ch.1 (*the cycle is observe → reason → act → evaluate*), P1, P100 |
| G2 | Two execution modes — **ReAct** for unpredictable steps, **Plan-and-Execute** for structured multi-step work — with a **hybrid default** (plan per phase, ReAct inside a phase) | Ch.4's six failure modes, Ch.5 (*replanning after every step is essential, not optional*), P12 |
| G3 | **Production by construction:** bounded loop + kill switch non-optional, idempotent writes, cost circuit breaker, traces + replay, eval-gated deploys | Ch.10–13; P1 + P75 (the two patterns the index marks unconditional), P3, P26 |
| G4 | **Every number is a calibrated prior.** Model/step/cost/routing defaults ship as data with provenance; an eval run validates them | the report's *Numbers to know* table — and its "where to discount" section, which says the thresholds are asserted, not derived |
| G5 | Costs are **metered per action and enforced before the action**, not discovered on the invoice | Ch.13 (*cost is the silent killer of agent projects*: $150k/mo bill of a "cheap" $0.50-per-run agent), P3, P76, P82 |

The book's one law is adopted verbatim as the design law: every additional step **multiplies** cost, latency and failure probability. Therefore `MAX_STEPS` and `cost_budget` are economic instruments set per task type, not round numbers picked out of habit (`design.md` §1, §4).

### 1.2 Non-goals (v1, explicit)

- **Not** a model trainer or a fine-tuning pipeline.
- **Not** a new agent framework: model/tool SDKs sit behind `AgentBase`; the HTTP contract (App. C §"one abstraction layer, then swap implementations only within 3% on the same suite") is the moat, not the loop code.
- **Not** a domain prompt library: domain behavior ships as **versioned config** (`template`, `TemplateVersion`) using the eight App. G shapes, so a template change is reviewable and eval-gated like code.
- **Not** multi-tenant SaaS in v1 (single-tenant deployment; §7.4 names the seam).
- **Not** a chat product. The HTMX console is an operator surface, never an end-user UX.

### 1.3 Success criteria (v1)

| Metric | Target | How measured |
|---|---|---|
| Loop containment | **0** runs exceeding `max_steps`, `cost_budget`, or wall-clock; 0 duplicate side-effect writes without an idempotency key | §11.2 cases 1–4, §8 duplicate-write test · book: P1 (*every loop has a maximum step count. No exceptions*), P75 |
| Partial-answer honesty | 100% of budget-exhausted runs return a *labelled* partial synthesis, never a silent empty result | §11.2 case 1 · book: P3 (*halt and return the best result so far*) |
| Cost vs. naive baseline | ≥40% lower cost per completed task at eval parity (budgeted target; the book's prior is 40–70% from routing alone) | §11 eval A/B, §12 knee table · book: Ch.5 (Opus plan / Sonnet execute / Haiku replan ≈50% cheaper) |
| Reliability | ≥98% on the first use case before a second lands (book: reliability before features) | §11.4 deploy gate · book: Ch.18 (*a 95% success rate means 1 in 20 users has a bad experience*) |
| Eval coverage | 50 cases across 4 categories day one; +10/week from production incidents | §11.4 REFINE loop · book: Ch.10 (*evals before agent: 50 cases day one*); P99 (*each thumbs-down becomes an eval case*) |
| Cost discipline | per-run + daily ceilings enforced at 90% with forced synthesis; the service never aborts at 100% | §12.2 BudgetGuard · book: P3, P83 |

## 2. Framework mapping (App. A)

The playbook's five named frameworks are used as **names for phases we already run**, not as new subsystems. The book's own one-line instruction is the rule of use (App. A): *use it to name the phases of a loop you already run* — LOOP for quality, AGENT for side effects, CHAIN for decomposition, REFINE for iteration on a draft, SCALE for the platform underneath.

The diagnostic attached to it is the reason the mapping earns its place in a PRD: a loop whose **answer** is wrong needs LOOP/REFINE (re-reason, or refine the output); a loop whose **path** is wrong needs CHAIN/AGENT (decompose differently, change what it is allowed to touch); a loop you **cannot operate** needs SCALE (the platform work in §§6–12 of `design.md`). Three symptoms, three different next commits — and the most common mistake the book names is treating a SCALE symptom (no traces, no budget) as a prompt problem.

| Frame | Where it lives in agentloop |
|---|---|
| `LOOP` (Listen-Orchestrate-Observe-Perfect) | the default single-agent run path |
| `AGENT` (Assess-Navigate-Generate-Execute-Track) | runs with real side effects (writes, deploys, payments) — approval gates on |
| `CHAIN` (Chunk-Hypothesize-Act-Inspect-Next) | Planner phases; the decompose/debug path |
| `REFINE` (Review-Evaluate-Fix-Iterate-Narrow-Export) | the eval loop, and self-correction on high-stakes outputs only (≤2 rounds) |
| `SCALE` (Separate-Cache-Async-Log-Evaluate) | the platform layer: §§6–12 of `design.md` |

## 3. Architecture summary

Full detail and the request flow live in [`design.md`](../design.md) §3–§6. The shape below is Ch.3's control-loop model made concrete: **sensor** (memory + tool results), **controller** (router/planner/executor choosing the model tier), **actuator** (the tool registry), **feedback** (the evaluate phase), **termination** (the guard set). Ch.3's own summary of the chapter is the design brief for this section — *think in trajectories and convergence, not inputs and outputs*; trajectory divergence is the failure mode the guards exist for, and state compression is what stops the loop degrading as it runs. Summary:

```
 submit goal ──► Router (cheap tier) ──► Planner (strong tier) ──► Executor pool (ReAct + ToolRouter)
                     │                        │                             │
                     └──────── State + Memory (working/short/long/procedural) + checkpoints + audit ────────┘
                     │                        │                             │
                 ApprovalGate (fail-closed)  BudgetGuard            Tracer / Replayer / EvalRunner
```

**Components** (responsibility → playbook ref): Router (task classify + tier assignment) → Ch.5/13; Planner/Executor/Replanner (mutable plan object; ReAct inside; binary replan check) → Ch.5; ToolRegistry (5–15 visible tools, schema validation, sandbox, audit, 2,000-token result cap) → Ch.6; LoopRunner (bounds, dedup hash, cycle detector, budget pre-check at 90%, forced synthesis) → Ch.4; MemoryStore (4 tiers, 70% rule, landmarks, deletion API) → Ch.8; ApprovalGate (fail-closed policy table, timeout denies) → Ch.9; EvalRunner (4-category suite gating deploys) → Ch.10; Tracer/Replayer (one platform, nested spans, 3 replay modes) → Ch.11; Resilience (backoff → fallback chain → self-correct → breaker → degrade → escalate) → Ch.12; BudgetGuard (per-run + daily, per-action check) → Ch.13.

### 3.1 Stack decisions (draft — pending review, see §15)

The book's recommendation is explicit and it is *not* "write it in Go": for a **production** system, start custom rather than framework-first (Ch.7), wrap frameworks in an abstraction layer so they can be swapped (App. C), and migrate only when the replacement scores within 3% on the same eval suite. It also warns where the framework money goes: LangChain/LangGraph/CrewAI "add debugging complexity" in production.

That is what the decisions below implement — `AgentBase` is the abstraction layer, onegw is the model transport we refuse to rewrite, and the parts the book says are load-bearing (bounds, budgets, traces, eval) are ours. The user's stated direction is **Go service + HTMX UI**; `design.md` §16 left the runtime open. The PRD takes the direction as decided so the build order can be planned, and records the trade-off:

| Layer | Decision | Why / cost |
|---|---|---|
| Service | **Go 1.25**, `net/http` mux with method+pattern routes, `CGO_ENABLED=0` single binary | matches the rest of the portfolio (onegw, xdev, LeanKG) and the deployment envelope; the book is runtime-agnostic (its code is Python pseudocode), so this is a portfolio decision, not a book one — stated that way instead of dressed up as technique |
| Loop | Go goroutine pool + `context` deadlines; one goroutine per phase, `errgroup` for fan-out | implements P11 Parallel Loop (60–80% wall-clock cut on independent work) and Ch.5's 40–60% phase-parallel figure; replaces `asyncio.gather` with the same semantics plus explicit cancellation |
| Models | talk to **onegw** (OpenAI-compatible `/v1/chat/completions` + `/v1/messages`) | already the portfolio's LLM gateway: combo fallback chains, token savers, usage/cost rollups, per-key pools — none of which agentloop should re-implement |
| Tiers / routing | **onegw combos**, not agentloop code (`planning`, `execution`, `tiny`, plus a fail-open combo); the model list comes from `GET /v1/models` | implements *route models by task type* as gateway config instead of our code; App. B's `Model Tiers` pattern = this plus agentloop's per-step `task_type` label. Caveat: onegw has no per-step routing decision today (its issue #44), so agentloop picks the combo per step from its own versioned, eval-gated table and lets onegw route *within* the tier |
| Prompt cache / token saving | **onegw `[saver]`** (inject + external compress), not agentloop code | the provider-side prompt cache covers the static system+tools prefix; onegw's savers are the gateway-side half |
| Persistence | SQLite (WAL) for runs/checkpoints/evals/audit; pgvector when a tenant needs it | implements P8 Checkpoint Loop (durable state every 3–5 steps) and P42 Memory Versioning (state replay for debugging); LeanKG sets the single-binary precedent |
| UI | **HTMX over server-rendered templates**, no CDN | matches onegw's admin console discipline; the console's job is P91 Progressive Disclosure (summary first, evidence behind a disclosure) and P94 Explanation Mode — both of which are just markup, so a client-side framework would buy nothing |
| Cost metering | agentloop computes per-span cost from a **versioned price table**; onegw usage rollups cross-check it | P3 Cost Circuit Breaker is an *enforcement* pattern (halt at the threshold and return the best result so far) — a gateway that reports after the fact cannot enforce it, and Ch.13's own 100× price range means the table has to be versioned config, not a constant in code |
| Code intelligence | **LeanKG** over HTTP: ladder + graph verbs via `POST /api/v1/query`, memory via `/api/v1/memory/banks/{bank}/memories` | implements P33 Semantic Recall and P32 Landmark Memory over a real graph instead of re-embedding files: Ch.8's “large codebases are navigated with search plus selective retrieval, never full-context loading” only holds if the retrieval layer can answer structure questions (`impact`, `callers`, `context`), which a vector store cannot |

**Runtime risk, stated honestly:** the book's code shapes (asyncio fan-out, Python SDK tool-use) do not port line-for-line; Go buys the deployment envelope and the portfolio's operational habits, and costs the SDK's reference implementations. Two things make this a recorded decision rather than a preference. First, the book itself disagrees with "start custom" in Ch.2 — its landscape chapter says *past a 30% workaround share, go custom*, and App. C scores frameworks on a 12-dimension matrix instead of dismissing them. We are past that share: the portfolio already maintains the three services this loop binds to. Second, the trade is only defensible because §11 (evals) measures it — if Go's loop plumbing delays the eval suite past milestone 2, the decision is wrong and gets revisited in §15.

## 4. Tool-use engineering + v1 integration surface

Five tools is not an arbitrary small number; it is the book's own band. App. B puts the tool surface at **5–15** (P16 *Tool Router*, P23 *Tool Discovery*, P24 *Tool Doc Injection* for registries past that), and Ch.6's anti-pattern is *tool count explosion*: every tool past 15 dilutes selection and at 30+ "you will see agents using tool_17 when they should use tool_3". A 5-tool v1 therefore sits at the bottom of the band on purpose — the surface grows only when an eval shows the loop needs something it does not have, never because a tool was cheap to add.

Rules (Ch.6, `design.md` §5): typed envelope `ToolResult(success, data, message, metadata)`; errors typed and *actionable* (`timeout after 30s, try a simpler query`); name + `USE WHEN` + `DO NOT USE WHEN` + one example (the `DO NOT USE WHEN` clause is the highest-ROI prompt hour); 5–15 visible tools, router when the registry is larger; every write has a read twin; schema-validate before execution; result capped at 2,000 tokens; audit every call.

`design.md` §16 asks "which tools ship v1". Answer: **five**, each pointing at a service that already exists in this portfolio — no new backend is built for any of them.

| # | Tool | Backing surface | Write? | Notes |
|---|---|---|---|---|
| 1 | `repo_search` | LeanKG `POST /api/v1/query` (`action` empty = L0→L3 ladder, or `search`/`element`/`fuzzy`/`semantic`) | read | `retrieval{rung,reason}` + freshness ride back in the result metadata |
| 2 | `repo_context` | LeanKG graph verbs `context`, `impact`, `callers`, `callees` (relative to a resolved element) | read | the AST-aware context-budget extractor (Ch.14: "AST-level extraction cuts context 60–80%") |
| 3 | `web_search` | onegw provider `kind = "searxng"` (`<name>/query`) | read | no separate search integration; results come back pre-formatted |
| 4 | `run_tests` | `xdev rpc` (JSONL over stdio) running in a **restricted** `--add-dir` workspace | **yes** (sandboxed) | the verification half of the write-test-fix loop (Ch.14, ≤3 attempts); never a raw shell tool in v1 |
| 5 | `write_file` | `xdev rpc` file tools | **yes** | read twin = `repo_context`; approval gate by policy (§7.3); idempotency key on every call (§4.2) |

**Why five and not forty (Ch.6 / App. B):** the book's rule is *tool quality determines agent quality*, and it puts the working set at 5–15 tools with a router past it. What it is protecting is not token cost but **selection accuracy**: past ~15 tools the model reaches for `tool_17` when it meant `tool_3`, and every schema is paid on every step. Five is the bottom of that band on purpose — it is a budget spent deliberately, and a new tool has to earn its slot (§14).

Deferred to v2, deliberately: a persistent memory tool (LeanKG memory-bank shape first, Ch.8 — the one deferral the loop genuinely feels), a human-task tool (Jira/Confluence), and a generic `bash`. A generic shell is the single largest blast radius in the tool surface and buys nothing the four tools above do not already cover.

### 4.1 xdev as the execution sandbox — contract

xdev is the harness this very session runs in; its `rpc` mode is documented as the embedder seam (`xdev rpc`, "JSONL-over-stdio, for embedders", `internal/rpc`), and it consumes onegw as a provider. The protocol is `{id-tagged command → correlated response + streamed agent events}` with `session/prompt`, `session/steer`, `session/followUp`, `session/abort`, `session/new`, `session/state`, `session/setModel` commands; framing and limits are asserted by a `ready` frame carrying `FrameLimit`.

**Alignment policy:** xdev's own model may be onegw-routed — including *through agentloop's* tier combos. That is allowed and useful for dogfooding, but the PRD makes one rule explicit: **agentloop never loops xdev's loop.** agentloop drives xdev only as (a) a tool executor and (b) an editor for a single already-planned step; it never delegates an unbounded goal. Loop count stays 1, and `max_steps` semantics stay agentloop's.

**MCP boundary rule:** tools are agentloop-owned wherever "own" is cheap — the first two LeanKG tools are plain HTTP clients over `POST /api/v1/query` with **no MCP client in the request path**. MCP client support (Agent. B P16–P30) is a v2 registry extension; it is also the likeliest place a hung child process stalls a loop, so it will ship behind the same watchdog and timeout the tool registry already gives HTTP tools.

### 4.2 Two idempotency layers, one record of truth

The book lists idempotent tools as the pattern standing between an agent and a duplicate-write incident on retry (P26, *"makes retries safe"*), and its advice is one pattern, one rule — here the collision is real and needs three layers untangled before the rule can be stated. A real collision to settle: onegw already ships idempotency (`internal/idempotency`, `Idempotency-Key` or an `X-Request-Id` fallback) and xdev ships a per-call permission policy. Neither is sufficient, for a stated reason.

| Layer | Semantics | Why it is not enough for a loop |
|---|---|---|
| onegw request dedup | short TTL, LRU-bounded, streaming bodies never recorded; coalesces an in-flight retry or replays a recorded non-streaming response | stops a double-burn on a retried *call*; does not survive its TTL, and cannot tell the loop "this write already happened" |
| xdev permission policy | per-call approval in the sandbox | decides *whether* an action may run, never whether it already ran |
| **agentloop idempotency** | `sha256(run_id + tool + canonical(args))`, persisted **before** execution with the outcome, consulted before the call, stable across restarts | the durable record; the two above are defence in depth |

Rule: a write is keyed, checked and recorded by agentloop. Gateway dedup and sandbox permission may both fire first; neither can substitute.

### 4.3 Separation of powers (non-negotiable)

The reason this table is non-negotiable comes from the book's architecture chapter, not from taste: every layer here is one the agent may *talk about* but must never *move*. A model that can lower its own step ceiling, a sandbox that decides its own budget, a gateway that reports spend after the fact — each is a control that the controlled component can relax. The book's ordering principle is that control loops are only as strong as their weakest enforcement point, so each concern is pinned to the component that cannot be talked out of it by retrieved text or by the model's own plan.

| Concern | Owner | Rule |
|---|---|---|
| Enforcement (bounds, budgets, approval, kill) | **agentloop** | never delegated to the model, the sandbox, or the gateway |
| Model routing / token saving / fallback | **onegw** | agentloop sends the tier (`planning`/`execution`/`tiny`) per step; onegw picks the leg, saves tokens, records usage |
| Execution + file mutation | **xdev rpc** | sandboxed, `--add-dir` restricted, timeout- and watchdog-bounded, audited by agentloop |
| Knowledge retrieval + memory | **LeanKG** | the only place that owns the code graph and long-term recall |

## 5. Requirements

### 5.1 Functional (v1)

- **FR-1 Runs API** — submit a goal with `context`, `template?`, `max_steps?`, `cost_budget?`, `confirmations?`, `idempotency_key`; receive a run handle; poll/stream status. *(P92 Status Updates: the book claims 10× longer waits are tolerated when progress is visible — this is why the API streams instead of returning a single blocking response.)*
- **FR-2 Bounded loop** — enforce step ceiling, wall-clock ceiling, per-run and daily dollar ceilings, confidence floor, progress-stall detector, max-consecutive-failure ceiling; every exit is logged with its reason. *(P1 Bounded Loop is one of the two patterns the book's index marks unconditional — "every production agent"; P2 Early Exit, P4 Convergence Check, P5 Oscillation Detector, P8 Checkpoint Loop and P15 Adaptive Step Limit are its companions here.)*
- **FR-3 Hybrid planning** — Router classifies; Planner emits 3–7 one-sentence steps with success criteria and dependency marks; Replanner runs after *every* surprising step (binary `CONTINUE`/`REPLAN`), not only on error. *(P12 Conditional Loop: classify first — simple gets ~3 steps and a cheap model, complex gets ~15 and a premium one; P11 Parallel Loop cuts wall-clock 60–80% on independent sub-tasks.)*
- **FR-4 Loop guards** — dedup by `tool+canonical(args)` hash before execution; cycle detection at **3** identical `(tool,args)` pairs; unknown tool → typed error observation listing available tools; one tool call per ReAct turn. *(Ch.4: the Thought step is mandatory — skipping it costs 20–30% more tool-call errors for ~50–100 tokens, the cheapest trade in the book; Ch.1/9: confidence below 0.7 routes to a human.)*
- **FR-5 Tool registry** — schema validation pre-execution, per-tool timeout, sandbox, audit record, 2,000-token result cap with summarize/truncate middleware, per-tool fallback rungs (full/reduced/minimal/unavailable). *(P19 Tool Validation catches ~80% of tool-call errors before the API call is paid for; P29 Tool Result Summarization saves 60–80% of context tokens; P17 Tool Fallback, P20 Tool Caching 15–30% hits, P21 Tool Rate Limiter, P22 Tool Sandboxing, P28 Tool Health Check.)*
- **FR-6 Memory** — four tiers with the 70% context rule, landmark retention (decisions, recoveries, expensive outputs), rolling compression every 5 iterations, structured state with validation, **deletion API** for tenant/PII removal. *(P31–P45 are the book's memory band: P32 Landmark Memory keeps decisions verbatim, P34 Memory Compression frees 60–80% of context, P39 Context Budget is the 40/20/20/20 split, P40 Memory Eviction retains by value, P43–P45 are the typed state → validation → rollback chain.)*
- **FR-7 Approval** — fail-closed policy table (`read → auto`, `update → auto_if_confident`, `send/delete/deploy/pay → always_approve`, unknown → `always_approve`), 30-minute timeout **denies**, batching + fatigue guard (median approve <3s means a lost human). *(Ch.9: approval fatigue is worse than no approval — batch, tier, auto-approve the routine; ~30% of approval interactions are *modifications*, which the book calls the highest-value training signal in the system, so a Modify response is a corrected action, never a rejection; P30 Confirmation Tool, P68 Confidence Scoring.)*
- **FR-8 Idempotency** — fingerprint every write, check *before* executing, persist the key and the outcome; a retry re-reads the first attempt's result instead of re-firing. *(P26 Idempotent Tools: "one pattern standing between you and 2,400 duplicate refunds".)*
- **FR-9 Kill & degrade** — `POST /v1/runs/{id}/kill` halts in ≤1 step boundary and returns the partial synthesis; the degrade ladder answers with honest copy ("based on training data, may be outdated"), never silence. *(P75 Kill Switch is the second unconditional pattern — "every production system" — and the book's instruction is to test it quarterly; P100 Graceful Handoff is the honest-copy half.)*
- **FR-10 Traces & replay** — one trace per run, nested spans from the first commit, per-span tokens/cost/latency; replay in recorded / hybrid / live modes with a divergence flag (word-set similarity <0.9 flags divergence). *(Ch.11: "logs tell you what happened; traces tell you why" — the nested-span tracer is ~80 lines and "trivial to build, expensive to retrofit".)*
- **FR-11 Evals** — 4-category suite, scoring functions by type, pass = `score ≥ 0.8 ∧ latency ≤ cap ∧ cost ≤ cap`, CI gate, JSON report. *(Ch.10: the remedy for confident incorrectness; P61 Self-Critique catches 10–20% of mistakes for almost nothing, P62 Rubric Scoring, P64 Citation Verification, P65 Output Validation.)*
- **FR-12 Operator console** — HTMX pages: run list + trajectory viewer, live step feed, budget/spend rollups, approval queue, eval report, kill button ("if you cannot see the trajectory, you cannot tell convergence from an expensive wrong answer"). *(P91 Progressive Disclosure: summary first, evidence and reasoning trace behind a disclosure — most users never open them; P94 Explanation Mode presents the reasoning in plain language for the audit case; P92 Status Updates in the live feed.)*

### 5.2 Non-functional

| ID | Requirement | Target | There because |
|---|---|---|---|
| NFR-1 | Containment | 0 runs exceed any configured ceiling (enforced pre-action, tested) | P1/P3/P75 — the book's 20 × $0.05 × 10,000 users = $10,000/min runaway figure |
| NFR-2 | Latency | API p95 acknowledges a run in <300 ms; step latency p95 reported and baselined, not just the run | P92/P81 — 10× wait tolerance is bought with visible progress, not with a faster loop |
| NFR-3 | Memory | RSS bounded by construction (`debug.SetMemoryLimit` backstop, bounded queues/windows/output sinks), cell-checked on run state | portfolio envelope (onegw's ≤100 MB contract), not the book — the book assumes a server, we assume a box |
| NFR-4 | Durability | resume from a checkpoint after a mid-run fault without re-firing a write | P8 Checkpoint Loop (serialize every 3–5 steps; critical for 30+ min runs) + P26 |
| NFR-5 | Observability | every run replayable; every alert threshold from Ch.11 wired to one dashboard | Ch.11's five pillars with traces as the "why"; one platform only |
| NFR-6 | Portability | `CGO_ENABLED=0` single binary; loop code trafficks only through onegw and (v1) the 5-tool registry | App. C: one abstraction layer, then swap implementations only within 3% on the same suite |
| NFR-7 | Trace retention | 90 days; thresholds re-derived monthly (§11.5) | book prior; onegw's own `usage.retention_days` default is also 90 |

## 6. API & data contract (v1 sketch)

`design.md` §13 is the contract of record; the table below is that contract, kept here because the API is what consumers integrate against:

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/runs` | submit `{goal, context, template?, max_steps?, cost_budget?, confirmations?, idempotency_key}` → `{run_id}` |
| `GET` | `/v1/runs/{id}` | status, trajectory, spend, spans |
| `POST` | `/v1/runs/{id}/approve` · `/modify` · `/reject` | human decision on a paused gate; timeout denies |
| `POST` | `/v1/runs/{id}/kill` | the kill switch (P75) — tested quarterly, and by every deploy smoke test |
| `GET` | `/v1/runs/{id}/events` | SSE step feed and webhook delivery for runs >5s (P92: *"Searching 3 databases…"* — the book's claim is 10× longer waits tolerated when progress is visible) |
| `GET` | `/admin/api/v1/*` | console reads: runs, traces, budgets, evals, approval queue (paths follow onegw's console convention so both services read the same way) |

Stores: runs+checkpoints (SQLite/pgvector-ready), traces/spans (one platform, Langfuse self-hosted as the default backend), exact+semantic caches (in-process + Redis when shared), eval cases + golden sets, audit log (append-only). Every span carries `(in×p_in + out×p_out)` at a **versioned price table** so BudgetGuard and the console agree to the cent.

## 7. Trust boundaries, security, HITL

### 7.1 Inbound (untrusted → service)
Bearer-token auth on every route (mirror LeanKG's role model: admin/contributor/viewer), per-tenant quotas, body caps, and **validation at the boundary**: goal length, `context` size, template existence, numeric bounds on `max_steps`/`cost_budget`. `idempotency_key` is required for any run whose plan can write.

### 7.2 Retrieved content (untrusted → model context)
Prompt injection is the tool path's default failure mode: sanitize fetched content, strip instruction-like patterns (blocklist + embedding-similarity check), and never let a tool result change the policy table or the budget. Tool output is data; the only thing that may act on it is the loop, under policy.

### 7.3 Egress (loop → world)
Destructive and irreversible actions sit behind `confirm_action` with a preview; writes carry idempotency keys; the sandbox is `--add-dir`-restricted; every call is audited with its arguments. A rate-of-change guard pauses the run on anomalous mutation bursts.

### 7.4 Tenant data
v1 is single-tenant, but PII redaction is applied **after** action (with rollback on side-effect surprise) and the memory deletion API is part of the contract, because retrofit deletion is how PII work always goes wrong. Per-tenant budgets and isolation are §15 open decisions with the seam named (LeanKG's org/resource-claim model).

### 7.5 HITL
The four HITL patterns are all adopted, because the book is blunt about which single failure makes them pointless: **approval fatigue is worse than no approval** (Ch.9). An operator who rubber-stamps 40 gates a day is not a control, they are a latency. Hence gates, confidence thresholds, audit trails and progressive autonomy (supervised → semi-auto → autonomous, ramped by *measured* approval rate, not by time). Target: interrupt <10% of the time while catching ~95% of errors; sample auto-approvals; flag rubber-stamping. Approvals are training data for the day that gate is automated.

## 8. Error taxonomy & recovery (Ch.12)

Five categories, with engineering effort proportional to *time-to-detect*, not to frequency:

| # | Category | Detection | Recovery |
|---|---|---|---|
| 1 | Transient (timeout, rate limit, blip) | exception type/status | retry with backoff + jitter; **never** blind-retry 400/401 or a write |
| 2 | Tool (bad args, unavailable, format drift) | validation + parse | arg correction, then the fallback chain, logging each rung |
| 3 | Model (hallucinated tool, refusal, contradiction) | schema/output validation | re-prompt with the tool list, or rephrase once; no silent retry of an identical prompt |
| 4 | State (overflow, stale, lost) | state invariants | checkpoint rollback + context refresh |
| 5 | Logic (wrong plan, circular, scope creep) | trajectory analysis | plan revision, then escalate |

The five recovery strategies map onto the loop as: retry the failure → fall back if it repeats → self-correct on a tool or model error → circuit-break on a state or logic error. The numbers (Ch.12 priors, calibrated per §17): backoff base 1.0 s doubling to a 60 s cap, jitter ×[0.5,1.5], max 3 attempts; 429 ×5 honouring `Retry-After`, 500 ×3, timeout ×2 with a smaller input, **never** 400/401; breaker 5 failures open / 60 s recovery / 2 clean half-open probes (per-tool 3 / 30 s); self-correction ≤2 rounds, since a second critique pass triples that step's cost.

**The duplicate-write test is the one that matters** (§11.2 case 2): point a write tool at a recorder, kill the process between the mutation and the response, resume the run, assert exactly one mutation. That is the difference between an idempotency key and a comment.

Circuit breaker per tool, half-open probes, single failure reopens. Escalation always ships the packet: what happened / what was tried / what is needed / the full trace.

## 9. Loop invariants (testable)

These are not preferences. The book's one-line creed for production loops — *max steps, observability, recovery, cost, monitoring* (Ch.3) — is a list of properties, and properties only hold if something makes them impossible to violate. Each invariant below names the mechanism that enforces it, so a reviewer can ask "what fails if this is violated" and get an answer that is not prose.

New invariants from this PRD, beyond `design.md` §4:

Every default in §17 is a calibrated line item, not a book assertion, and the reviewer contract is one line: **no default moves without an eval run, in either direction** — an optimist lowering a ceiling is exactly as dangerous as a pessimist raising one.

1. **No progress means no spend:** if two consecutive steps produce no new state, the run is terminated or replanned — never continued.
2. **The evaluation phase is not optional:** every step ends with an evaluated predicate, even when the predicate is "not yet".
3. **One writer:** at most one in-flight write per (run, resource); parallel phases may fan out reads only.
4. **Ceiling semantics are pre-action:** the budget check happens *before* the call that would exceed it, and a partial synthesis is reserved before it is needed.
5. **Loop 1 only:** no agentloop-run recursion, and no delegation of an unbounded goal to another agent runtime (§4.1).

### 9.1 Known ceilings to cut at scale (marked in code)

Per convention, deliberate shortcuts ship with a `ponytail:` comment naming the ceiling and the upgrade path. v1 accepts three:

1. **Run registry:** single-process, in-memory + SQLite; one shared mutex guards state transitions. Ceiling: one agentloop process per host. Upgrade: a Postgres advisory-lock run table with a worker pool, the moment a second replica is wanted.
2. **Cost accumulation:** O(n) sum over a run's spans at the 90% check (n is small). Ceiling: O(n) per step. Upgrade: a running counter on the run row once n > 500.
3. **Retention sweep:** a periodic full scan of the trace table for 90-day expiry. Ceiling: O(rows) nightly. Upgrade: an indexed `expires_at` delete when the table passes ~10M rows.

## 10. Multi-agent stance

**Single agent in v1.** The book's own gate is adopted literally: add agents only for *10+ distinct tools, mixed model tiers, genuine parallelism, or context beyond one window* — otherwise the channel tax (`N(N−1)/2`) eats the win. When agents land (milestone 7), the shape is fixed: hierarchical, teams of 3–4, typed messages (`task|result|question|feedback`) with per-receiver FIFO, a role card per agent in version control (name, model tier, tools, prompt, I/O format, failure behavior), disagreement by stakes (vote / arbitrate on a stronger model / escalate with a highlighted diff), and an explicit lifecycle — no zombies. Stop rule: coordination messages above 30% of tokens means we added too many.

What that means concretely is that App. B's whole multi-agent band (P46–P60) is **deliberately not adopted** in v1, and the reason is arithmetic rather than modesty: coordination cost grows quadratically with agent count (Ch.7), so the book's own gate is the only honest trigger — 10+ distinct tools, mixed model tiers, genuine parallelism, or context beyond a single window. We have five tools and one window. Two patterns are also rejected on merit even after M7: **P49 Ensemble** (3–5 agents voting) buys reliability at 3–5× compute, which is a trade the eval suite has to prove before we pay it, and **P55 Consensus** is reserved for irreversible decisions — our irreversible decisions go to a *human* gate (§7.5), not to a majority of models agreeing with each other.

## 11. Evaluation — the product (Ch.10)

The book's chapter on evaluation opens with the reason this section is not an appendix: agent evaluation is *fundamentally harder* than LLM evaluation, because a run is non-deterministic, has intermediate steps nobody scores, and changes the world on the way through. Public benchmarks (SWE-bench, HumanEval, GAIA, WebArena) give an industry baseline but cannot answer "did *our* change make *our* agent worse". So the eval suite is custom, owned, and a deploy gate — not a number quoted from a paper. That is also why §11 is where this PRD claims a moat rather than in §4's loop, which any team can write in a week.

### 11.1 Case taxonomy and gates

Four categories at their target shares — happy path 40–50%, edge 20–30%, adversarial 15–20%, regression 10–15% — because a strong happy-path number must never mask adversarial weakness. A case: `{input, expected, scoring_fn, max_latency, max_cost}`; **pass = `score ≥ 0.8 ∧ latency ≤ cap ∧ cost ≤ cap`**, with the trajectory attached to every result.

Scoring by type: exact, fuzzy, cosine, constraint-check, LLM-judge (stronger model, 1–10 → /10), test-pass-rate. The adversary in every suite is **confident incorrectness** (a plausible wrong answer at high confidence): the correct behavior is a hedge or a refusal, and it is scored as a pass.

### 11.2 The containment acceptance suite (milestone 1)

| # | Case | Pass |
|---|---|---|
| 1 | Runaway probe (a prompt designed to loop forever) | halts at the ceiling, returns labelled partial synthesis, records the exit reason |
| 2 | Repeated identical write | second identical `(tool,args)` never re-fires; the idempotency key returns the first outcome |
| 3 | Budget creep | at 90% spend the run forces synthesis; the "no progress = no spend" invariant ends a stalled loop |
| 4 | Kill switch under load | kill lands inside one step boundary on a run with an in-flight tool call |
| 5 | Injected prompt injection in retrieved content | policy table and budget unchanged; the injection is surfaced, not obeyed |

These run in CI as a 10-case suite completing in under two minutes, per the book's "an agent you cannot measure is an agent you cannot improve" rule.

### 11.3 A/B discipline and flakiness

Every config change runs the full suite with 3 runs per case, `p<0.05`, watching for adversarial regressions hiding under headline gains. Flaky cases get a 10× rerun, then a decision: temperature-0 or vote, mock the tool, loosen the scorer, or it is a real bug — never delete the signal. Budget: under 2 flaky cases per 50.

### 11.4 Deploy gate and the REFINE loop

`EvalRunner` runs in CI; a deploy is blocked on the full-suite gate; each case runs on a fresh agent with `max_steps=10, max_cost=$1.00` (the book's harness caps; our per-run defaults stay §17's 9 steps / $1.00) and emits a JSON report (pass rate, avg/p95 latency, avg cost, per-category, failures). Every production incident's **first** fix step is a new regression case (Record → Extract → Formalize → Iterate → Normalize → Expand), +10 cases/week.

### 11.5 Baseline calibration (the goal G4 loop)

The book's priors ship as defaults **with their source cited**, and a monthly job re-derives them from our own traces: `max_steps` from staging p95 completions × 1.3; cycle threshold from observed cycle rates; alert thresholds from the rolling baseline; cache similarity from measured false positives. Until a number is re-derived, it is labeled a prior in the code and the console.

## 12. Observability & cost (Ch.11, Ch.13)

Two book claims set the shape of this section. On observability: *logs tell you what happened; traces tell you why* — which is why nested spans start in the first commit even though a flat log is easier, since the book's own words are that nested spans are "trivial to build, expensive to retrofit". On cost: model pricing spans roughly a **100×** range and *choosing the right model per task is the highest-impact optimization available* (40–70% savings). Both are configuration problems, not features, which is why the three levers below are ordered by yield rather than by effort.

### 12.1 Five pillars, one platform
Traces (why) / metrics / logs / alerts / replays. **One** tracing platform (Langfuse self-hosted default), nested spans from day one — trivial to build, expensive to retrofit. Auto-analysis on every trace: cycle ≥3 → critical; spans >20 → grinding; cost >0.8×budget; one tool >60% of spans; ≥3 consecutive errors → critical; quality below baseline → review. Alert thresholds (priors to calibrate): success rate warn <93% / page <85%; latency p95 >10s/>30s; cost >2×/>5× baseline; tool errors >3%/>10%; budget 80%/95%. Debug protocol when something breaks: provider status first → segment failures (never read traces one by one) → diff 5 bad vs 5 good traces → tool health → fix + add a regression case. The console's first dashboard also carries the two metrics that justify the *service* rather than its health: **human-intervention frequency** and **acceptance rate**, plus savings against the manual baseline.

### 12.2 Cost model
`cost = (in×p_in + out×p_out) × iterations`, metered per span, enforced per action. Levers in order of yield: **routing by step type** (cheap tier for classify/extract/route, mid for reason/synthesize, strong only for judge/complex) → **prompt cache** on the static system+tools block → **response/tool cache** (exact hash; semantic only above a threshold validated against our own false-positive rate) → **correctness before cleverness**. Onegw already implements the first three; agentloop contributes the per-step routing decision and the budget enforcement, and the knee table (savings vs quality loss per incremental lever) is published in the console.

## 13. Roadmap, status & acceptance

The build order is `design.md` §15, kept 1:1 so there is one record, not two. The book's 30-day/8-week plan is a *schedule overlay*, not a second backlog.

| # | Milestone | Scope | Acceptance (from design.md §15, sharpened) | Status |
|---|---|---|---|---|
| M0 | Docs SoT | this PRD + `design.md` reviewed; decisions closed | §15 decisions have owners and dates; PRD footer stamped | **in progress** |
| M1 | Containment core | LoopRunner + ToolRegistry + BudgetGuard + kill switch + runs API + `AgentBase` | §11.2 cases 1,2,3,4 pass in CI | not started |
| M2 | Guards + tracing | dedup/cycle/validation/2K cap, Tracer (nested spans), cycle alert, replay v1 | 3-layer repetition test passes; an injected fault is found by diffing traces; §11.2 case 5 | not started |
| M3 | Planning | Planner/Replanner, parallel phases, tiered routing through onegw | a 5+-step task is ≥40% cheaper than single-tier ReAct at eval parity (±3%) | not started |
| M4 | Memory & state | 4 tiers, 70% rule, landmarks, checkpoints, deletion API | 20-iteration run holds the 70% rule; resume from step-5 checkpoint after a step-7 fault | not started |
| M5 | HITL | ApprovalGate, audit, progressive autonomy counters, approval queue UI | <10% interruptions; sampling + anomaly review exercised; timeout denies | not started |
| M6 | Evals & console | EvalRunner in CI, REFINE job, HTMX console (trajectory, spend, evals, kill) | deploys blocked on the full-suite gate; +10 cases/week; knee table published | not started |
| M7 | Multi-agent (conditional) | supervisor + specialists, typed bus, role cards | only after §10's gate is met; coordination <30% of tokens | conditional |

**Definition of done for M1–M6:** the milestone's acceptance passes in CI, the status column here is updated in the same commit as the work, and each acceptance becomes a named eval case — never a prose claim.

**Task record:** until this repo has commits and a reachable GitHub remote, this table *is* the tracker. Per the global convention, tasks then move to **GitHub issues in `FreePeak/agentloop`** (PRD keeps the status summary and references issue numbers); no `TASKS.md` is ever created.

**Schedule overlay** (from the book's 30-day plan, adapted — the book's week 1 "from-scratch ReAct with no framework" is subsumed by M1, and its weeks 5–8 become M4–M6):

| Week | Book's deliverable | agentloop equivalent |
|---|---|---|
| 1 | working ReAct agent + tracing | M1 containment core + M2 tracing start |
| 2 | 10-case eval + recovery | §11.2 suite + Ch.12 resilience |
| 3 | domain agent + demo | first real template (App. G row 2 or 4) on LeanKG/xdev tools. **Not taken from the book's week 3:** the 2-agent pipeline (M7), the semantic-cache cost target (v2), and the demo video — the console plus a published knee table is the artifact a service is judged on |
| 4 | portfolio / launch | console + gate in CI + a public README with the knee table |
| 5–8 | production wrapper, supervisor, 50-case eval, ship | M4, M5, M6, M7 |

## 14. Risks & mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| **Threshold cargo-culting** — the book's priors (0.7/0.85/0.95, 3 attempts) shipped as if derived | silent quality/cost regressions that look like config | G4: every prior carries its source; §11.5 monthly re-derivation; console labels un-calibrated priors |
| **Go runtime tax** — loop plumbing delays the eval suite | the product's differentiator (measurement) ships late | M6 gate is the eval suite in CI before any domain template; §15 revisits the runtime if M2 slips |
| **Auto-approval erosion** — gates approved without reading | agents get blanket permission harmlessly, then not harmlessly | timeout denies, <10% interrupt target, sampling, median-approve-time fatigue guard |
| **Semantic-cache false positives** — a cached answer served to a distinct question | confidently wrong answers at scale | threshold validated against *our* measured false-positive rate, per-template, never a copied default; cache only above the validated threshold |
| **Infinite loop with cost** — the loop bounds themselves are the failure | worst case: spend breaks the service | pre-action enforcement, kill switch tested every deploy, per-day ceiling independent of per-run |
| **Tool surface growth** — 5 tools becomes 40 | selection accuracy collapses, prompt cost grows | hard cap of 15 visible tools with a router beyond it; new tool requires a removal or an eval justification |
| **Portfolio coupling** — onegw/LeanKG/xdev become hard dependencies | their outages stop our loops | every tool has a fallback rung and honest degrade copy; the loop itself has no hard dependency beyond the model gateway |
| **Framework drift** — onegw's tiering/routing evolves | routing decisions silently change | routing is data (config) with a version; the eval gate catches behavior change |

## 15. Open decisions (owner + deadline)

| # | Decision | Recommendation | Owner | By |
|---|---|---|---|---|
| D0 | Who reviews and owns this PRD (no name in the file today) | the author signs §16 and turns D1–D7 into dated decisions | **you** | before any code |
| D1 | Runtime: Go vs Python | **Go** (§3.1) — the loop itself is stdlib-only, so the real question is the tools' SDKs; revisit if M2 slips | — | M0 exit |
| D2 | Tracing backend: Langfuse self-hosted vs OTel-only | Langfuse self-hosted; OTel exporter as a secondary sink | — | before M2 |
| D3 | Tenancy: single-tenant v1 vs isolation now | single-tenant v1, seam named (§7.4) | — | before M5 |
| D4 | `TemplateVersion` immutability policy | templates are immutable, named+hash versioned, and eval-gated on change | — | before M3 |
| D5 | xdev rpc protocol version to pin | pin `protocol.ProtocolVersion` and refuse a mismatch loudly | — | before M1 tool 4 |
| D6 | Whether agentloop may route through its own tiers when xdev calls it | allowed for dogfooding, never as a second loop (§4.1) | — | M1 |
| D7 | Long-term memory backend | LeanKG memory bank vs a dedicated store | — | before M4 |

## 16. Sources

- [`design.md`](../design.md) — the architecture of record: canonical loop contract (§4), tool-use engineering (§5), memory and state (§6), multi-agent gate (§7), HITL (§8), eval suite (§9), observability (§10), recovery (§11), cost (§12), API and data (§13), ship defaults (§14), build order (§15). This PRD summarizes it and points at it; where the two ever disagree, `design.md` wins on architecture and this file wins on scope/status.
- `loop-engineering-playbook-report.html` — full-book report (20 chapters, App. A–G): the numbers, thresholds, code shapes and pattern identifiers cited throughout, and the "where to discount" section that produced §18.
- *The 0→1 Loop Engineering Playbook (2026 Edition)*, Valenx Press, first edition June 2026 — Ch.1–13 and App. A–G; the App. G CONFIG table is the basis of §17's per-template budgets, and App. C's "within 3% on the same eval suite" rule is why §1.2 refuses to write a framework.
- Portfolio surfaces this PRD binds to, verified against their repositories: **onegw** (README tiering/combos, `[saver]`, `GET /v1/models`, `/admin/api/v1/usage/daily`; `docs/ARCHITECTURE.md` for the usage/cost rollups; `internal/idempotency`), **xdev** (`xdev rpc` JSONL-over-stdio, `internal/rpc`'s handler contract, `internal/serve` broker/gateway), **LeanKG** (`POST /api/v1/query`, the `query` MCP tool's action set, `/api/v1/memory/...` and its hindsight-compat aliases, the role model in `internal/auth`).
- Deep read of both source documents for this PRD was done on 2026-09-18; all 40+ numeric figures reproduced here were taken from the report's *Numbers to know* table and the PDF's chapter bodies, not recalled.
## 17. Appendix A — Defaults & calibration baseline

One table, one rule: **nothing in the middle column is a spec.** Every value is a starting prior with its provenance in the third column, and the fourth column is the only legitimate way it changes.

| Knob | v1 value | Source | Calibration |
|---|---|---|---|
| `max_steps` | **9** per run | book prior (6-step task + 30% headroom) | p95 staging completions × 1.3, monthly (§11.5) |
| Wall-clock cap | **120 s** per run, excluding approval waits | App. G wrapper prior | p95 run time × 1.3 |
| `cost_budget` | **$1.00** default; per template $0.03–0.08 (haiku-tier) / $0.20–0.50 (sonnet-tier) | App. G rows 1–8 | observed cost per completed task at eval parity |
| Daily ceiling | 20× the per-run budget, per tenant-day | derived | measured runs/day × p95 cost, plus headroom |
| Pre-synthesis reserve | **10%** of budget | book prior | the knee table (§12.2) |
| Dedup | hash `tool+canonical(args)` before execution; break after 2 identical in a row | book prior | prevented-waste rate, observed before loosening |
| Cycle alert | **3** identical `(tool,args)` pairs | book prior (≈18% saved spend at 5k+ runs/day) | our own cycle rate; a default, not a law |
| Context ceiling | **70%** of the window for state+history | book prior | measured degradation curve per model |
| Compression cadence | every **5** iterations; last 5 turns verbatim | book prior | measured recall loss, not a schedule |
| Tool result cap | **2,000** tokens (truncate at 4,000 chars, else summarize to 5 items) | book prior | compressible-token ratio measured by onegw's savers |
| Visible tool count | **5** in v1, hard cap **15** | book prior | an addition needs a removal or an eval justification (§14) |
| Retry | 3 attempts, base 1.0 s ×2, cap 60 s, jitter ×[0.5,1.5] | book prior | the observed transient-error distribution |
| Circuit breaker | open at 5 failures / 60 s recovery / 2 half-open probes; per-tool 3 / 30 s | book prior | per-tool error rates |
| Self-correction | **≤2** rounds, high-stakes outputs only | book prior | marginal quality per round |
| HITL interrupt budget | **<10%** of runs; ~95% of errors caught; 2% of auto-approvals sampled; median approve <3 s = rubber-stamping | book prior | the measured confusion matrix — the target is the catch rate, not the interrupt rate |
| Semantic cache similarity | **≥0.95**, and only after measuring *our* false positives (book: ~8% at 0.90, <1% at 0.95) | book prior | our own FP rate, per template |
| Autonomy ramp | first 20 actions supervised → semi-auto above ~0.85 approval over 50+ → autonomous above ~0.95 over 100+ | book prior | the tenant's own history only |
| Eval pass gate | `score ≥ 0.8` ∧ latency ≤ cap ∧ cost ≤ cap; deploys blocked below an ~85% suite pass rate | book prior | raise it as the suite matures, never lower it |
| Alert thresholds | success <93% warn / <85% page; p95 >10 s / >30 s; cost >2× / >5× baseline; tool errors >3% / >10%; budget 80% / 95% | book prior | rolling baselines (§12.1) |
| Trace retention | **90 days**; thresholds re-derived monthly; prod → eval dataset weekly | book prior | storage cost vs replay need |

Two warnings about this table. A value **is not calibrated because it has not broken yet** — the absence of an alert is not evidence. And the optimistic reviewer is the dangerous one: the knobs an optimist lowers (`max_steps`, a budget, a confidence floor) are exactly the knobs the containment suite (§11.2) exists to test.

## 18. Appendix B — Where to discount the source

The book's own review flags these; the deep read found more. Read this before quoting any number above.

| # | Discount | What this PRD does about it |
|---|---|---|
| 1 | **Thresholds are asserted, not derived.** The book states 0.85, 0.7, 0.95, 3 attempts, 2 revisions, a 10% interrupt budget as rules and never shows how to measure your own. | G4 + §11.5 + §17: priors carry provenance, and a monthly job re-derives them from our traces. Numbers we have not re-derived are labelled un-calibrated in code and console. |
| 2 | **Model names and prices are the book's expiry date** — per-call rates in App. B and per-1M rates in Ch.13 disagree inside the same book. | We copy no price: the price table is versioned config, reconciled against onegw's catalog and usage rollups. |
| 3 | **Attribution is decorative** — anonymous epigraphs; a demand-side "2026 State of AI Engineering" report that was never published. | Nothing here rests on a quotation. Every claim is either a labelled book number or a verified portfolio surface (§16). |
| 4 | **Safety gets a section, not a part** — injection blocklists and PII redaction sit inside Ch.12, thin for a book about autonomous systems. | §7 is its own section with named trust boundaries and containment case 5 (§11.2). |
| 5 | **Structure repeats** — 20 chapters on one template; appendices re-summarize the body. | We cite chapters instead of restating them; the PRD summarizes `design.md`, which summarizes the book. |
| 6 | **The code is Python pseudocode** — decorators, `asyncio.gather`, a shipped 80-line tracer. | §3.1's Go translation is an assumption with a named risk (§14), never presented as a port. §11 settles it empirically. |

What survives the discount, and why the playbook was applied at all: the loop-level **patterns** — bounded loop, kill switch, idempotency on writes, model tiering, context ceilings, five-pillar observability, the four-category eval taxonomy, and *verify, don't generate harder*. None of them depends on a number being right, and each fails safe when it is wrong — which is the property a first version needs.

---

*Last updated: 2026-09-18 (v0.3.0 loop 2 — no orphan assertions left: §3 states the book's own recommendation and where we disagree with it (Ch.2's 30% rule, App. C's 3%), §4.1 says why the surface is 5 and not 40, §4.2 names the failure P26 prevents, §4.3 explains why enforcement cannot sit with the model, §7.5 leads with approval fatigue, §9 pairs every invariant with the mechanism that enforces it, §10 explicitly rejects P46–P60 and says why P49/P55 stay rejected, §§11–12 say what the book's two claims actually buy.

*v0.2.0 loop 1 — every load-bearing number now cites its source: goals carry pattern ids (P1/P75 unconditional), FRs carry the pattern band and the number behind them (P19 80% of tool errors, P29 60–80% of context tokens, P34, P39's 40/20/20/20, P12, P92), NFRs gained a "there because" column, the success criteria cite both the book's claim and our test.

*v0.1.1 review pass — self-reviewed against both source documents and the onegw/xdev/LeanKG surfaces; fixed dead cross-references and a superseded pointer; added the two idempotency layers and their record of truth (§4.2), the duplicate-write test and the Ch.12 recovery numbers (§8), Appendix A's calibration baseline table (§17), and Appendix B's extended discount of the source (§18); §16 re-headlined with the verification basis; D0 added for review ownership).*
