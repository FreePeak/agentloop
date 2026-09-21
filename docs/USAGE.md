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
| HITL approval gate wired into the runner | **implemented and working**: the gate holds a write, and approving it **resumes** the run. The read-only three tools never interrupt; `write_file` always holds — §3, §9 |
| HTTP API, admin console, eval harness | **implemented** |
| Model calls to onegw | **partial** — one outbound call, at synthesis (§2, §9). The loop's *planning* still makes none |
| The four built-in tools | **one real, three stubs** — `query` reaches LeanKG and returns its envelope; `web_search`, `run_tests`, `write_file` return canned results whose message says `stub` (`internal/tools/registry_impl.go`) |
| The planner | **deterministic**, no model calls; model-driven planning is the documented production path |
| M7 multi-agent (`internal/supervisor`) | **gated shut** by design — refused unless one of [PRD §10](PRD.md#10-multi-agent-stance)'s four conditions is met |

**What this means:** a run today exercises the real loop, budget, gate, and
observability machinery end to end — it stops on its own bounds and it holds
only on a write — but it does no real work, because the tools return empty
results. It is a harness you can develop against, not yet an agent that fixes
your code. The honest next steps are listed at the end.
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

### Configuration the binary reads

Deployment facts, not compiled defaults:

| Variable | Default | Meaning |
|---|---|---|
| `AGENTLOOP_PORT` | `8080` (the Makefile passes 8081) | listen port |
| `AGENTLOOP_ONEGW_URL` | `http://127.0.0.1:8080` | gateway base URL |
| `AGENTLOOP_ONEGW_KEY` | *(empty)* | bearer key; empty sends **no** `Authorization` header |
| `AGENTLOOP_ONEGW_COMBO` | `dev` | combo name, used as the wire `model` |
| `AGENTLOOP_LEANKG_URL` | `http://127.0.0.1:8090` | LeanKG REST root behind the `query` tool |
| `AGENTLOOP_LEANKG_OFF` | *(unset)* | any value disables the knowledge client; `query` then says no service is configured |

Take the onegw key from `onegw.toml`'s `[auth] [[auth.keys]]`; the combo must
exist there too, since the client sends whatever name you give it and onegw
rejects an unknown one. **The port clash is yours to handle:** onegw owns
`127.0.0.1:8080`, so leave `AGENTLOOP_ONEGW_URL` alone and change `PORT`.

Both outbound dependencies degrade the same way — a run still completes. A
gateway that is unreachable falls back to the deterministic partial and records
`synthesis_error`; a LeanKG that is down records the transport error on the step
and the loop keeps its bounds.

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

**Expect a run that actually runs.** The gate categorizes tools by name
(`internal/loop/approval.go:26`). The read-only three — `query`, `web_search`,
`run_tests` — are `CatAuto` and never interrupt; `write_file` is `CatApprove`
and holds **fail-closed**. So a read-only goal exhausts its step budget on its
own, and the first write is where a human appears.

Approve it:

```bash
# see what is waiting
curl -sS localhost:8081/v1/runs/9f2c…/approvals | python3 -m json.tool

# approve step 0
curl -sS -X POST localhost:8081/v1/runs/9f2c…/approvals \
  -H 'Content-Type: application/json' -d '{"step_id":0}'
# => {"approved":true}
```

**The decision lands in the audit ledger, and the run resumes.** Approving a
held step flips its record from `deny` to `approve`, marks the key so the gate's
re-check honours it (`internal/loop/approval.go:152`), and re-enters the loop
through `runner.Resume` (`cmd/agentloop/main.go:218`, `:243`). Without that
resume leg the run sat in `paused_approval` forever — that gap is closed.

Each `write_file` step holds on its own: the loop pauses again at the *next*
write, not the same one, so a run with three writes costs three approvals. Both
approval forms work: `POST /v1/runs/{id}/approvals` with a body, or
`POST /v1/runs/{id}/approvals/{step_id}`.

> `make smoke` submits a run but never approves it, so if that run reached a
> write step it sits in `paused_approval`; a read-only goal finishes on its own.

Requests accept `goal` (required), `context`, `max_steps`, and `cost_budget`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/runs` | submit a run → `201` + `{run_id, state}` |
| `GET` | `/v1/runs/{id}` | run result: state, exit reason, steps |
| `POST` | `/v1/runs/{id}/kill` | sets state `killed` in the store (see §9: the live runner is not signalled) |
| `GET` | `/v1/runs/{id}/events` | SSE stream: `event: step` …, `event: done` |
| `POST` | `/v1/runs` | submit a run → `201` + `{run_id, state}` |
| `GET` | `/v1/runs/{id}` | run result: state, exit reason, steps |
| `POST` | `/v1/runs/{id}/kill` | stamp `killed` **and** signal the live runner (§9) |
| `GET` | `/v1/runs/{id}/events` | SSE stream: `event: step` …, `event: done` |
| `DELETE` | `/v1/runs/{id}` | forget a run → `204`, then `404` |
| `DELETE` | `/v1/runs/{id}/memory` | erase a run's memory → `204`, then `404` |
| `GET` | `/v1/runs/{id}/approvals` | pending approvals + current state |
| `POST` | `/v1/runs/{id}/approvals` | approve by body `{"step_id":N}` |
| `POST` | `/v1/runs/{id}/approvals/{step_id}` | approve by path |
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

- **Approval holds, and resumes.** Both approval handlers call `runner.Resume`
  after `gate.Approve` returns true, so an approved step re-enters the loop and
  the gate's re-check honours the recorded key
  (`internal/loop/approval.go:152`, `cmd/agentloop/main.go:218`, `:243`).
- **Every `write_file` holds.** `Categorize` keeps the writer in `CatApprove`
  deliberately (the function's comment says why): §7.3 wants irreversible writes
  behind confirmation, and the runner's confidence signal is step success, not a
  model's — so there is nothing for `auto_if_confident` to hang on yet.
- **Synthesis is the only model call.** The loop reaches onegw once, at the end
  of a bound run, through `internal/onegw`. Planning does **not**: the planner is
  still the deterministic rule table, so a run's *steps* are chosen without a
  model. A deploy with no gateway reachable behaves exactly as before, because
  the deterministic partial remains the fallback.
- **The kill endpoint reaches a live run.** `POST /v1/runs/{id}/kill` stamps the
  stored state *and* signals the runner's kill channel, which `Run()` checks at
  every step boundary. The stop is bounded by one step, not instant — a step
  already in flight finishes first.
- **Three of the four tools are stubs.** `query` is **real**: it reaches LeanKG
  `POST /api/v1/query` and reports the retrieval rung that answered.
  `web_search` has no client, and `run_tests`/`write_file` should go through xdev
  rpc in a restricted workspace. Each stub says so in its result message, so a
  step that did nothing cannot be read as one that worked.
- **Retrieval needs a LeanKG to point at.** `AGENTLOOP_LEANKG_URL` (default
  `http://127.0.0.1:8090`); `AGENTLOOP_LEANKG_OFF=1` disables the client, and the
  tool then says no knowledge service is configured. A LeanKG that is *down* is
  an observation, not a crash: the step records the reason and the run keeps its
  bounds.
- **Tier routing is half-wired** — see §6.
- **M7 is gated shut**, correctly: the gate is a measurement, not a milestone,
  and it opens only when a [PRD §10](PRD.md#10-multi-agent-stance) condition is
  actually met.

In rough order: a **model-driven planner** (`query` retrieval and onegw
synthesis both exist now; planning is the last deterministic piece), then
xdev-rpc execution for `run_tests`/`write_file`, and with it the tier
propagation of §6.
