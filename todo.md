# agentloop — TODO index

The open items that used to live here have been promoted to GitHub issues.
This file is now a short index; the milestone records (M1–M7) live in
`docs/PRD.md` §13.1 and the commit history.

Priority bands (labels, defined 2026-09-21): **P0** blocks the loop from doing
real work at all · **P1** blocks a milestone's acceptance criteria · **P2**
improves an already-passing path · **P3** deferred by design, revisit on a named
trigger.

CI exists as of 2026-09-21 (`.github/workflows/ci.yml`): a PR that breaks the
build, the tests, the lint or the PRD now shows a red check — #27 closed.

> **The workflow needs Actions minutes.** The repo is private, so the jobs fail
> at dispatch on a billing limit until the org raises it; `make check` runs the
> identical five steps locally in the meantime (both caveats are in
> `docs/USAGE.md` §2).

## Open issues

| Issue | Priority | Item |
|-------|----------|------|
| [#21](https://github.com/FreePeak/agentloop/issues/21) | **P1** | onegw PR #110: verdict-driven combo reorder (System One pre-route UC-4; backends Jev/Laya behind onegw) |
| [#20](https://github.com/FreePeak/agentloop/issues/20) | P2 | M6: HTMX console per `docs/UI-DESIGN.md` |
| [#8](https://github.com/FreePeak/agentloop/issues/8) | P2 | System One guardrails (Jev/Laya): `Route()` done; wire screen + Laya sidecar parity + Jev↔Laya corpus — see `docs/JEV-INTEGRATION.md` |
| [#19](https://github.com/FreePeak/agentloop/issues/19) | P3 | M7: re-evaluate `EvaluateGate` when the tool registry grows past 10 |
| [#15](https://github.com/FreePeak/agentloop/issues/15) | P3 | Agent-to-agent trust scoring via System One Noul (Jev or Laya local) — UC-3 in `docs/JEV-INTEGRATION.md` |
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
