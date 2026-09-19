# agentloop TODO

## M1: Containment Core (PRD §13.1)

Status: **COMPLETE**

### Components
- [x] `LoopRunner` — bounded loop with 6 exits (MaxSteps, WallClock, CostBudget, DailyCeiling, CycleThreshold, ConfidenceFloor)
- [x] `ToolRegistry` — interface + 5 v1 tools (repo_search, repo_context, web_search, run_tests, write_file)
- [x] `BudgetGuard` — cost circuit breaker with pre-action check
- [x] Runs API: submit, poll, kill, SSE events
- [x] Kill switch (P75): operator-triggered halt within 250ms

### Bug fixes
- [x] Fix: `TestLive_KillRun` — routes() not registered on production mux; extracted shared `routes()` function

### Test results (M1)
| Package | Tests | Build | Vet |
|---|---|---|---|
| cmd/agentloop | 3 live HTTP | OK | OK |
| internal/loop | 6 | OK | OK |
| **Total (M1)** | **9 functions** | **OK** | **OK** |

## M2: Guards + Tracing (PRD §13.1)

Status: **COMPLETE**

### Components
- [x] `Tracer` nested spans (StartSpan/EndSpan/RunSpans/Spans)
- [x] `Replay` — Replay, ReplayResult.Diff, Validate
- [x] Runner wiring — tracer spans in Run(), cycle alert callback, 2K token cap
- [x] `ExitDailyBudget`/`ExitConfidenceFloor`/`ExitConsecutiveFailures` exits in Run()
- [x] `internal/loop/exitreason.go` — ExitReasons struct with M2+M3 reasons

### Test results (M2)
| Package | Tests | Build | Vet |
|---|---|---|---|
| cmd/agentloop | 3 live HTTP | OK | OK |
| internal/loop | 11 (incl. M2) | OK | OK |
| internal/replay | 4 | OK | OK |
| internal/tracer | 4 | OK | OK |
| **Total (M2)** | **22 functions** | **OK** | **OK** |

## M3: Planning + Tiering (PRD §13.1)

Status: **COMPLETE** — merged in PR #6 (commit `5b53513`)

### Components
- [x] `internal/planner/planner.go` — Planner/Replanner
  - [x] `Plan(cfg)` → 3–7 one-sentence steps with success criteria + dependency marks (FR-3)
  - [x] `Replan(done, confidenceFloor)` → binary CONTINUE/REPLAN (design.md §3)
  - [x] `ParallelPhases(plan)` → phase grouping for errgroup fan-out (P11)
  - [x] Per-step `Tier` field for routing through onegw combos
  - [x] `ValidateStepCount` test guard (3–7 band)
- [x] `internal/planner/planner_test.go` — 10 tests (plan shape, phases, tier/frame, replan continue/replan/nil, parallel phases, step-count validation)
- [x] Runner wiring — ExitReasons + tier routing + ceiling checks at step boundary

### Test results (M3)
| Package | Tests | Build | Vet |
|---|---|---|---|
| internal/planner | 10 | OK | OK |
| Total | **32 functions** | **OK** | **OK** |

## M2.x: TypeSafe guardrail screening (PRD §4.3, §7.2, §11.2 case 6)

Status: **EXPERIMENTED** — live API screen completed 2026-09-19; PRD updated; results in GitHub issue #8

### Components
- [x] `typesafe_benchmark.sh` — live A/B benchmark (with vs without TypeSafe), outputs comparison table + JSON
- [x] `internal/experiments/experiments_test.go` — testable check for routing logic (14 cases, strict/permissive/precedence/severity)
- [x] `typesafe_experiments.sh` — reproduces all 4 experiments against live `$TYPESAFE_API_KEY`
- [x] `internal/experiments/experiments.go` — `Route()` implementing cookbook strict/permissive routing
- [x] `docs/PRD.md` — §4.3 (TypeSafe row in separation-of-powers table), §5.2 (NFR-1b), §5.1 FR-2 extended, §7.2 (live scoring layer + 4 experimental findings), §11.2 case 6 (guardrail screen), §13.1 M2 extended, §14 (coupling + latency + threshold-cargo-culting risks), §17 (guardrail policies + cost as priors)
- [x] `docs/JEV-INTEGRATION.md` — §8 (guardrail screening results + verification checklist addition)

### Experiment results (live runs, jev-1.13.0, 2026-09-19)
| Run | Harmful containment | Notes |
|---|---|---|
| `typesafe_experiments.sh` (10 input, 5 output) | 5/5 inputs blocked/routed, 5/5 outputs blocked | Clear separation, high noul scores |
| `typesafe_benchmark.sh` (10 inputs) | 10/10 pass | Low noul scores this run — non-determinism finding |
| Direct probe (same prompts, 3×) | harmful_request 0.78–0.94, sev 2.1–2.8 | Confirms model is capable; noul scores vary per invocation |

**Variance finding**: `jev-1.13.0` returns non-deterministic noul scores across runs.
A single benchmark run is a point estimate; production deployment should consider this.
The `Route()` logic itself is deterministic and tested (`go test ./internal/experiments/...` — 14 cases, all pass).

### Production routing (blocker: onegw `feat/systemone-provider`)
- [x] `onegw.toml.example` — `systemone` provider + combo added
- [x] onegw `feat/systemone-provider` merges into onegw master — see agentloop PR #7
- [x] Connect Planner output into Run() loop (phase-based fan-out) — PR #7
- [x] Connect tier routing to onegw `/v1/chat/completions` — PR #7

## Next
- UI (M6) — HTMX console per `docs/UI-DESIGN.md`
