# agentloop M2 TODO

## M2: Guards + Tracing (PRD §13.1)

Status: **IN PROGRESS → COMPLETE**

### Components
- [x] `internal/tracer/` — Tracer nested spans (StartSpan/EndSpan/RunSpans/Spans)
- [x] `internal/replay/` — replay.Replay, ReplayResult.Diff, Validate
- [x] Runner wiring — tracer spans in Run(), cycle alert callback, 2K token cap
- [x] `internal/loop/m2_test.go` — 6 M2 tests (tracer wiring, cycle alert, 3-layer trace diff, 2K cap, validate, kill span)
- [x] `internal/tracer/tracer_test.go` — 4 tracer tests (nested spans, run spans, separation, thread safety)
- [x] `internal/replay/replay_test.go` — 4 replay tests (structure, valid, cycle detection, diff)
- [x] `cmd/agentloop/main_test.go` — 3 live HTTP tests (submit+poll, kill, SSE events)

### Bug fixes
- [x] Fix: `submitRun` goroutine used `r.Context()` (cancels on handler return) → `context.Background()` so background runs aren't killed prematurely

### PRD §11.2 coverage
- [x] Case 1 — runaway halts (M1, re-verified)
- [x] Case 2a — same run_id dedup (M1, re-verified)
- [x] Case 3 — budget fires at 90% (M1, re-verified)
- [x] Case 4 — kill switch (M1 + M2 span recording)
- [x] Case 5 — injection safe (M1, re-verified)

### Test results
- `go test ./... -count=1` → all pass (25 test functions)
- `go build ./...` → clean
- `go vet ./...` → clean
- Live HTTP tests → pass (httptest, no port conflict)

### Next: M3 (deferred)
- Planning + Tiering through onegw
