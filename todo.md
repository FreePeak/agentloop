# agentloop — TODO index

The durable task record is **GitHub issues**; this file is the short index plus
the handoff notes a new session needs. Milestone records (M1–M7) live in
`docs/PRD.md` §13.1 and the commit history.

**Status as of `5b7859a` (2026-09-21):** the loop reads (LeanKG), writes and
verifies (xdev), **decides its own steps** (one model call per step, #34), routes
by tier on the wire (#35), and screens the goal and every tool result (#37). CI
exists (#31) but cannot run until the org's Actions billing limit is raised —
`make check` runs the identical five steps.

Priority bands (labels, defined 2026-09-21): **P0** blocks the loop from doing
real work at all · **P1** blocks a milestone's acceptance criteria · **P2**
improves an already-passing path · **P3** deferred by design, revisit on a named
trigger.

---

## Open issues

| Issue | Pri | Item |
|-------|-----|------|
| [#8](https://github.com/FreePeak/agentloop/issues/8) | **P1** | System One guardrails: the screen is wired (#37); its **evidence is stale** — live experiments not re-run since 2026-09-19, §11.2 case 6 not an explicit CI gate. Three of four checkboxes unticked, which is why it was reopened rather than closed |
| [#21](https://github.com/FreePeak/agentloop/issues/21) | P2 | onegw PR #110: verdict-driven combo **reorder** (leg choice behind a combo). No longer blocks tier routing — #35 landed that. Re-scoped in a comment |
| [#20](https://github.com/FreePeak/agentloop/issues/20) | P2 | M6: HTMX console per `docs/UI-DESIGN.md`. Pages are `fmt.Fprintf` HTML; no htmx anywhere |
| [#19](https://github.com/FreePeak/agentloop/issues/19) | P3 | M7: re-evaluate `EvaluateGate` when the registry passes 10 tools (it has 4) |
| [#15](https://github.com/FreePeak/agentloop/issues/15) | P3 | Agent-to-agent trust scoring via System One Noul — a design with evidence, no code |
| [#28](https://github.com/FreePeak/agentloop/issues/28) | P3 | Put `Closes #N` in PR bodies. Labels are done; closure is not |

---

## Build work with no issue, in the order it matters

These are the next milestone, not a backlog item. Ranked by what they unblock,
each verified against the code rather than assumed.

### 1. The loop cannot read a file. (biggest gap)

*Files:* `internal/tools/registry.go` (add the tool), `internal/tools/registry_impl.go`
(implement it over the existing `Sandbox *xdev.Client`), `internal/loop/reason.go`
(nothing needed — the reasoner picks from the registry automatically).

Four tools: `query` (graph), `web_search` (stub), `run_tests`, `write_file`.
**None returns file *content*.** `query` returns graph elements with a
400-character `content` excerpt and the rung that answered; it is not a reader.

So the reasoner can locate `parseConfig` and knows it lives in `config.go:41`,
and then **cannot look at it**. That is why the live demo writes
`func parseConfig() {}` — the model had nothing to read, so it invented a body.
`write_file`'s description already promises a "read twin"; it does not exist.

Fix: one `read_file` tool (path, optional line range) over the same xdev client
that already backs `write_file`, or an xdev turn whose single job is "print this
file". Then update `query`'s and `write_file`'s `DO NOT USE WHEN` text, which
currently point at a tool that is not in the registry.

### 2. The run has no answer. (makes "goal_met" meaningful)

*Files:* `internal/loop/runner.go` (`RunResult`), `internal/loop/model.go`
(where `ExitReason` is known), `cmd/agentloop/main.go` (the API response),
`docs/USAGE.md` §5 (state vs answer).

On the `goal_met` exit the loop calls `synthesize` and stores the result in
`PartialSynthesis` — a field named for the *bound* case (a run cut short by a
budget wants "here is what I got"). A run that finished says its answer in
`partial_synthesis`, and the model's `done` rationale is buried in a step's
`why` (`Tool: "(none)"`).

Fix: a dedicated `Answer` on `RunResult`, set from the synthesis when
`ExitReason == goal_met`, and `PartialSynthesis` reserved for the bound exits.
Console and API should show `answer` first.

### 3. Success criteria are prose, never evaluated.

*Files:* `internal/planner/planner.go` (`successCriteria`), `internal/loop/runner.go`
(the `evaluate` phase), `docs/PRD.md` §13.1 move 2.

`planner` emits `Success: "Action completed without error"` per step. The runner
reads only the step's *tier* from the plan; `ps.Success` appears exactly once in
the codebase — inside the reasoner's prompt as text. Nothing evaluates it.

Consequence: the loop's only stopping signal is the model saying `done`.
Fix: make the criterion a checkable predicate for the phases where that is
possible (`act` → the tool succeeded; `evaluate` → the caller's predicate), and
say plainly in the PRD that the rest stay advisory.

### 4. `web_search` is the last stub.

*Files:* `internal/tools/registry_impl.go`, or the tool entry in `registry.go` if it
is deleted. Check onegw's `kind = "searxng"` provider exists before building it.

Its description claims `onegw provider kind=searxng`; no client exists. Either
add the onegw call (same shape as `internal/onegw`) or delete the tool — a
fourth tool that returns `{"results":[]}` costs a model call and a schema on
every step and can never answer anything.

### 5. Two gaps #37 recorded for the guardrail.

- the model's **reply** is not screened on the way out (goal and tool-result
  directions are);
- the measured **~740 ms/call** is not counted in `BudgetGuard`, which the PRD
  budgets as a per-step cost.

### 6. Planning is still a rule table.

`planStepCount(words)` (`words := len(strings.Fields(cfg.Goal))`) picks 3–7 steps and
`instructionForPhase` fills them with "Execute the next action for: <goal>". The
*chooser* (#34) carries runs, so this is no longer blocking — but M3's row still
describes tiered P&E, and the plan's phases reach the model only as prompt text.

---

## How to work in this repo

- **Never edit the main checkout.** `git fetch origin && git worktree add
  .worktrees/<slug> -b <type>/<slug> origin/main`; absolute paths for every edit.
  Finish with commit → push → PR → report the URL.
- `make check` is the gate: gofmt (`make fmt-check`), `go vet`, `go test ./...`,
  `golangci-lint` (pinned in `.golangci.yml`), `python3 docs/check-prd.py`.
  CI runs the same five steps plus `--selftest`.
- **Verify by breaking the thing you just wrote**, then watching the test fail.
  Every defect this repo has shipped lately was a check that could not fail:
  #25 (a gate that held everything), #29 (a checker that could not run), #32 (an
  eval suite reporting "blocked" while green), #37 (a screen nothing turned on),
  and #8's checklist (ticked on the author's behalf). Prefer a test that names
  the failure over a test that asserts a success.
- When you fix a claim, **check the docs that assert it** — PRD, USAGE and
  README have each gone stale within a day. The PRD footer takes one live
  `* Last updated:` stamp; older ones go into the dated changelog below it.
- Shell quirks that cost time here: a backgrounded server dies unless launched
  with `nohup env … &`; `pkill -f` on a name matching your own command line
  kills your shell; ports 8080 (onegw) and 9699 (leankg MCP) are taken.

## Closed

- M1–M7 implemented (`docs/PRD.md` §13.1); M7 gated shut by design until §10's
  criteria are met.
- #27 CI, #12 gate-in-runner, #13 TypeSafe screen + tier routing, #14 D7 store
  decision, #4 build plan, #11 multi-agent.
- Landed recently: #31 CI · #32 eval gate · #33 xdev execution · #34 the
  reasoner · #35 tier routing on the wire · #37 the guardrail screen.
