# Using agentloop

An operator's guide: how to build it, run it, drive its HTTP API, and read what
it tells you. The design rationale lives in [`PRD.md`](PRD.md) — this file is
the hands-on half.

---

## What this is (and what it is not)

**agentloop is the policy plane.** It owns a bounded agent loop — step ceiling,
wall-clock, dollar budget, confidence floor, progress stall, consecutive
failures — plus the human-approval gate, the kill switch, memory, and checkpoint
resume. Those bounds are enforced in code, not by asking a model nicely.

It is **not** the model transport. Model calls belong to
[onegw](../../onegw), which is the single gateway for every provider in this
portfolio; xdev owns sandboxed execution; LeanKG owns the code graph.

```
your caller ──▶ agentloop  (policy: tiers, budget, approval, kill)
                     │
                     ▼
                   onegw    (transport: which provider, fallback, usage)
                     │
                     ▼
            DeepSeek / kilo-code / TypeSafe JEV / …
```

### Honest status

Read this before you plan work around it. As of this writing:

| Area | State |
|---|---|
| Loop bounds, six typed exits, kill, memory, checkpoints | **implemented and tested** |
| HITL approval gate wired into the runner | **implemented, and it holds** (PR #16 fixed a wiring bug where the gate was built but passed as `nil`). The *hold* works; approval is recorded but does **not** resume the run — §3, §9 |
| HTTP API, admin console, eval harness | **implemented** |
| Model calls to onegw | **not yet** — there is no outbound client in the loop path |
| The four built-in tools | **stubs** — each returns a canned `Success: true` (`internal/tools/registry_impl.go:44`) |
| The planner | **deterministic**, no model calls; model-driven planning is the documented production path |
| M7 multi-agent (`internal/supervisor`) | **gated shut** by design — refused unless one of [PRD §10](PRD.md#10-multi-agent-stance)'s four conditions is met |

**What this means:** a run today exercises the real loop, budget, gate, and
observability machinery, but it does no real work — the tools return empty
results. It is a harness you can develop against, not yet an agent that fixes
your code. The honest next steps are listed at the end.

---

## 1. Prerequisites

- **Go 1.25+** (the module targets `go 1.25.14`)
- `golangci-lint` on `PATH` for `make lint`
- `python3` for `make prd`
- **A free port.** `onegw` binds `127.0.0.1:8080` on this machine, and
  agentloop's own default is *also* 8080 — so run agentloop on 8081 (the
  Makefile's default) or any other free port.

## 2. Build and run

```bash
make            # list targets
make build      # -> ./agentloop (gitignored)
make run        # builds, then serves on :8081
make check      # tests + lint, before a PR
make prd        # the PRD asserts its own promises (12 properties)
```

| Target | What it does |
|---|---|
| `build` | `go build -o agentloop ./cmd/agentloop` |
| `run` | builds, then runs with `AGENTLOOP_PORT=$(PORT)` (default 8081) |
| `test` | `go test ./...` |
| `check` | `test` + `lint` |
| `lint` / `vet` / `fmt` | `golangci-lint` / `go vet` / `go fmt` |
| `tidy` | `go mod tidy` |
| `prd` | `python3 docs/check-prd.py` |
| `smoke` | submits one run and prints the response |
| `clean` | removes the built binary |

Override the port: `make run PORT=9090`.

## 3. A first run, end to end

Submit a goal. The server returns immediately with a run id and runs the loop in
the background:

```bash
curl -sS -X POST localhost:8081/v1/runs \
  -H 'Content-Type: application/json' \
  -d '{"goal":"smoke test","context":"make smoke"}'
# => {"run_id":"9f2c…","state":"thinking"}
```

Poll it:

```bash
curl -sS localhost:8081/v1/runs/9f2c… | python3 -m json.tool
```

**Expect `"state": "paused_approval"` — not completion.** This is the important
first lesson about how agentloop behaves today. The gate categorizes tools by
name (`internal/loop/approval.go:27`) and only `read`/`search`/`list`/`get` run
automatically; anything unrecognized falls to `CatApprove` **fail-closed**. The
four built-in tool names (`query`, `web_search`,
`run_tests`, `write_file`) all land in that default branch, so a gated run pauses
on its very first step and waits for you.

Approve it:

```bash
# see what is waiting
curl -sS localhost:8081/v1/runs/9f2c…/approvals | python3 -m json.tool

# approve step 0
curl -sS -X POST localhost:8081/v1/runs/9f2c…/approvals \
  -H 'Content-Type: application/json' -d '{"step_id":0}'
# => {"approved":true}
```

Approve by path instead, if you prefer: `POST /v1/runs/{id}/approvals/{step_id}`.

**What approving does — and does not do.** The decision lands in the audit
ledger: step 0's record flips from `deny` to `approve` with reason `"operator
approved"`. That is the whole effect. **The run does not resume.** `Run()`
returned the moment the gate held (`internal/loop/runner.go:365` returns
`result, nil`), so the stored result is still `paused_approval` with `steps: 0`
and `spend_usd: 0`. Both approval handlers only record the decision and answer
`{"approved":true}` (`cmd/agentloop/main.go:197`, `:211`) — neither re-invokes
the loop, so the run never picks up again.

The resume *machinery* does exist: `prepareResume()` (`internal/loop/m4.go:36`)
loads the run's checkpoint and sets the start step, and `Run()` calls it
(`internal/loop/runner.go:268`). What is missing is the leg that connects an
approval to it. And even re-invoked, the step would be denied once more:
`CatApprove` records `deny` and holds on every pass
(`internal/loop/approval.go:144`) and never consults an earlier approval.

So operator approval today is a **record, not a resume**. Closing the loop means
two small changes: have `Check` honor an existing approval for the same run and
step, and re-invoke `Run()` from the checkpoint after `Approve` succeeds. See §9.

> `make smoke` submits a run but does not approve it, so the run it creates sits
> in `paused_approval`. That is the intended behavior, not a bug.

## 4. HTTP API

Requests accept `goal` (required), `context`, `max_steps`, and `cost_budget`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/runs` | submit a run → `201` + `{run_id, state}` |
| `GET` | `/v1/runs/{id}` | run result: state, exit reason, steps |
| `POST` | `/v1/runs/{id}/kill` | sets state `killed` in the store (see §9: the live runner is not signalled) |
| `GET` | `/v1/runs/{id}/events` | SSE stream: `event: step` …, `event: done` |
| `DELETE` | `/v1/runs/{id}` | forget a run → `204`, then `404` |
| `DELETE` | `/v1/runs/{id}/memory` | erase a run's memory → `204`, then `404` |
| `GET` | `/v1/runs/{id}/approvals` | pending approvals + current state |
| `POST` | `/v1/runs/{id}/approvals` | approve by body `{"step_id":N}` |
| `POST` | `/v1/runs/{id}/approvals/{step_id}` | approve by path |

Admin / operator console:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/admin/api/v1/runs` | all runs as JSON |
| `GET` | `/admin/api/v1/evals` | eval report |
| `GET` | `/admin/console/runs` | HTML run console |
| `GET` | `/admin/console/approvals` | HTML approvals console |
| `POST` | `/admin/console/kill` | kill by body `{"run_id":"…"}` |

Stream events with curl:

```bash
curl -N localhost:8081/v1/runs/9f2c…/events
```

## 5. States and exit reasons

A run's `state` says **where** it ended; `exit_reason` says **why**. They are
deliberately separate.

| State | Meaning |
|---|---|
| `queued`, `thinking`, `acting`, `evaluating` | in flight |
| `paused_approval` | held by the gate — needs a human |
| `success` | finished |
| `exhausted` | hit a bound (see exit reason) |
| `failed` | errored |
| `escalated` | handed off |
| `killed` | stopped by operator |

Exit reasons: `max_steps`, `wall_clock`, `cost_budget`, `daily_budget`,
`confidence_floor`, `progress_stall`, `consecutive_failures`,
`guardrail_block` (the TypeSafe screen refused).

Defaults, each a sourced prior rather than a guess (`internal/loop/exitreason.go`):
`max_steps` 9, `wall_clock` 120s, `cost_budget` $1.00/run, daily ceiling 20× that.

## 6. Model tiers

The runner routes each step to a **tier**, and a tier is an onegw *combo name* —
`planning`, `execution`, `tiny`. The mapping agreed for this portfolio:

| Tier | Role | Model |
|---|---|---|
| `planning` | reasoning | `opencode/deepseek-v4.1-flash` |
| `execution` | coding | `kilocode/kilo-auto/free` |
| `tiny` | execution + classify | TypeSafe JEV |

Two things to know before you rely on this:

1. **The combos must exist in onegw.** They are declared in `onegw.toml` as
   `[[combo]]` blocks named after the tiers. Until they are added, a tier
   resolves to nothing.
2. **agentloop emits only `tiny` today.** `internal/loop/runner.go:262` builds
   the plan with a hardcoded empty tier, and the planner defaults an empty tier
   to `tiny` — so `planning` and `execution` are currently unreachable in
   production. Selection can still be driven by a client sending the combo name
   as its model string; wiring the tier through `RunnerConfig` is the small
   change that makes the switch live.

xdev selects these by name too — its `~/.xdev/agent/models.yml` carries pinned
`planning` / `execution` / `tiny` rows, so `xdev models` lists them and
`-model onegw/planning` works once the combos exist upstream.

## 7. Memory and checkpoints

A run keeps four tiers of memory and checkpoints its step index to SQLite (WAL),
so an interrupted run resumes from where it stopped. `DELETE /v1/runs/{id}/memory`
erases a run's memory and drops its record, after which `GET` returns `404`.

The store decision — in-process tiers plus SQLite WAL, not LeanKG — is recorded
in [`D7-STORE-DECISION.md`](D7-STORE-DECISION.md).

## 8. Development

```bash
make check                  # the gate: tests + lint
go test ./internal/loop/ -run TestM5_GateRegisteredForNewRun -v
```

Package map:

| Package | Owns |
|---|---|
| `internal/loop` | the runner, six exits, approval gate, states |
| `internal/planner` | deterministic plan + per-step tiers |
| `internal/tools` | the tool registry (stubs today) |
| `internal/budget` | per-run and daily guards |
| `internal/memory`, `internal/store` | four-tier memory, SQLite checkpoints |
| `internal/tracer`, `internal/replay` | spans, replay |
| `internal/eval` | eval harness and deploy gate |
| `internal/experiments` | the TypeSafe guardrail screen |
| `internal/supervisor` | M7 multi-agent, gated by PRD §10 |

## 9. What is not built yet

Stated plainly, so nobody discovers it the hard way:

- **Approving does not resume a run.** The gate holds correctly, and the resume
  machinery exists (`prepareResume`, `internal/loop/m4.go:36`, called at
  `internal/loop/runner.go:268`) — but nothing connects an approval back to it.
  `Run()` returns when it holds (`runner.go:365`) and neither approval handler
  re-invokes it (`cmd/agentloop/main.go:197`, `:211`). `Check` also never
  consults an earlier approval: `CatApprove` records `deny` and holds on every
  pass (`internal/loop/approval.go:144`). Since all five built-in tool names are
  approve-category, a gated run therefore **cannot** make progress. This is the
  largest functional gap: HITL today is an audit record, not a gate you can pass
  through.
- **No outbound model client.** The loop never calls onegw. Everything above
  runs against stubbed tools and a deterministic planner.
- **The four tools are stubs.** `query` should reach LeanKG `POST /api/v1/query`;
  `run_tests`/`write_file` should go through xdev rpc in a restricted workspace.
- **The kill endpoint does not reach a live run.** `POST /v1/runs/{id}/kill`
  rewrites the stored state to `killed` (`cmd/agentloop/main.go:114` sets
  `result.State = StateKilled`, `:128` writes it back), but the handler never
  calls `runner.Kill()`. That method sits unused by the service
  (`internal/loop/runner.go:235`) even though `Run()` checks the kill channel at
  every step boundary (`runner.go:280`). The stop is real for a run that has
  already returned — as every gated run has, since all five tools are
  approve-category — and cosmetic for one still working: the operator sees
  `killed` while the loop keeps going and overwrites the state on completion.
- **Tier routing is half-wired** — see §6.
- **M7 is gated shut**, correctly: the gate is a measurement, not a milestone,
  and it opens only when a [PRD §10](PRD.md#10-multi-agent-stance) condition is
  actually met.

In rough order: **the approval→resume path** (make `Check` honor an existing
approval, then re-enter the loop from the checkpoint), **the kill endpoint**
(call `runner.Kill()` so a working run stops at its next step boundary), a real
onegw client and tier propagation, then LeanKG-backed retrieval, then xdev-rpc execution.
