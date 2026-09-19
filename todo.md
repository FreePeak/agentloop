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

### Production routing (blocker: onegw `feat/systemone-provider`)
- [x] `onegw.toml.example` — `systemone` provider + combo added
- [ ] onegw `feat/systemone-provider` merges into onegw master
- [ ] Connect Planner output into Run() loop (phase-based fan-out via errgroup)
- [ ] Connect tier routing to onegw `/v1/chat/completions`

## Next
- UI (M6) — HTMX console per `docs/UI-DESIGN.md`
