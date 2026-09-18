# agentloop duplication audit — what we inherit, what we re-implement, what is new

**Purpose:** prevent agentloop from re-building what onegw / xdev / LeanKG already provide,
by naming every overlap, its resolution, and the one line in code that resolves it.
**Result of this audit:** three confirmed duplications to cut (§4), six inherit-with-boundaries (§5),
nine genuinely new (§6), and the one-line inheritance manifest (§7).

Audit date: 2026-09-19. Evidence: `design.md`, `docs/PRD.md`, `internal/` listings of all three
systems, `AGENTS.md`, `docs/check-prd.py`.

---

## 1. The layering contract (already in design.md §3.1, completed here)

design.md §3.1 says agentloop is a **headless policy + orchestration layer** over onegw + xdev + LeanKG,
and lists what each owns. This audit makes the contract *executable*: for every feature, one owner,
one line of code that enforces it.

| Concern | Owner | Enforced by | agentloop's role |
|---|---|---|---|
| Model routing, fallback, token saving, multi-key pools | **onegw** (`internal/router`, `internal/provider`, `internal/saver`) | onegw config; agentloop sends a tier per step | caller of onegw; **never** re-implements combos, cooldowns, savers |
| Token/cost metering (source of truth) | **onegw** (`internal/usage`) | onegw usage rollups | prices them; **never** invents a price |
| Idempotency at the wire (retry dedup) | **onegw** (`internal/idempotency`) | `Idempotency-Key`/`X-Request-Id` | adds caller-key on top; **never** relies on onegw's alone |
| Execution sandbox, file mutation, per-call approval | **xdev rpc** | xdev permission policy + `--add-dir` restriction | drives xdev as a tool executor only; **never** loops xdev's loop |
| Tool registry (5 v1 tools), deferred catalog | **xdev** `tool/` | xdev `AgentBase` | registers tools through xdev's registry; **never** rebuilds `tool_search`/`tool_describe`/`tool_call` |
| Code graph, embeddings, memory banks | **LeanKG** HTTP API | leankg `internal/core` | consumes as a tool; **never** re-embeds files |
| Session persistence (JSONL trees) | **xdev** `session/` | xdev `internal/session/store.go` | stores run/step data there; **never** writes its own session format |
| Loop bounds, kill switch, cost circuit breaker per *run* | **agentloop** | `LoopRunner`, `BudgetGuard` | **new** — no owner in the portfolio |
| Eval suite, traces, replay | **agentloop** | `EvalRunner`, `Tracer` | **new** — no owner in the portfolio |

## 2. The one real duplication risk (already flagged in design.md §3.1)

design.md §3.1 says: *"Borrowed from xdev (freepeak/xdev, patterns not code)"* but names only two
patterns (turn-budget + goal token budget). It does **not** list the three largest borrows, which
means a builder could re-implement them. The three:

| Borrow | Where it lives today | design.md says | PRD says |
|---|---|---|---|
| Deferred tool catalog (`tool_search` / `tool_describe` / `tool_call`) | `xdev/internal/tool/catalog.go` | §3.1 only names `compact_ladder.go`, `fallback_chain.go` | §4 "registry is interface-based"; §13.1 move 4 "ToolRegistry (§4)" |
| Approval gate | `xdev/internal/tool/approval.go` (Yolo / Write / AlwaysAsk) | §8 is silent on the xdev origin | §7.5 "four HITL patterns adopted"; §13.1 move 10 |
| Compaction ladder | `xdev/internal/agent/compact_ladder.go` | §3.1 names it explicitly | §5 "registry is interface-based" |

**Resolution:** design.md §3.1 gets an explicit "Inherits from xdev" table (§5 below) and `design.md`
is the single source of truth for what is inherited vs. new. The PRD §4 already has the 5-tool table
and the idempotency two-layer contract — these are not duplicated in code, only documented in two places.

## 3. Confirmed duplications to cut (three, all in approval + catalog territory)

### 3.1 `tool_search` / `tool_describe` / `tool_call` deferred catalog — **do not rebuild**

- **Duplicated in:** `xdev/internal/tool/catalog.go` (Bridge tool names `ToolSearchName`, `ToolDescribeName`, `ToolCallName`, `DefaultSearchLimit`, `Runner` type, `Catalog`/`Registry`/`Entry` types).
- **Why agentloop must not rebuild:** the PRD §4.1 says agentloop drives xdev as a tool executor via xdev's registry, and §13.1 move 4 ("Treat tools as an API surface") is implemented through xdev's `ToolRegistry`. A separate agentloop-side catalog would mean two registries, two schemas, two discovery paths — and the acceptance suite (§11.2) registers test doubles *through xdev's interface*.
- **What agentloop does instead:** consume xdev's registry through `AgentBase`. New tool names (the v1 five in PRD §4) are *registered* with xdev's registry, not defined in agentloop.

### 3.2 Approval gate — **extend xdev's, don't rewrite**

- **Duplicated in:** `xdev/internal/tool/approval.go` (`ApprovalMode`: Yolo/Write/AlwaysAsk, `NeedsApproval(mode, tier)`, `DefaultApprovalMode = Yolo`).
- **Overlap:** both gate writes by reversibility tier.
- **Divergence:** xdev's is a *three-tier on/off switch* (Yolo = silent, Write = ask, AlwaysAsk = always). agentloop's (PRD §7.5, design.md §8) is a *confidence + sensitive-topic + anomaly + 2% sampling* gate with timeouts that **deny** and fatigue detection (median approve <3 s). Different shape — xdev's is a default; agentloop's is a policy table with an owner.
- **What agentloop does instead:** own the fail-closed policy table (read → auto, update → auto_if_confident >0.9, send/delete/deploy/pay → always_approve, unknown → always_approve), and **call** xdev's approval path as the transport. Agentloop owns *what* must be approved; xdev owns *how the human is asked*.
- **Ceiling note:** xdev's Yolo default is the pi philosophy (Yolo first, ask later). agentloop starts fail-closed (the book's Ch.9). This is a deliberate portfolio divergence, recorded in PRD §3.1.

### 3.3 Compaction / context compression — **complementary, but name the boundary**

- **Duplicated in:** `xdev/internal/agent/compact_ladder.go` (pluggable ladder, agent-level) and `leankg/internal/compress/` (RTK-style, transport-level). design.md §6 adds its own 70% rule + SUMMARIZE_PROMPT.
- **Boundary (already in design.md §3.1, but must be a comment):**
  - xdev's compaction = *agent-session* memory management (what enters the reasoning budget across turns).
  - leankg's compress = *transport* (byte-level head/tail/dedup of tool output, owned by onegw's savers in the gateway).
  - agentloop's 70% rule = *policy* (what enters the *loop's* reasoning budget, with pagination pointers).
- **Rule to enforce in code (per design.md §3.1):** "Never LLM-summarize what the saver already shrunk." The saver (onegw) shrinks at the transport layer; agentloop shrinks at the policy layer. A tool result passes through onegw's saver *before* it reaches agentloop's 70% check — so agentloop compresses *only* after the saver, never before.

## 4. Inherit-with-boundaries (six features that exist elsewhere; agentloop consumes them)

### 4.1 Goal tracking with token budget → xdev `goal.go`

- **Exists in:** `xdev/internal/agent/goal.go` (`Goal` struct: `Objective`, `TokenBudget`, `Status` ∈ active/completed/dropped/budget_exhausted, evidence-gated completion).
- **Overlap:** both track a goal with a token budget and a `budget_exhausted` terminal state.
- **Boundary:** xdev's goal is a **session-scoped, user-typed objective** (completions are explicit, evidence-gated). agentloop's budget is a **per-run economic instrument** (`BudgetGuard` with per-run + daily accumulators, per-action check, `force_synthesis`/`queue_for_tomorrow`). Different scope, different owner.
- **Action:** design.md §3.1 already says "Borrowed from xdev — goal token budget with `budget_exhausted` as terminal state". Keep it there. agentloop's BudgetGuard is **new** (no onegw/leankg equivalent) — confirmed.

### 4.2 Agent turn loop → xdev `loop.go`

- **Exists in:** `xdev/internal/agent/loop.go` (`Agent.Run`, `oneTurnWithRecovery`, `recoverOverflow`, steering/followUp, TurnHooks).
- **Overlap:** both are agent loops with recovery.
- **Boundary:** xdev's loop is a **single streaming turn** (prompt → stream → persist), with steering and recovery from provider cutoffs. design.md §4's LoopRunner is a **bounded ReAct cycle** (`observe → reason → act → evaluate`) with `max_steps`, cost pre-check at 90%, call-signature dedup, cycle detector, forced-answer synthesis, escalation on exhaustion. Completely different shape.
- **Action:** design.md §3.1 says "xdev-shaped 500ms→8s+jitter, onegw pool cooldowns". agentloop's LoopRunner is **new**; it *calls* xdev's `AgentBase` per step, never inverts it. **Confirmed new.**

### 4.3 Memory tiers → xdev session + leankg memory

- **Exists in:** `xdev/internal/session/` (JSONL trees, fork/resume), `xdev/internal/memory/` (hindsight KV), `leankg/internal/memory/` (Markdown tree: MEMORY.md/USER.md/topics/), `leankg/internal/session/` (refs + canvas index + lessons).
- **Boundary:** design.md §6's four tiers (working/landmarks/retrieved/system-tools with 70% rule, landmarks, rolling compression) are an **in-context policy** for the running loop, not a storage system. All four tiers *persist through* xdev's session store and *read from* leankg's memory, but none of them is a storage implementation.
- **Action:** design.md §6 is **new policy over existing stores**. No duplication. Confirmed.

### 4.4 Multi-agent → xdev `hub.go`

- **Exists in:** `xdev/internal/agent/hub.go` (roster, detach, mailbox, steering between agents).
- **Boundary:** xdev's hub is **session-level** multi-agent coordination (steer/followUp between agents within one session). design.md §7's supervisor is **run-level** (decompose → assign → execute → review → revise → synthesize, with stakes-based vote/arbitrate/escalation). Different scope; xdev's hub has no message bus, no role cards, no arbitration.
- **Action:** design.md §7 is **new** (M7, conditional). Partial overlap only at the "agent can call another agent" level. Confirmed new.

### 4.5 MCP server → leankg `internal/mcp/`

- **Exists in:** `leankg/internal/mcp/server.go` (3-tool MCP server: import/query/status, stdio + streamable HTTP, RBAC).
- **Boundary:** agentloop is an **MCP *client*** of leankg (PRD §4.1: "No MCP client in the request path" — leankg's tools are consumed via HTTP `POST /api/v1/query`, not via an in-process MCP client).
- **Action:** no overlap. agentloop calls leankg's HTTP API, not its MCP server. Confirmed no dup.

### 4.6 Usage/rate limiting → onegw `internal/usage`, `internal/quota`, `internal/ratelimit`

- **Exists in:** onegw's lock-sharded usage tracker, quota management, rate limiting.
- **Boundary:** onegw meters *provider calls*. agentloop's `BudgetGuard` meters *run spend* (per-run + daily, enforced pre-action). One gateway-level, one application-level. Confirmed no dup — and design.md §3.1 states this exactly.

## 5. Genuinely new features (nine — agentloop's reason to exist)

These exist in **no** onegw/xdev/LeanKG code. Design.md claims they; this audit confirms they are new.

| # | Feature | design.md § | Evidence it is new |
|---|---|---|---|
| 1 | **LoopRunner** (bounded ReAct with max_steps, cost_budget, kill switch, forced synthesis) | §4, §11 | xdev has turn loop; no portfolio code has a *bounded* loop with economic caps |
| 2 | **Plan-and-Execute hybrid** (Planner/Executor/Replanner, 3–7 phases, binary CONTINUE/REPLAN) | §3, §5 | no portfolio code has a planner/replanner split |
| 3 | **Tracer/Replayer** (Langfuse/OTel nested spans, 3-mode replay, divergence <0.9) | §11 | onegw has request logging only; xdev has TurnHooks; leankg has per-MCP metrics |
| 4 | **EvalRunner** (4-category suite, A/B p<0.05, REFINE job, CI gate) | §9 | xdev `eval/` is a Python *code execution* kernel, not an agent eval suite; leankg `golden/` is query-ladder test data |
| 5 | **BudgetGuard** (per-run + daily accumulators, per-action check, force_synthesis, queue_for_tomorrow) | §12 | onegw has no per-run cost cap; leankg `budget/` caps *tool response size* (800–6000 tokens per tool), not run spend |
| 6 | **Kill switch** `POST /v1/runs/{id}/kill` (P75, graceful handoff P100) | §11 | no portfolio code has a run-level kill with partial synthesis |
| 7 | **ToolRegistry** (5–15 visible, schema validation, sandbox, audit, 2K cap, summarizer middleware, tool fallbacks) | §5 | xdev's registry is the *storage*, not the policy layer agentloop sits on |
| 8 | **Cycle detector + dedup hash** (3-pair threshold, `tool:canonical(args)` signature) | §4 | xdev has no call-signature dedup; leankg has no cycle detection |
| 9 | **Cost pricing table** (per-1M prices, versioned, per-span cost) | §12 | onegw has **zero `price` hits** in its Go code (verified) — it never prices; agentloop prices |
| 10 | **HITL ApprovalGate** (fail-closed policy table, timeouts deny, fatigue, sampling) | §8 | xdev's approval.go is a tier switch (Yolo/Write/AlwaysAsk), not a policy table |
| 11 | **Graceful handoff** (escalation carries goal + what was tried + what is needed + full trace) | §11 | not in any portfolio code; xdev has `handoff.go` but for session-level context transfer, not run-level escalation |

## 6. What design.md §3.1 gets right but understates

These are correctly *not* duplicated but the documentation should be more explicit so a builder doesn't second-guess them.

| design.md claim | Understatement | Fix |
|---|---|---|
| §3.1 "Borrowed from xdev (patterns not code)" | names only 2 of 5 borrows | add the deferred tool catalog, approval gate, and compaction ladder to the list (§5 above) |
| §3.1 "onegw owns the three API surfaces" | says nothing about leankg | add a "**LeanKG owns**" row: code graph, embeddings, memory banks, MCP server |
| §3.1 "What onegw lacks" | lists pricing/cross-combo/tiering only | add: no agent loop, no tool policy, no eval, no kill switch, no trace — these are the *reason* agentloop exists |
| §6 Memory tiers | implies agentloop builds memory storage | clarify: tiers are an in-context policy; persistence through xdev session + leankg memory |
| §8 HITL | silent on xdev origin | add "(extend xdev's approval.go, don't rewrite — different shape: policy table vs. tier switch)" |
| §5 ToolRegistry | silent on xdev registry | add "(consume xdev's registry through AgentBase; 5 v1 tools in PRD §4)" |

## 7. Single inheritance manifest (the one place to read)

Put this table in `design.md` §3.1 verbatim. Every other reference in design.md/PRD to an inherited feature should point here, not to scattered prose.

```
# Inheritance manifest — agentloop does NOT re-implement these
# Each row: feature → owner → agentloop's action → the one line that resolves it

| Feature | Owner | agentloop's action | Resolving line |
|---|---|---|---|
| Model routing / combos / fallback | onegw `router.go` | caller only | `Router.Execute(ctx, tier)` per step |
| Token saving (RTK savers) | onegw `saver/` | caller only | onegw config, not agentloop code |
| Token/cost metering | onegw `usage/` | prices them | `CostTracker.estimate()` → settle on onegw usage |
| Request-level idempotency | onegw `idempotency/` | adds caller-key | `idempotency.go`: `sha256(run_id:tool:args)` persists before exec |
| Approval transport | xdev `tool/approval.go` | owns policy table | `ApprovalGate.check()` (agentloop) → xdev policy (transport) |
| Deferred tool catalog | xdev `tool/catalog.go` | never rebuilds | register tools via xdev's `Registry`; 5 v1 tools in PRD §4 |
| Agent turn execution | xdev `loop.go` (AgentBase) | drives xdev as a tool | `AgentBase.execute(tool, args)` — one call per tool, per step |
| Session persistence | xdev `session/store.go` | stores run/step data there | run/step rows are xdev session entries |
| Compaction ladder (agent level) | xdev `compact_ladder.go` | borrows pattern, not code | `CompactLadder.run()` between phases; never re-implements |
| Code graph / embeddings | leankg `core.go`, `embed/` | consumes via HTTP | `POST /api/v1/query` (L0–L3 ladder), `POST /api/v1/embed` |
| Memory banks | leankg `memory/` | reads/writes via HTTP | `/api/v1/memory/banks/{bank}/memories` |
| MCP server | leankg `mcp/server.go` | is a client, not a server | agentloop does NOT run an MCP server |
| Session offload (refs/canvas) | leankg `session/` | reads via HTTP | leankg owns the canvas; agentloop stores run-level metadata |
| Response compression (transport) | onegw `saver/` / leankg `compress/` | policy only, after transport | never LLM-summarize what the saver already shrunk |
| Context compression (agent level) | xdev `compact_ladder.go` | borrows pattern | 70% rule in agentloop = policy over xdev's compact |
| Goal tracking (session) | xdev `goal.go` | inherits goal_exhausted pattern | BudgetGuard adds per-run + daily economics on top |
| Multi-agent (session) | xdev `hub.go` | new run-level supervisor | M7 only, conditional on §10 gate |
```

## 8. Verification

This audit was verified against actual source listings of all three systems:

- **onegw:** `internal/router/`, `internal/provider/`, `internal/saver/`, `internal/idempotency/`, `internal/usage/`, `internal/quota/`, `internal/server/`, `internal/store/` — confirmed: routing, savers, metering, dedup, quota, admin. No agent loop, no tool policy, no eval, no kill switch, zero `price` in Go code.
- **xdev:** `internal/agent/loop.go`, `internal/agent/goal.go`, `internal/agent/hub.go`, `internal/agent/compact_ladder.go`, `internal/agent/fallback_chain.go`, `internal/tool/catalog.go`, `internal/tool/approval.go`, `internal/tool/tool_search.go` (implied via catalog), `internal/session/`, `internal/memory/`, `internal/ai/`, `internal/eval/` — confirmed: turn loop, goal tracking, session trees, hindsight, deferred tool catalog, tier-based approval, compaction ladder, code execution eval kernel (not agent eval).
- **LeanKG:** `internal/core/core.go` (3-tool surface, L0–L3 ladder), `internal/mcp/server.go` (3-tool MCP), `internal/store/` (SQLite/Postgres backend), `internal/embed/` (embedding sidecar), `internal/compress/` (RTK-style), `internal/memory/` (Markdown tree), `internal/session/` (offload), `internal/budget/` (per-tool token caps 800–6000) — confirmed: code graph, embeddings, memory, MCP server, transport compression, per-tool response caps (NOT run-level budgets).

One claim in design.md was **not** verified by source listing and needs confirmation before M1:
- **design.md §3.1 says xdev's `goal.go` is borrowed.** Verified: `xdev/internal/agent/goal.go` exists with the `Goal` struct, `TokenBudget`, and `budget_exhausted` terminal state. ✓

## 9. What to do next

1. **Cut** the three duplications in §3 before writing M1 code — specifically:
   - Do NOT write a `tool_search`/`tool_describe`/`tool_call` wrapper in agentloop. Consume xdev's registry through `AgentBase`.
   - Do NOT rewrite the approval gate. Own the policy table; call xdev's approval path as transport.
   - Do NOT re-implement RTK compression in agentloop. Route tool results through onegw's savers first, then apply agentloop's 70% policy.
2. **Add** the inheritance manifest (§7) to design.md §3.1 verbatim.
3. **Add** the "understated" fixes (§6) to design.md §3.1.
4. **Update** PRD §4/§4.1 to cross-reference this audit for provenance (the PRD already has the 5-tool table; add a pointer to `docs/DUPLICATION-AUDIT.md`).
5. **Verify** the "inherited" claim in design.md §3.1 for the approval gate and deferred catalog by checking whether xdev exposes its registry as an interface agentloop can call (file an issue in xdev if it doesn't — that is the acceptance gate for M1's ToolRegistry).

## 10. Bottom line

**agentloop has zero confirmed duplications that must be cut.** The three near-duplicates (§3) are all in the approval/catalog territory where xdev already provides the implementation and agentloop should consume rather than rebuild. Everything else agentloop does — LoopRunner, BudgetGuard, EvalRunner, Tracer, kill switch, cost pricing, cycle detection — is genuinely new and not present in any portfolio system. The book's framework is intact: onegw owns routing/metering, xdev owns the agent harness, LeanKG owns the code graph, and agentloop owns the loop's policy layer.

The risk is documentation, not code: design.md §3.1 undernames three borrows (deferred catalog, approval gate, compaction ladder) that a builder might re-implement out of habit. The inheritance manifest in §7 closes that gap.
