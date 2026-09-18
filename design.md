# agentloop — System Design

> Greenfield loop-runtime service. Applies *The 0→1 Loop Engineering Playbook (2026)* — read cover to cover — as its architecture source: canonical `observe → reason → act → evaluate` cycle, ReAct + Plan-and-Execute hybrid, tool-use engineering, memory tiers, HITL, Part IV production engineering, Part V domain tracks, and the Ch.18 product layer.
> All numeric thresholds below are **priors from the book to calibrate against own evals**, not specs.

## 1. Problem & goals

**Problem:** request–response services cannot do work where the next step depends on what the current step discovers (debugging, research, multi-tool ops). Naive LLM loops fail expensively: infinite iteration, cost explosion, drift, silent wrong answers.

**agentloop** is a single service that runs **bounded, budgeted, observable agent loops** on behalf of products. Products submit a goal + budget; agentloop plans, executes tools, evaluates, and returns an answer or a structured handoff.

Goals:

1. One canonical loop (Ch.1): `observe → reason → act → evaluate`, repeat until goal met or a guard fires. Evaluation is a distinct phase; exhaustion has an explicit owner (`escalate_to_human`).
2. Two execution modes (Ch.4–5): **ReAct** for unpredictable steps, **Plan-and-Execute** for structured multi-step work, **hybrid** by default (plan for phases, ReAct inside phases).
3. Production by default (Ch.10–13, App.B): bounded loop + kill switch non-optional, idempotent writes, cost circuit breaker, traces + replay, eval-gated deploys.
4. Cost law (Ch.1): every extra step **multiplies** cost/latency/failure. `MAX_STEPS` and `cost_budget` are economic instruments set per task, not round numbers.
5. Loop only when justified (Ch.1 KEY INSIGHT): if the flowchart is drawable with all branches known upfront, ship a pipeline — a loop is for branches that depend on what the model discovers at runtime.

Why these five: the book's Ch.1–3 mindset is that loops are control systems (sensor → controller → actuator → feedback), not functions. agentloop maps that directly — tools/APIs are the sensors and actuators, the tiered models are the controller, the evaluate phase plus budget/quality gates is the feedback signal. Thinking in trajectories (not transactions) is why every run persists its full state sequence: a correct answer reached through hallucinated reasoning is, per Ch.3's anti-pattern, a ticking time bomb, so trajectory quality is a first-class output alongside the answer. Divergence (quality decaying per step) has exactly three causes — context saturation, misaligned evaluation, oscillation — and §§4, 6, 9 each own one of them.
*Refs: Ch.1 paradigm/failure modes/cost law (PDF pp.6–9); pipeline-vs-loop rule (Ch.1 KEY INSIGHT, p.12); Ch.3 control loop, trajectory thinking, divergence causes, state machine, Creed (PDF pp.21–28).*

Submission contract: `"inbox is empty" is a success condition, not a termination condition` (W1.2). Every run therefore declares both: what counts as done (goal + verifiable success criteria) and what force-stops the loop regardless (step cap, wall-clock timeout, budget kill). Success is evaluated; termination is enforced.

Non-goals (v1): training/fine-tuning models, building a new agent framework (wrap existing SDKs behind `AgentBase`), domain-specific prompts (ships as config, cf. App.G templates).

## 2. Framework mapping (App.A)

| Playbook frame | Use in agentloop |
|---|---|
| `LOOP` Listen-Orchestrate-Observe-Perfect | Single-agent quality path (default run) |
| `AGENT` Assess-Navigate-Generate-Execute-Track | Runs with real side effects (writes, deploys, payments) |
| `CHAIN` Chunk-Hypothesize-Act-Inspect-Next | Decompose/debug path; plan phases |
| `REFINE` Review-Evaluate-Fix-Iterate-Narrow-Export | Eval loop + self-correction on high-stakes outputs only |
| `SCALE` Separate-Cache-Async-Log-Evaluate | Platform layer of this doc (§§6–12) |

Rule: wrong **answer** → LOOP/REFINE; wrong **path** → CHAIN/AGENT; cannot **operate** it → SCALE.

Why five names for one loop (App.A): each frame puts the spotlight on a different risk posture — LOOP ends on Perfect (quality), AGENT on Track (side-effect accountability), CHAIN on Next (decomposition momentum), REFINE on Export (iteration discipline), SCALE on Evaluate (production instrumentation). Naming the run's frame at submission tells every later reader which failure family the run was designed against. (This mapping is design synthesis from the frames' "Best for" rows, not a book quote.)
*Refs: App.A five frameworks, phase mechanics, "Best for" rows (PDF pp.273–276).*

## 3. Architecture

```
                ┌──────────────────────────────────────────────┐
                │                   agentloop                   │
  submit goal   │  ┌────────┐  ┌──────────┐  ┌───────────────┐  │
 ──────────────►│  │ Router │─►│ Planner  │─►│ Executor pool │  │
                │  │(Tier-1)│  │(Tier-2/  │  │ ReAct loops   │  │
                │  └────────┘  │ 3)       │  │ + ToolRouter  │  │
                │       │      └────┬─────┘  └──────┬────────┘  │
                │       │           │               │           │
                │  ┌────▼───────────▼───────────────▼────────┐  │
                │  │ State + Memory (working/short/long/    │  │
                │  │ procedural) · Checkpoints · Audit log   │  │
                │  └────┬────────────────────────────┬───────┘  │
                │  ┌────▼────────┐  ┌────────────────▼──────┐   │
                │  │ HITL gates  │  │ Eval / Traces / Cost  │   │
                │  │ + KillSw    │  │ + Replay / Alerts     │   │
                │  └─────────────┘  └───────────────────────┘   │
                └──────────────────────────────────────────────┘
                         │ tools (5–15 visible/step) │ models │ vector store │ queue
```

**Request flow (hybrid default):**

1. `POST /v1/runs` with goal, context, `max_steps`, `cost_budget`, confirmation policy. Router (cheap model) classifies simple (→ direct ReAct, 3 steps) vs complex (→ Plan-and-Execute, 3–7 phases).
2. Planner emits 3–7 one-sentence steps with success criteria + dependency marks. Independent steps in a phase fan out via `asyncio.gather` (P11 Parallel Loop; 40–60% latency cut, Ch.5). Strict test: parallel only if neither reads the other's output. Independent *tool calls* within a step go concurrently too (P86; −50% latency).
3. Each step runs a **ProductionReAct** loop: `THOUGHT → ACTION → ACTION_INPUT` or `FINAL_ANSWER`, one tool per turn, errors returned as observations (never raised), cost rounded to 4dp.
4. Replanner (cheap model, binary `CONTINUE`/`REPLAN`) runs after every surprising step, not only on errors. Stale-plan execution is the top P&E anti-pattern. The replanner is literally the book's thermostat (`AgentThermostat`, Ch.3): each phase it senses (current results), compares against the goal with a tolerance (convergence threshold / quality bar — "how close is good enough"), decides (CONTINUE vs REPLAN), acts (proceed or replan), and on budget/step exhaustion falls into the timeout handler (forced synthesis, never a crash). Error here is distance-to-goal, not an exception — which is why replan triggers on surprise (assumption violated), not just on errors.
5. Evaluate phase checks success criteria, confidence, cost/latency caps. Below threshold → refine (≤2 rounds, high-stakes only), degrade, or escalate with full trace.

Why hybrid (Ch.4–5): predictability is the router's decision variable. If the plan is writable before starting, Plan-and-Execute wins (explicit checkpoints, ~50% cheaper tiered: $0.12 vs $0.25 single-tier ReAct on 5+-step tasks); if each step's input depends on the previous tool's output, ReAct wins (naturally adaptive). The hybrid — plan for phases, ReAct inside — is the default because production work mixes both. Replanning after every surprising step is non-optional because plans rest on assumptions about tool outputs; executing a stale plan produces garbage at full budget.
*Refs: Ch.4 ReAct loop, golden rule, 6 failure modes (PDF pp.30–40); Ch.5 planner/executor/replanner, tiering, granularity, parallelism (PDF pp.41–49); W4.3 predictability rule; W5.2 replan-on-surprise.*

Variant selection (Ch.4 table): Basic (simple Q&A) / +Reflection (self-critique per action, 2× token cost — high-accuracy only) / +Planning (phased coherence, stale-plan risk) / Parallel (independent queries, harder errors) / Bounded (strict budgets, may truncate) / Hierarchical (manager + workers, 10+-step tasks despite overhead). agentloop exposes these as run policies, not separate systems.

Model policy: tiers are roles, not vendors. **Tier-1** = cheapest/fastest (classify, route, extract) · **Tier-2** = balanced (reason, plan, synthesize, tool use) · **Tier-3** = most capable (complex reasoning, judging, final edits). You map your purchased models to tiers in config — each tier resolving to an ordered list of onegw combo/targets (Tier-1 → `free`-class combos, Tier-3 → frontier targets with full fallback chains); the router, budgets, and admission trio (§12) only ever see tier labels. Book model names (Haiku/Sonnet/Opus, GPT, Gemini) appear below solely as calibration examples for the book's own measured numbers — substitute your models' prices and re-verify the deltas on your eval suite.

### 3.1 Platform boundaries (onegw · xdev — what agentloop does NOT rebuild)

agentloop is a headless policy + orchestration layer over three existing systems. Single-upstream rule in each direction: agentloop speaks to **onegw only** for models (the way xdev treats `onegw` as one provider key in `models.yml`, `xdev/internal/config/models.go:301`) — no provider credentials, no second transport, no second metering in agentloop config; agentloop calls **xdev** only through `AgentBase` (tool execution + session persistence); agentloop calls **LeanKG** only over HTTP (`POST /api/v1/query`, `/api/v1/memory/...`). Nothing agentloop owns can be rebuilt inside onegw, xdev, or LeanKG.

**Inheritance manifest** — every feature below is INHERITED from the named system. agentloop does NOT re-implement any of it. The one line in code that resolves the inheritance is on the right. Additions (e.g. `execution` combo) are ours to define, marked `(ours)`.

| Inherited from | What we inherit | agentloop's action | Resolving line |
|---|---|---|---|
| **onegw** (`freepeak/onegw`) | three API surfaces + any-to-any translation | caller only | onegw route per step, tier in request |
| | multi-key account pools, adaptive 429 cooldown, flap breaker, P2C slot scoring | config only | onegw config, not agentloop code |
| | ordered fallback **combos** (`order|fastest|round-robin`) | config only | `internal/router/router.go` |
| | keyword task-reorder within a combo | config only | onegw task routing |
| | RTK-style token savers (head/tail/dedup) | caller only | onegw `[saver]` config |
| | token/cost metering (source of truth) | prices them | `CostTracker.estimate()` → settle on onegw usage |
| | request-level idempotency (LRU, short TTL) | adds caller-key | agentloop: `sha256(run_id:tool:args)` persists before exec |
| | per-call approval policy (transport) | owns the policy table | agentloop `ApprovalGate.check()` → xdev approval transport |
| **xdev** (`freepeak/xdev`) | agent turn execution (`Agent.Run`, `oneTurnWithRecovery`) | drives xdev as a tool | `AgentBase.execute(tool, args)` — one call per tool, per step |
| | session persistence (append-only JSONL trees, fork/resume) | stores run/step data there | xdev `session/store.go` |
| | deferred tool catalog (`tool_search`/`tool_describe`/`tool_call`) | **never rebuilds** | register tools via xdev `Registry`; 5 v1 tools in PRD §4 |
| | approval gate (Yolo / Write / AlwaysAsk tier switch) | extends, does not rewrite | xdev `tool/approval.go` (transport); agentloop owns the policy table (fail-closed) |
| | pluggable compaction ladder | borrows pattern, not code | `CompactLadder.run()` between phases |
| | turn-budget + wrap-up prompt on exhaustion (= §4 forced synthesis) | pattern only | `xdev/internal/agent/loop.go:128-135` |
| | goal tracking (`Goal` + `TokenBudget` + `budget_exhausted`) | inherits the pattern | `xdev/internal/agent/goal.go:24-32`; BudgetGuard adds per-run economics |
| | failover-chain catalog (fuzzy resolve + dedupe) | pattern only | `xdev/internal/agent/fallback_chain.go` |
| | multi-agent hub (roster, detach, mailbox) | new run-level supervisor | xdev `hub.go` (session-level); agentloop §7 supervisor is run-level, M7 |
| **LeanKG** (`freepeak/leankg`) | code graph (L0–L3 query ladder) | consumes via HTTP | `POST /api/v1/query` (action empty = full ladder) |
| | embeddings | consumes via HTTP | `POST /api/v1/embed` (query-side, L3 only) |
| | memory banks (MEMORY.md/USER.md/topics) | reads/writes via HTTP | `/api/v1/memory/banks/{bank}/memories` |
| | MCP server (3 tools) | is a **client**, not a server | agentloop does NOT run an MCP server |
| | session offload (refs + canvas index + lessons) | reads via HTTP | leankg owns the canvas |
| | per-tool token caps (800–6000 tokens per tool) | irrelevant | leankg caps *tool response size*, not run spend — different layer |
| | RTK-style transport compression | policy only, after transport | **never** LLM-summarize what the saver already shrunk |
**What onegw lacks — agentloop's reason to exist (verified gaps, no duplication):** no price anywhere in the routing path (zero `price` hits in onegw Go code); no cross-combo strategy; no tier-by-role selection (your `onegw.toml` curates `free|dev|fast` combos manually with no `[[providers.tier]]` blocks). So agentloop decides Tier + combo/target **deliberately per call** and invokes onegw with a tier per step — onegw has no model-tier concept of its own, so the tier is agentloop's decision, not onegw's. **No agent loop, no tool policy, no eval, no kill switch exist in onegw** — these are the features that justify agentloop, not enhancements to it.

**What LeanKG lacks — agentloop consumes it, does not extend it:** no agent loop, no tool registry, no cost metering, no run lifecycle, no approval gates, no traces. LeanKG answers **queries** (`import`, `query`, `status`) and **stores** code graph + memory; it does not **run** anything. agentloop is the run layer that calls LeanKG's query surface (§4 tools 1–2).


**onegw owns** (`freepeak/onegw`): the three API surfaces + any-to-any translation; multi-key account pools with adaptive 429 cooldown, flap breaker, and P2C slot scoring (`internal/provider/provider.go:2536` — strikes/speed/recency, not price); ordered fallback **combos** with `order|fastest|round-robin` strategies (`internal/router/router.go`); keyword task-reorder within a combo (`internal…[+372b]
### 3.2 Core components

| Component | Responsibility | Playbook ref |
|---|---|---|
| Router | task classify + model/step tier assignment (Conditional Loop P12, Warmup P13) | Ch.5, Ch.13 |
| Planner / Executor / Replanner | explicit plan object the loop mutates; executor ReAct-inside; replanner binary check | Ch.5 |
| ToolRegistry | 5–15 visible tools/step, schema validation, sandbox, audit, result cap 2,000 tokens + summarizer middleware | Ch.6, P16–P30 |
| LoopRunner | bounded loop, dedup hash, cycle detector (len≥3), budget pre-check at 90%, forced-answer synthesis reserve | Ch.4, P1–P6 |
| MemoryStore | 4 tiers + 70% rule + landmarks + structured state + deletion API | Ch.8, P31–P45 |
| Supervisor | hierarchical teams only (no flat N² mesh); role cards; voting/arbitration/escalation by stakes | Ch.7, P46–P60 |
| ApprovalGate | fail-closed policy table, timeouts deny, fatigue guard (median approve <3s = lost human) | Ch.9, P30/P68 |
| EvalRunner | 4-category suite gating every deploy; A/B with p<0.05; REFINE growth 10 cases/week | Ch.10, P61–P65 |
| Tracer/Replayer | one platform (Langfuse self-hosted default), nested spans, 3-mode replay, divergence <0.9 | Ch.11 |
| Resilience | retry/backoff+jitter (transient only) → fallback chain → self-correct → breaker → degrade → escalate | Ch.12 |
| BudgetGuard | per-run + daily accumulators, per-action check, `force_synthesis` / `queue_for_tomorrow` | Ch.13, P3/P76–P83 |

## 4. The canonical loop contract

```python
async def run(state: AgentState) -> RunResult:
    for step in range(state.max_steps):                    # P1 Bounded Loop, 10–25 default
        if budget.used_pct >= 0.9:                         # pre-check, reserve 10% for synthesis
            return forced_answer(state, "budget")
        thought, action = await llm.reason(state.observe())  # Thought mandatory (20–30% fewer tool errors)
        sig = f"{action.tool}:{canonical(action.args)}"
        if sig in state.history:                           # P5 dedup
            state.observe("You already tried this exact call. Try a different approach.")
            continue
        if cycle_detected(state.history, min_len=3):       # 3-pair threshold, not 2 or 5
            return recover_or_escalate(state)
        result = await tools.exec(action, idempotency_key=sig)  # P26: writes need key checked BEFORE exec
        state = state.update(result)                       # evaluate phase: goal met?
        if state.resolved or replan_check(state) == "DONE":
            return state.resolution
    return escalate_to_human(state)                        # exhaustion has an owner
```

Why this shape (Ch.4): the Thought trace costs ~50–100 tokens (~$0.001/step) yet removes 20–30% of tool-call errors — the cheapest quality lever in the book, hence mandatory, never optional. Errors return as observations (never raised) because self-correction — the model reading its own error and retrying with context — is the loop's defining advantage over try/catch. Budget is checked *before* the call with 10% reserved because an agent that hits $0 mid-reasoning can neither synthesize nor exit cleanly. The contract mirrors `ProductionReActAgent` (book's example used a Tier-2 model, `max_tokens=1024`, THOUGHT/ACTION/FINAL_ANSWER + 4 guards) with cost rounded to 4dp for ledger parity.
*Refs: Ch.4 Examples 1–2, KEY INSIGHT, PRODUCTION TIP (PDF pp.30–34); repetition-trap anti-pattern (p.35); golden rule (p.38).*

Six failure modes → owning guard (Ch.4 table, p.35): repetition trap → call-signature hash + "try different approach" injection; premature answer → minimum-evidence threshold before FINAL_ANSWER; tool fixation → randomized tool order + "consider all tools"; hallucinated tool → name validation returning the available-tool list; context overflow → sliding-window summarization (§6); scope creep → "stay focused on the original question" in the prompt. If a run exhibits a mode with no owning guard, that is a spec bug, not a model bug.

Invariants (state-machine checks catch ~80% of loop bugs before reading LLM output, Ch.3):

- Run state is an executable enum, not prose: `IDLE → THINKING → ACTING → EVALUATING → (THINKING | SUCCEEDED | ESCALATED)`, with `THINKING → SUCCEEDED|FAILED` and `ACTING → FAILED` as early exits and three terminals (`SUCCEEDED/FAILED/ESCALATED`, no out-edges). Every transition carries a reason string, is logged, and an invalid transition raises `InvalidTransition` naming the valid set — the tracer surfaces it before any LLM output is read. First debug question is always: what state, what attempted transition, was it valid?
- `max_steps` = p95 staging completions + 30% headroom, never a round habit number (6-step task → 9, not 20).
- Exactly one tool call per ReAct turn; unknown tool → error observation listing available tools.
- **Early exit (P2):** goal-check after every step; agents waste 30–50% of budget on "just in case" steps after the goal is met.
- **Convergence check (P4):** stop when two consecutive outputs are 95%+ similar (embedding cosine or diff) — further iterations are wasted compute.
- **Timeout guard (P9):** wall-clock ceiling (e.g. 5 min) independent of step count; return best partial result on fire.
- **Evidence threshold:** no `FINAL_ANSWER` below minimum evidence (fixes premature answers); randomize tool order + "consider all tools" (fixes tool fixation).
- **Nested sub-budgets (P10):** each decomposed sub-problem gets its own step/cost cap so one hard sub-task cannot eat the run.
- **Cooldown pass (P14):** one final verification iteration (facts, consistency, format) after the main loop — catches 10–15% of slip-through errors on quality-critical outputs.
- **Adaptive step limit (P15):** after 5 steps, 80%+ done → cut remaining to 3; <20% done → raise to 25. Prevents both premature termination and wasteful overrun.
- Tool results capped at 2,000 tokens before context injection (truncate + paginate or summarize).
- No silent catch-and-continue: every error is logged + recovered, degraded, or escalated.

## 5. Tool-use engineering (Ch.6)

- **Envelope:** every tool returns `ToolResult(success, data, message, metadata)`; errors typed (`tool_not_found` / `invalid_args`+schema / `timeout after 30s, try simpler query`). Messages are the agent's next move.
- **Description = prompt:** name + `USE WHEN` + `DO NOT USE WHEN` + one example. The `DO NOT USE WHEN` clause is the highest-ROI prompt hour (+30–40% selection in book ablations).
- **Visible set 5–15** (P16 Tool Router + P24 doc injection when registry is large; Claude Code pattern: register many, present few, return summaries not dumps).
- **Pairs & gates:** every write has a read twin (P25); destructive/irreversible → `confirm_action` preview gate (P30); sandbox + dry-run + rollback (P22/P73; cf. W4.4 four-layer guardrails); tiered scopes (read / write+confirm / restricted+out-of-band approval, cf. K8s walkthrough).
- **Hygiene:** schema-validate args pre-exec (catches ~80% before spend, P19); sanitize inputs + strip instruction-like patterns from retrieved content (indirect injection); cache search/DB hits by `tool+args` hash (15–30% hit, P20); paginate large sets (default 10, max 50); audit-trail every call.
- **Lifecycle:** health-check every tool at loop start — fail fast before step 1 rather than dying at step 5 (P28); token-bucket/sliding-window rate limiter per external tool (P21); versioned tool interfaces with old version kept live during migration (P27); `dry_run` simulation mode for review workflows (P73); inject full docs only for the step-relevant tools via a lightweight classifier — 40–60% smaller prompts (P24); bundle predictable sequences into one composed tool to save reasoning steps (P18).
- **Test tools alone:** `ToolTestSuite` per tool — normal query shape, empty-input error, injection sanitization (`'; DROP TABLE…'` → success with 0 rows, no crash), timeout mapping, enum rejection echoing allowed values, output-schema consistency. A well-tested tool with a mediocre agent beats the reverse.
- **Canonical executor shape** (`ToolCompositionPipeline`, Ch.6 Ex.3): search (top-10) → read narrowly (±20 lines around hits, not whole files) → generate minimal change → apply → validate (tests/lint) → self-heal on failure (read error, targeted re-fix — never blind full retry). Write tools follow the K8s walkthrough's 3 tiers (read / write-with-dry-run-preview / restricted-with-out-of-band approval, non-prod scope by default). Dynamic tool registration (P23, Devin pattern: probe CLIs/ports, synthesize wrappers at runtime) is a v2 extension point behind the same schema contract.

Why tool quality is the thesis of §5 (Ch.6): most team failures come from ambiguous definitions and useless errors, not model choice — and an hour on descriptions out-returns an hour on model selection (+30–40% selection accuracy from a `DO NOT USE WHEN` clause + one example; bad → better → best description ladder in Ch.6). The 2,000-token result cap exists because a 500-row dump crowds out the 30% reasoning budget (§6) while multiplying cost — compression is therefore middleware, not a per-call afterthought (P29: summarize to essentials, −60–80% result tokens). Register-many/present-few (Claude Code: 40+ tools, >95% selection, effective ≤15 per decision) is why the registry scales without violating the 5–15 visible rule.
*Refs: Ch.6 tool definition/error/compression/testing/registry/security (PDF pp.50–67); 10 tool patterns (p.60); Claude Code case study (p.65).*

## 6. Memory & state (Ch.8)

Four tiers (eviction is lossless — demoted content lands in long-term recall):

| Tier | Store | Budget share |
|---|---|---|
| Working (P31 sliding window for <10-step runs; P36 buffer for intermediate work) | in-context: current file/error, last 3–5 tool results verbatim | 40% |
| Landmarks (P32; never evicted) | plan changes, user feedback, state transitions, recoveries, cost >$0.01 outputs — auto-detected on 5 signals; 4–6 per 40-step run, fitting under 10K tokens | 20% |
| Retrieved (P33 semantic recall + P38 fact cache) | vector store / KV | 20% |
| System + tools | prompt, schemas, templates | 20% |

Rules: **70% rule** — never fill >70% of window with state/history; 30% stays as reasoning budget (compress trigger calibrated per model around 0.7–0.8). Rolling compression every 5–10 steps by a cheap model (P34: keep decisions/data/status, drop pleasantries/redundancy, ≤20 bullets; 60–80% smaller). Category budgets fixed at 40/20/20/20 (P39: overruns compress or evict inside their own category only). Structured state (P43: `{sources_found: 3, …}`) over prose + validation (P44: required fields, ranges, contradictions after every mutation) + rollback for risky actions. Eviction scored on recency + relevance + importance; user messages and landmarks are never evicted (P40). **Consistency check (P67):** track key claims across the run and flag contradictions (e.g. "5 milestones" → "7 completed"). Large codebases: 30K working / 20K project map / vector index — search + selective read, never full load. Privacy: redact PII pre-write (P70: regex + NER → reversible tokens), never store secrets, ship a deletion API from day one. Context persists across sessions (P97) so returning users never start from scratch.

Supporting stores: **episodic memory** — past successful trajectories retrievable as few-shot examples for recurring tasks (P35); **working buffer** — scratch pad for intermediate work kept out of the main history (P36); **preference store** — per-user KV profile loaded at session start (P37) and updated from behavior over time (P95: preferred formats, unmodified approvals become defaults); **shared memory** (Redis/DB) as the multi-agent coordination layer (P41); **memory versioning** — every state update timestamped with its reason for post-mortem replay (P42).

Why tiers, not one store (Ch.8): the window is finite, expensive, and degrades as it fills — memory management is attention budgeting, not storage. Lossless demotion (nothing destroyed, only moved to recallable long-term) is what makes the 70% rule sustainable across long runs; the SUMMARIZE_PROMPT contract (keep preferences/decisions/data/status in ≤20 bullets; discard pleasantries, redundant info, and failed attempts unless the lesson matters) is what keeps compression at 60–80% smaller with minimal loss. The coding-agent proof: 100K lines ≈ 400K tokens can never load into a 200K window — the 30K/20K/index split navigates by search exactly like a human developer.
*Refs: Ch.8 four tiers, 70% rule, landmark auto-detect, SUMMARIZE_PROMPT, task-type matrix, W8.3 (PDF pp.78–87); P31–P45.*

## 7. Multi-agent (Ch.7) — only when justified

Single agent unless: 10+ distinct tools, mixed model tiers, genuine parallelism, or context > one window. Otherwise the N² channel tax (`N(N-1)/2`: 5→10, 10→45; >30% tokens on coordination = too many agents) eats the win.

- Topology: **hierarchical** default (P52 delegation for 10+ agents); **pipeline** (P47) for ordered stages; peer-to-peer only for creative/low-stakes. Flat all-to-all is a chaos engine — impose structure first.
- Scale as teams of 3–4 under leads (10 agents ≈ 15 channels, not 45). **Message bus** (P58: typed topics, schemas, routing rules) with messages (`task|result|question|feedback`), per-receiver FIFO, every send logged (observability).
- Role card per agent in version control (name, model tier, tools, prompt, I/O format, failure behavior) — API contracts for agents.
- Disagreement by stakes: **vote** (low) / **arbitrate** on stronger model (medium) / **escalate** with highlighted diff (high). Add one agent at a time; stop when marginal coordination > marginal value. Explicit lifecycle (P57: birth/life/death) — no zombies.
- First-class run modes (App.B P48–P60): **reviewer-writer** (P51: loop converging in 2–3 rounds); **ensemble** (P49: 3–5 agents, vote/best-pick at 3–5× compute); **specialist routing** (P50: lightweight classifier to SQL/code/writing agents); **debate** (P48: opposing positions + judge for high-stakes review); **consensus gate** (P55: majority/unanimity before irreversible actions); **agent pool** (P56: elastic spawn/terminate on queue depth); **blackboard** (P53: async shared board for loosely coupled work); **escalation chain** (P60: junior → senior → human). Audit multi-agent quality with **role rotation** (P59: reviewer↔writer). Auction allocation (P54) deferred — no heterogeneous cost-varying pool in v1.
- At 10+ agents, scale as supervisor-of-supervisors (book org: Research / Creation / Quality teams under leads).

Why hierarchy is the default (Ch.7): coordination channels grow as N(N−1)/2 — 5 agents already mean 10 channels, 10 mean 45 — and the book's gate is a token split: coordination spend above 30% means too many agents talking. The supervisor shape (decompose → assign → execute → review → revise → synthesize, with review/revise as a per-subtask loop, not an end gate) plus typed messages (`task|result|question|feedback`, per-receiver FIFO, every send logged) converts the mesh into a tree: 10 agents as 3 teams of 3–4 under leads drop ~45 channels to ~15. Role cards are to multi-agent systems what API contracts are to microservices — written before orchestration code, kept in version control. The content-pipeline proof ($0.50–1.00/post with Tier-1 SEO / Tier-2 research-edit / Tier-3 writing) shows tiering and structure beating raw agent count.
*Refs: Ch.7 topologies, N² law, supervisor pattern, W7.1–7.3 (PDF pp.69–77); P46–P60.*

## 8. Human-in-the-loop (Ch.9)

Fail-closed `ApprovalGate`: `read → auto`, `update → auto_if_confident(>0.9)`, `send/delete/deploy/pay → always_approve`, default `always_approve`. Timeouts **deny**. Target <10% interruptions while catching ~95% of errors via: confidence gate (<0.7 → human; >0.9 auto-approves after calibration), sensitive-topic list, anomaly detector (±2σ or rare-tool use), 2% random sampling of auto-approvals. Anti-fatigue: batch/tier/auto-approve routine; median approve <3s is the alarm (book walkthrough variant: <2s not reading). Progressive autonomy from first deploy: supervised (all writes approved) → after 20 actions, >85% over 50+ actions → semi-auto → >95% over 100+ → autonomous on low-risk actions. Route approvals by action type (finance/comms/general channels); a **Modify** response is a corrected action, not a rejection — the book's approval options are approve / modify / defer / escalate, and modifications are the highest-value training signal, feeding eval + gate re-tuning. Name the **decision boundary** (confidence at which autonomy yields) and the **capability boundary** (what the agent may/may not do) explicitly in each run template.

Why targeted HITL (Ch.9): approval fatigue — median approve time collapsing below ~3s — is strictly worse than no approval because it keeps the illusion of oversight while discarding the reality. The 500-ticket proof: confidence + sensitive-topic + anomaly + 2% sampling gates hold humans to ~50 of 500 daily tickets (10%) while catching 95%+ of errors — 1 reviewer FTE instead of 10 without the agent. Oversight data is then training data: every logged decision re-tunes the gate, which is what earns progressive autonomy instead of granting it by gut feel.
*Refs: Ch.9 ApprovalGate, fatigue signal, sweet spot, progressive autonomy, W9.1–9.3 (PDF pp.88–97); P30/P68.*

Safety rails (Ch.12 safety class): **guardrails** (P66: regex + keyword + classifier checks, async to avoid latency) with blocklist (≥8 injection phrases, 94% catch in book red-teams) + embedding-similarity check (<5ms); PII redact post-action with rollback on side-effect surprise; 10 MB/run export cap; rate-of-change guard (P72: pause if >5 files deleted or >20 DB records/step). **Bias detection** (P69: scan gender/racial/confirmation/recency/anchoring — flag, never auto-correct; corrections need human judgment). Every action lands in an immutable **audit log** (P71: timestamp, trace ID, reasoning) for debugging, compliance, and post-incident analysis.

## 9. Evaluation (Ch.10) — the product

- Suite split: happy 40–50% / edge 20–30% / adversarial 15–20% (incl. confident-incorrectness: correct answer = hedge/refuse) / regression 10–15%. Gate on **all four** — never ship on happy-path alone. Ready bar: happy >95%, no dangerous modes, clear escalation for known-fail cases.
- Case = `{input, expected, scoring_fn, max_latency, max_cost}`; pass = `score≥0.8 ∧ latency≤cap ∧ cost≤cap`, trajectory attached. Scoring by type: exact / fuzzy / cosine>0.85 / constraint-check / LLM-judge (stronger model, 1–10→/10) / test-pass-rate. **Output validation** (P65): structured outputs are schema-checked before return; failures retry with the validation error in the prompt. **Adversarial check** (P63): a second model, prompted to be critical (different model where affordable), must clear high-stakes outputs — self-review misses what it cannot see. **Trajectory (behavioral) evals** score the path (steps, tools, cost), not just the answer — and a **golden dataset** of verified input-output pairs is kept as ground truth (Ch.3 anti-pattern: a right answer via hallucinated reasoning is a ticking time bomb).
- Week-7 categories on top of the base four: **budget-pressure** (tasks requiring cost discipline) and **error-recovery** (tasks where tools fail) cases.
- Discipline: evals **before** agent (50 cases day one); A/B every config change (`runs_per_case=3`, `p<0.05`, watch adversarial regressions hiding under headline gains); flaky triage (10× rerun → bucket the cause: temp>0 → pin 0 or majority-vote-3; tool timing → recorded-response mocks; strict scorer → semantic/LLM-judge; genuine instability → real bug kept as "known flaky", target <2/50 in 2 weeks — never delete signal); REFINE loop (Record→Extract→Formalize→Iterate→Normalize→Expand, +10 cases/week from prod). **Justification rule:** each agent must beat a direct-LLM-call baseline by ≥2× on complex tasks or the loop overhead is unjustified.
- **Public benchmarks (industry baselines, never substitutes for custom evals):** SWE-bench (real GitHub issues; ~55–60% of Verified solved mid-2026; Python fixes/features only) · HumanEval/MBPP (164/974 function problems; 90%+ → nearly saturated; isolated functions, not multi-file work) · GAIA (real tool-use reasoning L1–L3; best agents still <70% on L3; artificial known-answer tasks) · TAU-bench (airline + retail tool-augmented service) · WebArena (shopping/forum/CMS navigation; narrow-domain tuning may not transfer). agentloop tracks the coding-track-relevant subset for regression context; ship decisions rest on the custom suite, not the leaderboard.
- `EvalRunner` in CI: fresh agent per case, `max_steps=10, max_cost=$1.00`, JSON report (pass rate, avg/p95 latency, avg cost, per-category, failures). Every bug's first fix step is a new regression case. Roll out prompt/agent changes by **canary** (P74): 5% traffic vs 95% old, compare 24–48h before full rollout.
- **External-API test pyramid** (App.F Q10): mock tests (recorded responses; deterministic; every commit) verify agent logic → integration tests (staging + rate limits; daily) verify API compatibility → production evals (sampled live traffic; continuous) verify real-world quality. Three levels because each answers a different question; one level alone lies.

Why eval is the product (Ch.10): a coding agent at 60% unit-test pass silently corrupts code with the other 40%; a fluent research agent hallucinates ~15% of citations — without measurement you fly blind at $0.10–5.00 per eval (vs $0.001–0.05 per LLM check), which is still orders of magnitude cheaper than production damage. Non-determinism compounds (same task: 3 steps once, 12 the next with different tool paths), so every change is A/B'd over the full suite at `runs_per_case=3` with `p<0.05` — the book's worked case (82.5% → 86.0% hiding a −2% adversarial regression → guardrail → 87.5%, then 10% traffic for 48h) is why headline gains never ship alone. And 92% is never "good enough" without the four checks: which category failed, dangerous vs incomplete ("I don't know" beats hallucination), failure concentration (one root cause can mean 98% after one fix), and the human baseline.
*Refs: Ch.10 eval-vs-LLM table, taxonomy, harness, scoring, A/B, REFINE, flaky triage, W10.1–10.5 (PDF pp.99–123); P61–P65.*

## 10. Observability (Ch.11)

Five pillars, one platform (Langfuse self-hosted default; one tracing platform only): **traces** (why) / metrics / logs / alerts / **replays**. Nested spans from day one (≈80-line `Trace{spans}` + `span()` context manager — trivial to build, expensive to retrofit). Auto-analyze every trace: cycle≥3 → critical; spans>20 → grinding; cost>0.8×budget; one tool >60% spans; ≥3 consecutive errors → critical; quality <0.7 baseline → review. Cycle alert at exactly 3 identical `(tool,args)` pairs (≈18% wasted-spend cut in book data). Dashboards: steps p50/p95 percentiles (>2× baseline), cost p50/p95, rolling-1h success (<93% warn / <85% page+pause), tool error %>10%, latency p95 (App F target: <5s; warn >10s, critical >30s), window util >80%, cycle rate >2%, escalation >15%. Replay in 3 modes (recorded/hybrid/live), Jaccard divergence flag <0.9. Debug protocol: provider status (30s) → segment failures → diff 5 bad vs 5 good traces → tool health → fix + regression case (book claims 95% in 30 min). Retention window per compliance (report suggests 90d), thresholds tuned monthly, prod→eval dataset weekly.

Operations contract (book Week 5): `GET /health` returns uptime, 24h success rate, avg cost, total runs. Tasks exhausting all retries land on a **dead-letter queue** for manual review, never dropped silently. Cost alerts fire at 50/80/100% of daily budget (P83) — notify, then throttle/halt non-critical traffic.

Incident playbook (Ch.18 ex.3 × Ch.11 runbook exercise): **detection** (which metric alert fired — success, latency, cost, error, cycle, escalation) → **diagnosis** (provider status → segment → 5-bad-vs-5-good trace diff → tool health; root causes ranked by probability: model update, tool breakage, input-distribution shift) → **mitigation** (pin model version, enable fallbacks, throttle/halt non-critical traffic, page on-call + auto-pause on critical) → **resolution** (fix + regression eval case) → **post-mortem** (monitoring for the cause so next time is faster). The "success rate dropped 20%" runbook names who gets paged and which dashboard opens first — written before the incident, not during it.

Why this observability shape (Ch.11): logs say *what*, traces say *why* — plausible-wrong answers never page you, so the reasoning path with per-step latency is the only signal that catches them, including across agent boundaries. One platform because context-switching destroys debugging velocity; Langfuse self-hosted is the default (≈80% of the value at zero vendor cost; LangSmith only if LangChain-native under ~5K traces/day; custom OTel only on an existing OTel stack). The analyzer's thresholds (`max_steps 20 · budget warn 0.8 · quality drop 0.7 · tool dominance 0.6 · cycle len 3 · error rate 0.1`) encode the book's incident experience — including "one cycle alert is enough" and the 3-pair threshold that cut looping waste ~18% past 5K runs/day. Replay modes answer distinct questions (recorded = did my code change? hybrid = did model behavior change? live = is it still happening?) with cheap Jaccard divergence (<0.9) instead of a judge call. The 5-step debug protocol claims 95% of issues inside 30 minutes precisely because it orders cheap global checks (provider status, 30s) before expensive local ones (trace diffs) — and every incident becomes a regression case, closing the loop with §9. Setup cost is 2–3 days; dashboards get reviewed in standup, thresholds monthly.
*Refs: Ch.11 five pillars, platform table, tracer, analyzer, metrics/alerts, replayer, protocol, W11.1–11.2 (PDF pp.124–142); P81/P92 streaming cross-ref.*

## 11. Error recovery (Ch.12)

Five categories, proportional effort (80% on model/state/logic — a rate limit wastes 2s, a wrong plan wastes $200):

1. **Transient** → `retry_with_backoff(3, base×2^attempt, cap 60s, jitter [0.5×,1.5×])`; 429×5 w/ Retry-After, 500×3, timeout×2+shrink input; never retry 400/401. Idempotency key mandatory — retry checks first attempt before re-firing.
2. **Tool** → arg correction + fallback chain (P17: `primary → secondary → cached/training knowledge`, log each rung; `AllStrategiesFailed` carries the log).
3. **Model** → re-prompt with tool list / rephrase refusal once; structured self-correct ≤2 rounds on high-stakes outputs only (each round ≈1× generation cost).
4. **State** → **checkpoint** every 3–5 steps (P8: serialize messages, results, variables; resume — not restart — on crash; mandatory for 30+-min runs), rollback on corruption.
5. **Logic** → plan revision / escalate.

Breaker: `failure_threshold 3–5, recovery 30–60s, half-open needs 2 clean probes`, single half-open failure reopens. Degrade ladder per tool (full/reduced/minimal/unavailable) with honest user copy ("Based on training data, may be outdated…"). Escalation packet: what happened / what was tried / what is needed / full trace.

Why this ladder order (Ch.12): effort follows detection latency, not frequency — a rate limit wastes 2 seconds while a wrong plan wastes $200, so ~80% of resilience engineering goes to model/state/logic failures. Retry is gated by a per-status matrix (429×5 honoring Retry-After, 500×3, timeout×2 with shrunken input; 400/401 never; refusal rephrased once; hallucinated tool re-prompted with the tool list) because blind retry of a non-idempotent write double-charges — the payment proof (pending → completed, failed → ≤2 retries, uncertain → human reconcile, status checked before every retry) is the template for all mutating tools. Self-correction triples a step's spend, hence high-stakes outputs only; the breaker (book usage: 3 failures / 30s recovery) exists to give failing services time to recover, not to fail fast; degradation rungs are pre-planned per tool so partial success never looks like silent failure — the worst option, per W12.2, is an agent pretending it researched when it did not.
*Refs: Ch.12 five categories, retry matrix, fallback chain, self-correct, breaker, degrade table, guardrails class, cycle 3-layer defence, W12.1–12.3 (PDF pp.143–160); P72/P75 safety cross-ref in §8.*

## 12. Cost (Ch.13)

`cost = (in×p_in + out×p_out) × iterations`; monthly = per-task × tasks/day × 30. Price before building (10K/day × 4 iters × 3K tok × $0.01/1K = $3.6K/mo baseline). Levers in order: **routing** (Tier-1 for classify/route/extract ≤500 tok t=0; Tier-2 for reason/plan/synthesize; Tier-3 for judge/complex only — 40–70% saved) → **prompt cache** the 5–6K system+tools block (~10% input price, 90% off repeated context, <1h work) → **layered response cache** (P78: exact 1h TTL hash → semantic 0.95/24h; 0.90 only for low-stakes; net 20–50% calls) → fewer iterations via better prompts. `ModelRouter` rows bind `(model, max_tokens, temperature)` + per-type caps (outputs cost 5–10× inputs — cap verbosity; truncate to what the question needs, P85). **Router admission (Ch.2 capability trio):** a model enters a tier only after demonstrating, on agentloop's own evals, (1) ≥95% valid structured tool-call accuracy, (2) coherent state tracking across 100K+ tokens (multi-iteration trajectory test — this caps sustainable loop depth), (3) reliable system-prompt/format adherence (this keeps the loop on track). Re-verify on every provider model update — a silent upgrade that breaks any leg is the first suspect in any overnight success-rate drop (Q11). `BudgetGuard(per_run, daily)`: per-action check → `force_synthesis` (run over) / `queue_for_tomorrow` (daily over); pre-allocate **phase token budgets** (P82: e.g. 30% plan / 50% work / 20% synthesis — a phase that overruns simplifies instead of stealing from the next) + hard kill at 100%. Frontier rule: first 50% off is nearly free; past 75% you're buying savings with quality — prove each cut on the eval suite. Surface a cost dashboard (spend/day, $/run, hit rates, model mix, budget util) and the cost-quality knee plot.

Further levers (App.B P77–P90): prompt compression (strip/abbreviate/compact formats, ~60% input saved); shared prompt-template reuse across agents (P84); streaming responses via SSE so users tolerate 10× waits (P81); lazy evaluation — fetch only what the step needs (P80); batch uniform work (groups of 10, −90% overhead tokens, P79); prefetch predictable data in parallel (P87); defer non-real-time analytics off-peak (P88); pool connections/caches across agents (P89); pre-warm before peaks (5s → <1s first request, P90); escalate through a **model cascade** (cheap → premium until acceptable) where routing confidence is low.

Why routing-first (Ch.13): model choice spans ~100–187× per token, so matching tier to subtask is the highest-impact lever (40–70%) — the book's worked reduction stacks routing (−45%) + prompt cache (−15%) + semantic cache (−15%) + fewer iterations (−10%) to take $2.50/run at 5K runs/day from ~$375K/mo to ~$132K (~$0.88/run). Caching is layered because exact (10–15% hits, zero cost) and semantic (0.95 threshold, +15–20% hits at $0.0001/query embed; 0.90 only for low-stakes given ~8% vs <1% false positives) have different risk prices — the layered proof nets ~$21.7K/mo on 10K queries/day. Budgets are enforced per action with prescribed degraded outcomes (not booleans) so a capped run still finishes: the 13.2 split (10% plan / 60% research / 20% synthesis / 10% buffer, hard kill at $1.00) forces synthesis mode — users always get a useful answer — and >20% capped runs is the signal to raise the budget or optimize. Price with `calculate_task_cost` before building (book's worked example at Tier-2 pricing: $0.054/iter → $0.27/task → $81K/mo at 10K/day); every variable in that formula is an optimization target.

Platform projection (Ch.2 ex.7, applied to agentloop's own tracks; $/run × vol/day × 30):

| Daily vol | Support track (~$0.04/run blended) | Data track (~$0.27/run) | Human baseline (support $15/ticket) |
|---|---|---|---|
| 10K | ~$12K/mo | ~$81K/mo | ~$4.5M/mo |
| 50K | ~$60K/mo | ~$405K/mo | ~$22.5M/mo |
| 100K | ~$120K/mo | ~$810K/mo | ~$45M/mo |

Why publish this in the design: the table is what turns the cost law (§1 goal 4) into capacity planning — per-tenant daily budgets, tier pricing, and the routing-vs-premium tradeoff all read off it. Recompute on every model-price change; the book's prices are mid-2026 approximations, not constants.
*Refs: Ch.13 pricing table, cost math, router, caches, prompt cache, BudgetGuard, tradeoff table, W13.1–13.3 (PDF pp.161–175); P3/P76–P83.*

## 13. Domain tracks (Part V)

agentloop ships four run profiles, one per book domain. Each reuses the core loop with domain-specific tools, budgets, and stop rules. The bottleneck is never generation — it is selection and verification.

### 13.1 Coding (`write → test → fix`, ≤3 attempts)

- Tool set: `read_file / write_file / search_code / run_tests / run_linter / git_diff`. Read-only replica + 5s query timeout + row/file caps; whitelist tables/paths.
- Context budget 50K tokens: 30% task-mentioned files, 25% import graph, 20% grep hits, 15% test files, 10% architecture map. Never read a file you won't use; prefer signatures over bodies; always include the test file of anything modified. Function-level (AST) extraction cuts context 60–80%.
- **Test-first (non-optional):** reproduce with a failing test → minimal root-cause fix → run ALL tests. Skipping tests yields fixes that break other things ~40% of the time. On exhausted attempts, re-approach with the full diff of prior edits attached (repetition drops 70% → <20%).
- Edit strategy default: **search-and-replace** (precise, token-cheap); whole-file rewrite only for new/<200-line files; never line-range (numbers shift); AST edits for refactors.
- Task prompts: bug-fix (reproduce→minimal fix→full suite), feature (read arch→follow patterns→tests+docs), refactor (baseline suite first→small increments→behavior unchanged).
- Autonomy spectrum per run: copilot (<500ms, 4K window, Tier-1, discard-on-bad) / assistant (30s–5min, 100K+ window, Tier-2, fix-retry 3×) / autonomous (10–60min, persistent env, Tier-3, research-debug-retry). Higher autonomy = higher cost; pick per task.
- Structural transformations (W14.3 rename across 47 files): categorize occurrences by kind (definition/imports/annotations/test strings) → one search-and-replace op per kind → full suite → sweep configs/docs for stragglers. Two LLM calls instead of 47 rewrites — renames are transformations, not writing tasks.

Why this profile (Ch.14): context selection, not generation, is the bottleneck — 30% more tokens on gathering (imports, grep, test files) beats richer generation prompts, and function-level extraction saves 60–80% with no quality drop. The 5-phase bug loop (parse → locate root cause → failing test → minimal fix → full suite, ≤3 cycles) exists because skip-the-test fixes break other things ~40% of the time while buying a free regression test. The three autonomy tiers mirror Cursor (sub-500ms: 4K context, intent classify, speculative decode, parse/type/symbol filter), Claude Code (incremental expansion, search-and-replace, test-driven batches, self-imposed 3-attempt/context limits), and Devin (environment discovery → plan → long execution with 5–10 expected errors → browser verification, escalate after 5 min stuck) — latency, window, and retry budgets differ by an order of magnitude per tier, so the tier is a cost decision first.
*Refs: Ch.14 write-test-fix, context budgets, edit strategies, task prompts, W14.1–14.3, Cursor/Devin cases, spectrum (PDF pp.177–193).*

### 13.2 Research (`decompose → search → verify → cite`)

- Decomposition into 3–10 sub-questions is the top quality driver — bigger than a Tier-2→Tier-3 upgrade for synthesis. Typical task: 15–50 sources, $1–5.
- Pipeline: rank/dedupe findings → extract claim–evidence pairs (Tier-1, −35–45% cost, <3% quality loss) → resolve contradictions (Tier-2, keep highest-credibility + resolution note) → synthesize → **verify every citation** (P64: re-fetch URL, judge SUPPORTED/CONTRADICTED/NOT_FOUND, drop failures + re-synthesize) → self-critique.
- Credibility tiers: peer-reviewed 9 > analyst 8 > official/company 7 > reputable news 7 > wiki 6 (check its sources) > expert blog 5(+2) > forum 3(+2 if corroborated). Contradictions → report the range with methodology/scope explanation, never false precision.
- Anti-patterns: trusting training memory for recent facts (15–25% citation hallucination; <3% with verification); uncited claims must carry "training data as of [cutoff] — verify" caveats. Budget tokens across sub-questions by importance (**priority loop**, P7: highest-impact sub-questions first so a capped run defers the marginal ones, not the critical ones).

Why this profile (Ch.15): decomposition beats model size — five targeted sub-questions surface specialist sources one broad query never finds, a larger quality delta than a Tier-2→Tier-3 upgrade on synthesis. Verification is a pipeline stage (not a hope) because unverified agents hallucinate 15–25% of citations for ~$0.30/report extra, reaching 97%+. Cost envelope per type (fact-check $0.25–0.75 → literature review $3–8) plus the proofs — competitive analysis at $2.34 (15 searches, 47 pages, 23 cites, 4m32s with contradiction resolved by scope difference), weekly briefings at $3.40 saving ~8 analyst hours (50× over 6 months), daily CEO briefs at $1.50 with dedupe, history tracking, and "dig deeper in 10 min" follow-ups — are why research is agentloop's highest-ROI scheduled workload.
*Refs: Ch.15 research loop, decomposition insight, credibility tiers, contradiction handling, verification pipeline, synthesizer, W15.1–15.4, case studies (PDF pp.194–208).*

### 13.3 Business process (exception-first automation)

- Candidate scorecard (1–5 each, 30 max): volume, consistency (80%+ standard), data availability, error cost, manual time (>30 min), integration surface. ≥25 full agent; 15–24 partial (agent does routine 80%, human takes judgment 20% with pre-populated context); <15 don't automate — or compress 30 min of work into 8 min of review.
- **Exception handler is the product:** map every known exception type (missing data, policy violation, duplicate, amount mismatch ±2% tolerance, unknown vendor, system error) before the happy path, each with auto-resolve policy + retry cap + escalation target. Automating only the happy 80% moves the hard 20% into an unpredictable fire.
- Always HITL high-impact decisions (approvals, payments, compliance). Per-rule independent checks with `compliant / violation / unclear→human` outcomes and an overall approve/conditional/reject rollup.
- **Integration tax:** budget 60% integrations, 20% agent logic, 20% testing — every enterprise API hides an undocumented format, limit, or auth edge. ROI = (savings − costs)/costs; book example 633% annual, 52-day payback. Track time saved, error-rate delta, cycle time, satisfaction.
- **Candidate mining (W16.4):** agentloop's own intake scores ERP/event-log processes on volume × rework rate × duration variance × handoff count — high rework means manual exception handling (the prime target), high variance means easy/hard mix (partial automation), handoffs are delay/error surface. Validate top-5 against process-owner interviews.
- **Cross-system shape (W16.3 deal-close proof):** webhook trigger → read full deal context → intelligent defaults per system (template by industry, timeline by contract, team by product area) → revenue entry with proper codes → personalized onboarding doc → team notification; merge-on-duplicate, escalate on unmapped products, VP sign-off above $100K. Every action lands in an audit trail *with the agent's reasoning* — trigger-action workflows without validation, judgment, and exception paths are not agents.

Why exception-first (Ch.16): the happy path could be a script — the 20% of cases consuming 80% of human time is the only thing worth the loop's multiplied cost, which is why the finance proof ($680K labor + $120K error/penalty savings on $95K implementation, 52-day payback) comes from exception-heavy departments, not clean ones. The Zapier test draws the line: a trigger-action workflow with no validation, judgment, or exception paths is not an agent and should not pay loop prices. And the middle of the scorecard matters most — partial automation that compresses 30 minutes of expert work into 8 minutes of review often beats full automation on risk-adjusted ROI.
*Refs: Ch.16 loop, scorecard, invoice/onboarding/compliance code, exception table, W16.1–16.4, finance case, integration tax (PDF pp.209–222).*

### 13.4 Creative (`generate → evaluate → refine`, ≤3–4 revs)

- Rubric first: decompose "quality" into measurable dimensions (P62: 1–10 scales with examples per level; total gates return-vs-refine) — clarity, engagement, accuracy ≥95%, tone match, SEO, originality — with 1–10 scales + examples, calibrated against ≥30–50 human ratings (re-train rubric on >20% disagreement). A loop optimizing the wrong metric produces simple, clear, boring prose.
- Each round refines only the weakest dimension; stop at all-thresholds-met, max 3–4 revs, or Δmin-score <0.05 (best-seen returned). Returns diminish fast (+30% / +10% / +3%); beyond that the agent edits in circles. Invest savings in better first drafts and voice profiles (quantitative metrics + qualitative traits + reference samples).
- Tiering: Tier-1 research → Tier-2 draft → **Tier-3 edit** (+7–9% human scores at −40% vs all-Tier-3). Pipeline shape: researcher → writer → editor with a human gate on outline and review threshold (e.g. 0.85) on output.
- Not for: truly novel work, deep expertise gaps, emotionally sensitive content, inarticulable brand voices — agent drafts, humans finish.

Why rubric-first (Ch.17): the loop can only climb what the evaluation measures — optimizing readability scores yields simple, clear, boring prose, which is why "boring, no personality" is fixed by operationalizing the complaint (W17.3: mine boring-vs-great pairs for quantitative gaps, add specificity ratio / ≥2 analogies per 1K words / hook score / ≥1 example per section, verify the new dimensions separate the groups, then refine against them). Calibrate the judge against ≥30–50 human ratings and retrain past 20% disagreement; A/B agent output against human output in production. The pipeline proof (research→write→editor, outline gated by human, 0.85 review gate: 4× output, 82% vs 78% human baseline, 15 min vs 3 days, $1.20 vs $250/piece) is why creative runs are agentloop's cheapest high-volume track.
*Refs: Ch.17 creative loop, ContentAgent, variants, rubric calibration, W17.1–17.3, voice matching, pipeline case (PDF pp.223–234).*

## 14. Product layer (Ch.18)

agentloop is productizable only with five layers beyond the loop: **UI** (chat/dashboard/API) / **authN/Z** per capability / **rate limit + billing** / **monitoring + incident response** / **feedback flywheel**. Request pipeline: verify token → rate-limit check → traced run → metered bill (tokens + cost). Never leak raw provider errors across the app boundary — every failure maps to a human message with next steps (wait / partial-now / simplify; accept-anyway / add-context / human-review).

- **Fit test** before building: repetitive enough? failure cost acceptable? success measurable? users will trust it? Any "no" → rescope.
- **Pricing:** 5–15% of replaced labor, not cost-plus (book example: $0.10 compute vs $150 manual value → $0.49/doc at 80% margin). Tiers gate steps/model/support (e.g. free 50 runs×5 steps tier-1 → pro 1K×15 tier-2 → enterprise custom tier-3).
- **Reliability bar:** 95% means 1-in-20 bad experiences — reach 98%+ on the core case before scaling users or scope; classify failures, degrade gracefully, add human fallback for premium, feed every failed trace into evals.
- **UX contract:** live progress (step, count, ETA, partials, spend-so-far); **progressive disclosure** (P91: one-line summary + confidence, evidence/trace/raw data behind expanders — most users never open them); retry/export/feedback actions on every result; runs >5s stream via SSE (P81/P92). Every write is **reversible in one click** (P93: action history + rollback) and every decision is explainable on request — **explanation mode** (P94) surfaces the trace in plain language from the same data debugging uses. Match format to content (P96: tables for comparisons, charts for trends, code for implementations). Translate infrastructure errors into user actions (P98: what happened, what it means, what happens next). The **feedback loop** (P99: thumbs on every output → eval cases → auto-investigation on clusters) is wired in §9, not bolted on.
- **Flywheel:** thumbs up/down (+category/comment) stored with trace; weekly clustering; clusters ≥5 auto-create regression eval cases. This is the growth engine — satisfaction → usage → feedback → quality.
- **MVP discipline:** one agent, one doc type, no collaboration/editing/API; ship in ~4 weeks, then reliability → scale (queue, cache boilerplate) → growth. New scope only after 98% on the core.
- **Onboarding (Ch.18 ex.4):** every product surface sets expectations up front — a sample task with expected output, explicit accuracy disclaimers, and a test drive with feedback collection. Users who have seen the failure modes trust the successes.
- **Unit economics (Ch.18 ex.7):** track cost/run, revenue/run, gross margin, CAC, LTV, and payback period per track — and the scale at which the track turns profitable. A track with great evals and negative unit economics is a demo, not a product.
- **Billing model:** subscription + usage (recurring revenue with cost alignment), not per-run (unpriceable variance), per-token (user-hostile), outcome-based (unmeasurable), or seat-based (usage-blind) — the book's five-model tradeoff. agentloop meters per span so both the flat subscription quota and the overage price rest on the same ledger.
- **Failure-rate program:** 95% → classify failures by input/segment/window → degrade to partials → human fallback for premium → every failed trace into evals → 5%-to-2% in 30 days on a daily dashboard. The reference economics justify it: the book's support design runs $0.04–0.09/ticket against $15 human cost ($2–4.5K/mo vs $750K at 50K tickets) — agentloop's margin story is identical arithmetic at platform scale.

Why product-first (Ch.18): a demo is not a product — the gap is ~10,000 edge cases plus billing, and reliability (98%+) must precede users and scope because each new user multiplies the 1-in-20 failure surface. Value pricing (5–15% of replaced labor, 80%+ margins) is what funds the reliability work; cost-plus pricing starves it. The flywheel closes the loop economically: satisfaction → usage → feedback → quality → more usage, so each reliability point earned makes the next one cheaper to earn.
*Refs: Ch.18 five layers, pricing models/tiers, MVP/failure/scale/pricing walkthroughs, fit test, UX patterns, flywheel (PDF pp.236–246).*

## 15. API & data (v1 sketch)

- `POST /v1/runs {goal, context, template?, max_steps?, cost_budget?, confirmations?, idempotency_key}` → `{run_id}`. `GET /v1/runs/{id}` (status, trajectory, spend, spans). `POST /v1/runs/{id}/approve|modify|reject`. `POST /v1/runs/{id}/kill` (P75, tested quarterly) is the run-level switch; **graceful handoff** (P100) is its companion — every escalation carries what the user asked, what the agent tried, and why it failed, so the human never makes the user repeat themselves. Webhooks/SSE status updates for runs >5s (10× wait tolerance, P92).
- Why this shape: the request body is the book's 8-dimension loop spec (W3.3) made mandatory — goal, sensor (context sources), controller (model tier), actuator (tool set), feedback (eval criteria), termination, max_steps, cost_budget. A run that cannot state all eight is not plannable and is rejected at submission, which is cheaper than discovering it at step 9. Rejection costs one validation; an unplannable run costs a full budget plus a confusing partial answer — the submission gate is the cheapest guard in the whole design.
- Stores: runs+checkpoints (Postgres), traces/spans (Langfuse/OTel), vectors (pgvector), exact/semantic caches (Redis), eval cases + golden sets (Postgres), audit log (append-only). All spend metered per span for BudgetGuard + CostTracker parity (`(in×p_in+out×p_out)/1M`; per-1M table below holds **your** models' prices — book's App.F Q8 values shown as calibration examples: Tier-1 0.8/4, Tier-2 3/15, Tier-3 15/75). Span estimates gate actions; usage actuals settle nightly against onegw `/admin/usage/export` (§3.1).
- Schemas (design synthesis applying P65 to our own API — every response is schema-validated, every violation retries with the error, including agentloop's own):

```json
{
  "run_submit": {"goal": "string", "context": {"sources": ["tool|kb|upload"]},
    "frame": "LOOP|AGENT|CHAIN|REFINE|SCALE", "template": "T1..T8",
    "controller": {"tier": "1|2|3"}, "actuators": ["tool names"],
    "feedback": {"success_criteria": ["string"], "confidence_floor": 0.7},
    "termination": {"max_steps": 9, "wall_clock_s": 300, "cost_budget": 0.5},
    "confirmations": {"policy": "fail-closed", "channels": ["finance"]},
    "idempotency_key": "string (required for writes)"},
  "run_state": "IDLE|THINKING|ACTING|EVALUATING|SUCCEEDED|FAILED|ESCALATED",
  "webhook": {"run_id": "string", "state": "run_state", "step": 0,
    "partial": "summary|null", "spend_so_far": 0.0, "eta_s": 0},
  "error": {"type": "timeout|low_confidence|budget|tool|validation",
    "message": "human sentence, never a stack trace",
    "options": ["wait", "partial-now", "simplify", "add-context", "human-review"],
    "trace_id": "string"}
}
```
- Why this stack: Postgres/pgvector/Redis is the book's scaling path (queue like SQS/RabbitMQ so one large document never blocks the line; shared boilerplate cache across runs) with billing (Stripe-style) and blob storage (S3-style) at the MVP edge — boring, managed, and sufficient until the eval suite says otherwise. The queue is load-bearing for multi-tenant fairness: per-tenant daily budgets (§18 open questions) are enforced at enqueue, not mid-run. Nothing here is chosen for novelty; every component is replaceable behind its interface the day a measurement justifies it (App.C strangler logic applied to infrastructure).

## 16. Ship defaults (App.G-style CONFIG)

| Template | Model | max_steps | budget | Gate |
|---|---|---|---|---|
| support (T1) / schedule (T5) / doc-extract (T7) | Tier-1 | 5–6 | $0.03–0.08 | escalate <0.7 (support) / <0.90 → review queue (docs) / confirm writes (schedule) |
| data (T2) / content (T3) / review (T4) / monitor (T6) | Tier-2 | 6–10 | $0.20–0.50 | quality≥0.85 (content) / confirm incident + 300s auto-escalate (monitor) / ≤20 files (review) / SQL ≤100 rows + 30s timeout (data) |
| supervisor (T8) | Tier-2, specialists mixed Tier-1–3 | 12 | $2.00 total | review_output 0–10, retry-once-then-absorb |

Start from the nearest row, keep its caps, expand only tools/schemas.
*Refs: App.G T1–T8 SYSTEM_PROMPT/TOOLS/CONFIG verbatim values (PDF pp.319–331); chooser table ("If Your Agent Needs To… Start With").*

## 17. Build order & acceptance

Primitives first (Ch.20 Day-1 rule): no frameworks until the loop, tool call, trace, and eval primitives are hand-built and understood — agentloop's custom LoopRunner behind `AgentBase` is that rule applied, not framework avoidance for its own sake. Post-build hardening follows Weeks 5–8: production wrapper (tracer + retry/fallback + BudgetGuard + `/health` API) → supervisor-worker multi-agent → 50+ eval cases across five categories as a CI leaderboard → ship to ≥5 real users and let their feedback teach what solo development cannot.

Milestone shape (Ch.20 hours adapted to the team build): M1 LoopRunner + guards, ~15–20h → M2 EvalRunner + error recovery, ~15–20h → M3 first domain track + trace demo, ~20–25h → M4 deploy + docs + cost dashboard, ~15–20h. Constrained-scope cut (W20.1 rule, 40h instead of 70h): keep LoopRunner, eval suite, one domain track, and the demo; defer P&E conversion, multi-agent, and the web console — one excellent track beats five mediocre ones.

Why this order: the book's builder ratio (80% building, 20% reading) plus its mistake table — framework-first, eval-skipping, premature optimization, and multi-agent sprawl are the four documented ways to fail. M1–M2 front-load the two non-negotiables (a loop that stops; a suite that measures) before any domain work; the demo lands in M3 because recording first reveals what polishing hides (45-second pauses, unreadable traces, $0.87 two-step lookups).

1. LoopRunner + ToolRegistry + BudgetGuard + kill switch (P1/P3/P75) → acceptance: runaway prompt (e.g. "meaning of life" loop probe) halts at caps with partial synthesis, no duplicate write without key. Reference core stays at App.F Q6 scale (50–70 lines: prompt + schemas + loop + graceful termination, no framework) — if the LoopRunner outgrows that without new guards, it is accreting framework, not logic.
2. ReAct guards (dedup/cycle/validation/2K cap) + Tracer + cycle alert → acceptance: 3-layer repetition test passes; trace diff finds injected fault.
3. Planner/Replanner + parallel phases + tiered models → acceptance: 5+-step task ≥40% cheaper than single-tier ReAct with eval parity (book: 52% tiered P&E; App.C ship bar within 3%).
4. Memory tiers + checkpoints + deletion API → acceptance: 20-iteration agent holds 70% rule; resume from step-5 checkpoint after step-7 fault.
5. HITL gates + audit + progressive autonomy counters → acceptance: <10% interrupt, sampling + anomaly review path exercised.
6. EvalRunner in CI + REFINE job + cost dashboard → acceptance: deploys blocked on full-suite gate; weekly +10 case growth; knee plot published.

Hardening backlog (book exercises, adopted as agentloop's own test plan): chaos harness injecting timeouts/wrong-responses/unavailable-tools into tool calls, measuring recovery rate, time-to-recovery, and quality degradation (Ch.12); degradation mapping — 5 tools × 32 availability combos each rated full/reduced/minimal/unavailable with routing logic (Ch.12); cost-quality curve at 5 levels (full, −25/−50/−75/−90) to publish the knee (Ch.13); cost-profiling pie per run (model-by-model, tools, embeddings, overhead) feeding the §12 dashboard (Ch.13); tool-usage dashboard (most-called / most-failed / never-used) feeding registry pruning (Ch.6); 10-query cost measurement including the trace's share (Ch.4).
*Refs: Ch.4/6/12/13 exercises (PDF pp.39, 66, 160, 175).*

## 18. Open questions

- Runtime language (Go to match freepeak backends vs Python for SDK parity) — hidden behind `AgentBase.execute()` either way. Framework chooser per App.C matrix: linear chains → LangChain (ecosystem 5s); branching/state/HITL-persistence → LangGraph (control-flow 5s, steep curve); role-based teams → CrewAI (multi-agent 5, weak cost control); conversational/research → AutoGen (prototype-only: runaway-token risk); else custom. Prototype on a framework if <4 weeks, wrap it in the abstraction from day one, migrate via strangler / abstraction-swap / big-bang (2-week parallel) once workarounds exceed ~20–30% — and only when the custom build scores within 3% on the same eval suite (>5% worse = debug first). Time economics behind the rule: framework prototype 1–3 days vs custom 3–7; production 2–6 weeks vs 4–8; framework overhead +10–30% on cost. Vet any framework dependency first: stable release in the last 3 months, ≥5 findable production users, docs current with the release — any "no" means wait or build custom.
- Tenant isolation + per-tenant daily budgets and PII redaction scope.
- Which tools ship v1 (start: 3 reads + 1 search + 1 ticket/incident writer per template).

## Appendix: ubiquitous language (App.D, verbatim where quoted)

- **Trajectory** — sequence of states from start to goal. Think in trajectories, not transactions; convergence ("improving each step?") over correctness of a single output.
- **Convergence / divergence** — output stabilizing vs quality decaying per iteration. Divergence has three causes, checked in order: context saturation, misaligned evaluation function, oscillation between broken states.
- **Action cycle** — repeating the same action sequence without progress; detected by hashing recent actions.
- **Context poisoning** — irrelevant/misleading accumulation degrading performance. **Tool confusion** — wrong-tool selection from ambiguous overlap.
- **Attention / step / token / cost budget** — pre-allocated caps per category, run length, tokens, dollars. **Decision boundary** — confidence at which autonomy yields to escalation. **Capability boundary** — what the agent may/may not do, in prompt + tool set.
- **Golden dataset** — verified input-output ground truth. **Reflection** — self-evaluation meta-step. **Safety layer** — pre/post-action checks on every tool call. **Sandboxed execution** — isolated runs with host limits.
- **Model cascade** — escalating cheap → premium until acceptable. **Supervisor agent** — coordinates and quality-checks workers. **Feedback flywheel** — usage feedback compounding into quality.
- **Prompt injection** — malicious input text manipulating behavior; tool outputs are data, never commands (output filter strips instruction-like patterns pre-injection). **Grounding (RAG)** — retrieved documents joined to generation for verifiable output. **Temperature** — output randomness; eval runs pin low/zero. **Structured output** — machine-readable (JSON) responses validated against schemas (P65). **Tool schema** — the JSON name/description/parameters contract the router and validator share.

## Sources

- `The 0→1 Loop Engineering Playbook (2026 Edition) (Press, Valenx) copy.pdf` — read cover to cover (335 pp, 20 ch, App.A–G): Ch.1–3 mindset and cost law; Ch.4–6 loop patterns and tool engineering; Ch.7–9 coordination/memory/HITL; Ch.10–13 eval/observability/recovery/cost; Ch.14–17 domain tracks; Ch.18–20 product/career/30-day plan; App.A frameworks, App.B 100 patterns, App.C matrix + migration strategies, App.D glossary, App.F interview numbers, App.G 8 templates (CONFIG values in §16 are book-verbatim).
- `loop-engineering-playbook-report.html` — full-book report used as cross-check; its staleness warning stands (model names/prices are mid-2026 approximations; per-call tier figures in P76 vs per-1M table in App.F Q8 normalized here to the per-1M table for billing). Report-only claims not found in the book (e.g. 90-day trace retention) are marked as such above.
- Theory baseline (App.E): ReAct (Yao et al. 2022), Toolformer (2023), Weng's agent survey (2023), Masterman architecture taxonomy (2024), Anthropic's Building Effective Agents (2025) — the five papers behind Ch.4–6 and App.C; reference implementations to read against the LoopRunner: SWE-agent, GPT-Researcher, Aider, Open Interpreter, MetaGPT.
- Release gate (this document): 25 review loops against the book text; every number/quote grep-verified in-book or labeled synthesis/report-only; 100/100 patterns cited; every substantive section carries Why + Refs; known book contradictions recorded (70%-prose vs 0.8-code compress trigger; 3s vs 2s approve-time variants; per-call vs per-1M tier figures). Treat this doc like the book treats agents: calibrated priors, gated by evals, never finished.
