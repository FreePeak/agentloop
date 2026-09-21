# agentloop — Product Requirements Document

The status/scope statement of work for **agentloop**: a Go service that runs
bounded, budgeted, observable **agent loops** on behalf of other products —
goal + budget in, answer / handoff / escalation out.

**Status:** draft for review · **Version:** 1.1.5 · **Date:** 2026-09-19

**The whole PRD lives in [`docs/PRD.md`](docs/PRD.md) (23 numbered sections + 4 appendices).** This README is the landing page;
`docs/PRD.md` is the canonical statement of scope.

| What | Where |
|---|---|
| **How to build, run, and operate it** | [`docs/USAGE.md`](docs/USAGE.md) |
| **System One (Jev hosted · Laya local)** | [`docs/JEV-INTEGRATION.md`](docs/JEV-INTEGRATION.md) |
| The PRD (23 sections, 4 appendices) | [`docs/PRD.md`](docs/PRD.md) |
| The canonical architecture this PRD summarizes | [`../design.md`](../design.md) — at repo root |
| The runnable check the ten review loops were run by | [`docs/check-prd.py`](docs/check-prd.py) |

## Using it

```bash
make            # list targets
make run        # build + serve on :8081 (onegw owns 8080)
make check      # tests + lint — before a PR
```

Submit a run and watch it go to work:

```bash
curl -sS -X POST localhost:8081/v1/runs -H 'Content-Type: application/json' \
  -d '{"goal":"explore the repository","context":"hello"}'
curl -sS localhost:8081/v1/runs/<run_id>       # => steps run, then a hold
```

Two things are true of that run. It **runs without asking** — the three
read-only tools (`query`, `web_search`, `run_tests`) never interrupt. And it
**holds on the first write**: `write_file` is approve-category, every call, so
approving it **resumes the run** from the held step (M5) instead of leaving it
in `paused_approval`. Full walkthrough — API, states, exit reasons, tiers,
approvals, and an honest list of what is still stubbed — in
[`docs/USAGE.md`](docs/USAGE.md).

---

## Quick orientation

- **Why this exists:** a request–response service cannot do work whose next step
  depends on the current one. A naive loop fails expensively (infinite iteration,
  cost explosion, drift, *success that is expensive and wrong*). The book's own
  domain chapters agree the bottleneck is never generation — it is context
  management, verification, and the integration surface. That is what v1 builds.
- **The two unconditional patterns:** a bounded loop (**P1**) and a kill switch
  (**P75**) are M1's definition, not hardening. Six exits, typed: step, wall-clock,
  dollar, confidence floor, progress stall, consecutive failures.
- **Every number is a prior:** nothing ships as a spec. Each default carries its
  source, and a monthly job re-derives it from our own traces (§17, §11.5).

## How to read the PRD

Two anchors that never move: §22 (the adversarial read) and §23 (the one-page summary). The 23-section anchors below point directly at `docs/PRD.md`.

| If you are | Read | Then |
|---|---|---|
| deciding whether to fund or build it | [§23 one-page summary](docs/PRD.md#23-appendix-g-one-page-summary) + §1.1–§1.3 | §15, starting with D0 |
| reviewing the design | §3 (incl. the draft §3.1), §4, §9 | [§22 adversarial read](docs/PRD.md#22-appendix-f-adversarial-read) |
| about to write code | **§6 (the data model + wire shapes)**, §13 + §13.1, §11.2, §17 | the cited `design.md` section; `docs/check-prd.py` tells you what it must contain |
| auditing the sources | §16, §18 | §20 (all 100 patterns accounted for) |
| asking why a number is what it is | §17 | §11.5 (how it changes) |

## Verified by a check, not by trust

```bash
python3 docs/check-prd.py            # the PRD asserts its own load-bearing promises
python3 docs/check-prd.py --selftest # proves the check can actually fail
```

`docs/check-prd.py` asserts 12 properties of `docs/PRD.md` (all passing, and its
own `--selftest` catches all 12 deliberate breakages): every internal §
reference resolves, all 100 App. B patterns are accounted for, every FR/NFR
carries a sourced why, no default row has a vague source, §6 exposes the shapes
a builder needs (state/exit enums, run/step/idempotency/approval rows, wire
error, SSE events with resume), the parity harness behind M3 exists, and the
provenance sweeps are recorded. It ships with 12 deliberate breakages it must
trip (`--selftest`).

## Repo & branch

This is a normal git checkout (no worktree) on branch `main` at the
parent `agentloop/` directory. Active feature branches live in
`.worktrees/` (e.g. `.worktrees/m5-hitl`); stale ones were removed.

## Ten review loops — the record

The PRD was not reviewed once. Each loop was a distinct adversarial question,
and each ended in a commit whose message names what it changed:

| # | Loop | Question it answered | Commit |
|---|---|---|---|
| 1 | Every load-bearing number cites its book source | Where did this figure come from? | `0e5bb69` |
| 2 | Architecture/decision paragraphs carry why + book ref | Is any claim orphaned? | `05c36d4` |
| 3 | Apply the book's closing sections (12 moves → milestones) | What has we never applied? | `05c36d4` |
| 4 | App. G template shapes + template-vs-toolset decision | Why these shapes, why not built? | `30ca1dc` |
| 5 | 100-pattern catalogue mined for gaps | Adopted / deferred with a trigger / rejected with a reason | `f1870fa` |
| 6 | Domain chapters (14–17) mined for the *why* of each loop | What would each chapter raise here? | `8342b3d` |
| 7 | Cross-check against `design.md` + onegw/xdev/LeanKG | Which claims are unverified? | `7cc6bb5` — fixed 4 wrong `design.md` pointers (§13→§15 API, §15→§17 build order, §16→§18 open questions), confirmed the `tiny`/`planning` tier reality and that `execution` is ours to add, and recorded the caveat that onegw does not yet route per step |
| 8 | Adversarial read — where the PRD would fail its own eval suite | What would a reviewer break first? | `6cce4d5` |
| 9 | Structure, redundancy, doc-of-record quality | Is every section needed? | `ad520af` |
| 10 | External review + verification script | Could a stranger build M1 from this? | `a45e546` |

Loop 10's external review found seven blockers (no run/step model, no status
enum, no error shape, wall-clock absent from the submit contract, two
idempotency mechanisms with one record of truth, confidence 0.7 doing two jobs,
memory ownership undecided, and Ch.12's recovery ladder gated nowhere). All
are fixed in the current `docs/PRD.md`; §22 records the review and its fixes.

Design drift during the loops, stated so it is not repeated: **loop 7** decided
the `execution` combo does not exist in onegw (the tiering is `tiny`/`planning`
today, `execution` is ours to add), corrected several section pointers into
`design.md` (§15 = API & data, §17 = build order, §18 = open questions), and
added the caveat that onegw does not yet route per step — agentloop picks the
combo per step from its own eval-gated table.
