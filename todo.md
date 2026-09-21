# agentloop — TODO index

The open items that used to live here have been promoted to GitHub issues.
This file is now a short index; the milestone records (M1–M7) live in
`docs/PRD.md` §13.1 and the commit history.

Priority bands (labels, defined 2026-09-21): **P0** blocks the loop from doing
real work at all · **P1** blocks a milestone's acceptance criteria · **P2**
improves an already-passing path · **P3** deferred by design, revisit on a named
trigger.

## Open issues

| Issue | Priority | Item |
|-------|----------|------|
| [#27](https://github.com/FreePeak/agentloop/issues/27) | **P1** | CI: nothing runs `go test` on a PR — M6's "deploys blocked on the suite" is not enforced |
| [#21](https://github.com/FreePeak/agentloop/issues/21) | **P1** | onegw PR #110: verdict-driven combo reorder for agentloop tier routing |
| [#20](https://github.com/FreePeak/agentloop/issues/20) | P2 | M6: HTMX console per `docs/UI-DESIGN.md` |
| [#8](https://github.com/FreePeak/agentloop/issues/8) | P2 | TypeSafe guardrail screening experiments + containment proposal (the `Route()` half is implemented; the checklist is not ticked) |
| [#19](https://github.com/FreePeak/agentloop/issues/19) | P3 | M7: re-evaluate `EvaluateGate` when the tool registry grows past 10 |
| [#15](https://github.com/FreePeak/agentloop/issues/15) | P3 | Agent-to-agent trust scoring via TypeSafe Noul |
| [#28](https://github.com/FreePeak/agentloop/issues/28) | P3 | Tracker hygiene: priority labels + close-on-merge |

## Not tracked as issues, by design

The three remaining stub tools (`web_search`, `run_tests`, `write_file`), the
model-driven planner, and tier-combo names are **build work with a named owner
and source**, described in `docs/PRD.md` §4 and `docs/USAGE.md` §9 rather than
filed as tickets. They are the next milestone, not a backlog item — file them as
issues when the milestone is opened.

## Closed

- M1–M7 are all implemented (see `docs/PRD.md` §13.1). M7 is gated shut by
  design until §10's criteria are met.
