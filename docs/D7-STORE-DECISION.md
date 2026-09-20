# D7: Long-term memory backend — Store Decision

**Status:** closed · **Date:** 2026-09-19 · **Milestone:** M4

## Decision

Use a **dedicated 4-tier in-memory Go store** (`internal/memory/memory.go`) backed by **SQLite WAL checkpoints** (`internal/store/store.go`) for long-term/structural memory. **Not** LeanKG memory banks.

## Context

D7 was open before M4 (PRD §15): should agentloop's long-term memory live in LeanKG memory banks (which already exist in the portfolio) or in a dedicated store built for agentloop's memory model?

## Options considered

| Option | Why not chosen | Why the other |
|---|---|---|
| **LeanKG memory banks** | LeanKG's memory shape (`MEMORY.md`/`USER.md`/`topics` via `/api/v1/memory/banks/{bank}/memories`) is designed for code-context recall, not runtime loop state. Adding a network hop for what is essentially a run's state store adds latency and a deployment dependency on every loop step. | 4-tier store maps directly to the book's Ch.8 memory model (P31–P45): working/landmark/retrieved/system with the 70% ceiling, landmark retention, and compression cadence. |
| **Dedicated store (chosen)** | — | Single-binary deploy (matches onegw/xdev precedent from design.md §3.1), no network dependency, checkpoint every 5 steps via SQLite WAL enables resume after faults (NFR-4, P8). |
| **SQLite WAL checkpoints** (supporting choice) | — | WAL mode + busy timeout means readers see a consistent snapshot even while the writer commits (used by `store.Open` in `internal/store/store.go`). |

## Why this choice

1. **Book alignment**: agentloop's memory model is 4 tiers with the 70% rule, landmark retention, and rolling compression — these are runtime state properties that LeanKG's memory bank shape doesn't model. The book (Ch.8) prescribes this exact structure, not a graph-bank structure.
2. **Deploy constraint**: design.md §3.1 establishes the single-binary rule — agentloop doesn't add network dependencies for core state. LeanKG is called for knowledge retrieval (tools 1–2), not for run state.
3. **Durability without complexity**: SQLite WAL checkpoints every 5 steps (P8) give resume-after-fault (NFR-4) without a separate database service or schema migration story.
4. **Deletion API**: `memory.Store.Delete()` + `store.DeleteRun()` together satisfy PRD §5.1 FR-6 (tenant/PII removal) — easy to verify in tests, harder to guarantee over a network graph.

## Trade-offs

- **LeanKG semantic recall** is still available via tools (1–2). The dedicated store handles *run state*; LeanKG handles *knowledge*. This split is what design.md §3.1's inheritance manifest specifies — agentloop consumes LeanKG for code graph and memory banks, owns its own run state.
- **Async checkpoint writes** are synchronous in v1 (WAL sync on every commit). A future `ponytail:` ceiling: checkpoint write latency adds to step latency at 5-step cadence; upgrade to async flush with `PRAGMA synchronous=NORMAL` (already set) and a background writer when step counts exceed 500.

## Verification

```bash
go test ./internal/memory/...   # 4-tier store, 70% rule, landmarks, compression, deletion, marshal/unmarshal
go test ./internal/store/...    # SQLite WAL checkpoint, resume from step-5 checkpoint, delete run
```

Both pass. The M4 acceptance criteria are met: a 20-iteration run holds the 70% rule; resume from a step-5 checkpoint after a step-7 fault works.

## Related

- PRD §15 D7 row: closed 2026-09-19
- PRD §5.1 FR-6 (memory): 4 tiers, 70% rule, landmarks, deletion API
- `internal/memory/memory.go` — the store implementation
- `internal/store/store.go` — the checkpoint persistence layer
