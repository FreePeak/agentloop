# Jev provider — integration guide for agentloop

**Status:** merged (systemone Kind) · routing still open (PR #110) · **Date:** 2026-09-21
**Repo:** `github.com/FreePeak/agentloop`
**Onegw branch:** `feat/systemone-provider` — the provider `Kind` is **merged into master**; the
  verdict-driven combo reorder (PR #110) is **not**. See §3.1.
**Onegov doc:** [`../onegw/docs/ARCHITECTURE.md`](../onegw/docs/ARCHITECTURE.md) — Jev is now listed there.
**Onegov doc:** [`../onegw/docs/ARCHITECTURE.md`](../onegw/docs/ARCHITECTURE.md) — Jev is absent from it today.

---

## TL;DR

**Jev goes to onegw. Not xdev. Not agentloop.** It is a model provider (a
`Kind` in onegw's provider table, `KindSystemOne`, serving `POST /v1/systemone`
over the same OpenAI Chat Completions wire on both sides), and onegw is the
single owner of model transport in this portfolio. Agentloop never touches
Jev directly — it already sends a *tier* per step and onegw already picks the
leg. Adding Jev is a onegw config change (`[[providers]]` block with
`kind = "systemone"`) plus a combo that selects it; agentloop picks it up
with **zero agentloop changes**.

```
agentloop (policy: which tier, what budget, when to stop)
    │  sends tier per step
    ▼
onegw (transport: picks the leg, saves tokens, records usage, enforces quota)
    │  POST /v1/systemone    ← Jev lives here
    ▼
TypeSafe Jev upstream
```

If xdev is to *consume* Jev, it reaches it through onegw exactly the way it
reaches every other model (`onegw/systemone` selector in `models.yml`) — see
[xdev side](#xdev-side-consumer-only) below.

---

## 1. What Jev is

TypeSafe's model API. Already implemented in onegw as a first-class provider
kind, alongside `openai`, `anthropic`, `gemini`, `opencode`, `commandcode`,
`cursor`, `cline`:

| File (onegw `feat/systemone-provider`) | What it does |
|---|---|
| `internal/provider/kinds_wire.go:52` | `KindSystemOne Kind = "systemone" // TypeSafe Jev model (POST /v1/systemone, same wire on both sides)` |
| `internal/provider/provider.go:2156` | `case KindSystemOne: return "/v1/systemone"` — path resolver for the model catalog |
| `internal/provider/provider.go:2245` | dispatch case: *"TypeSafe Jev endpoint: same OpenAI Chat Completions wire on both sides, so the body is forwarded verbatim and the response passed through unchanged"* |
| `internal/provider/systemone.go` | `doSystemOne` — Bearer auth via `Authorization`, body read whole (non-streaming), returns `{model, answers, usage}`, or a typed upstream `APIError` |
| `internal/server/server.go:505` | `mux.HandleFunc("POST /v1/systemone", s.withIdempotency(translat.FmtSystemOne, s.handleSystemOne))` — registered with idempotency like `/v1/chat/completions` |
| `internal/translat/openai.go:32` | `FmtSystemOne Format = "systemone"` — a wire format constant |
| `internal/provider/provider.go:239` | `KindSystemOne.Format()` → `translat.FmtSystemOne` |

The critical property for agentloop: **same wire on both sides**. No
translation in either direction, no format negotiation, no response
reshape. Agentloop's `POST /v1/systemone` request is byte-identical to its
`POST /v1/chat/completions` request — only the URL and model selector change.

For streaming clients onegow assembles a single SSE chunk via the
`ForcedStream` path (the upstream is non-streaming only). See `kinds_wire.go`
`ForcedStream()` — note `KindSystemOne` is **not** in that switch, meaning Jev
answers non-streaming directly; a streaming client gets the assembled chunk.

---

## 2. Why onegw

The PRD (`docs/PRD.md` §4.3) already settles this:

| Concern | Owner | Rule |
|---|---|---|
| Model routing / token saving / fallback | **onegw** | agentloop sends the tier per step; onegw picks the leg, saves tokens, records usage |
| Enforcement (bounds, budgets, approval, kill) | **agentloop** | never delegated to the model, the sandbox, or the gateway |
| Execution + file mutation | **xdev rpc** | sandboxed, `--add-dir` restricted, timeout- and watchdog-bounded, audited by agentloop |
| Knowledge retrieval + memory | **LeanKG** | the only place that owns the code graph and long-term recall |

And §3.1's stack table:

> *"Models | talk to **onegw** (OpenAI-compatible `/v1/chat/completions` + `/v1/messages`)"*
> *"Tiers / routing | **onegw combos**, not agentloop code"*
> *"the model list comes from `GET /v1/models` — and **one combo is ours to add**"*

Jev is a provider. Onegw is the provider gateway. Placing it anywhere else
would be one of the three confirmed duplications the audit found
([`docs/DUPLICATION-AUDIT.md` §3](#3-confirmed-duplications-to-cut-three-all-in-approval-catalog-territory)):
"rebuilding an existing seam the code already has."

---

## 3. What still blocks it

### 3.1 The provider `Kind` is merged — the routing is not

**Status (verified 2026-09-21 against onegw master `d53f2a0`):**

| Check | Result |
|-------|--------|
| `internal/provider/systemone.go` on master | ✅ `git cat-file -t master:internal/provider/systemone.go` → `blob` |
| `POST /v1/systemone` route on master | ✅ `internal/server/server.go:502` — registered with idempotency |
| `FmtSystemOne` format constant on master | ✅ `internal/translat/openai.go:32` |
| `KindSystemOne` in provider dispatch | ✅ `internal/provider/provider.go:2245` |
| `systemone` row in `onegw.toml.example` | ✅ line 177–181 |
| Jev in onegw `docs/ARCHITECTURE.md` | ✅ |
| `go test ./internal/provider/...` passing | ✅ (the kind routes through the shared dispatch) |

The commits that landed: `9faea01` (feat: add systemone Kind), `ddd67b8` (gofmt),
`19a139e` (docs), `3e84e1c` (docs), `612a9fe` (PRD stamp), `305aa13` (PRD status),
`081540c` (translat repair that touched the same files).

**What is NOT on master** — the verdict-driven combo reorder:

```
$ git merge-base --is-ancestor 9390e2b master
NOT merged
$ git show master:internal/router/router.go | grep -c "verdict-driven\|systemone"
0
```

Commit `9390e2b` (`feat(router): verdict-driven combo reorder via TypeSafe systemone (T3)`)
exists only on `origin/feat/systemone-provider` (remote branch, `1dd5a59`) and
`refs/remotes/pr/110`. It is **PR #110**. Without it, onegw has a systemone
*provider* but no way for agentloop's tier selection to actually route to it
per step — the router's combo table has no systemone leg.

**The worktree at `onegw/.worktrees/gh-pr-systemone`** (head `9390e2b`) is the
source of truth for the routing change. The earlier note that the whole branch
was unmerged was stale — the `Kind` shipped in `9faea01`, the routing is the
separate open PR.

### 3.2 TypeSafe API spec not fetched

### 3.4 Config template — done

`onegw.toml.example` already has the `systemone` block (lines 177–181). The
block in §4 below is preserved as the reference copy; it is spec-verified only
against the code's assumptions, not against the TypeSafe API doc (see §3.2).

### 3.5 Docs — done

Jev is listed in onegw `docs/ARCHITECTURE.md` and `README.md`. The
`grep -rni "systemone\|jev\|typesafe" onegw/docs/ onegw/README.md` that
originally returned nothing now returns hits.

---

## 4. The onegw config (already in `onegw.toml.example`)

The block below is what shipped — mirror of the `anthropic` row:

```toml
# TypeSafe Jev model — POST /v1/systemone, same OpenAI wire on both sides.
# Agentloop selects it via tier combo; onegw routes per step.
[[providers]]
name = "typesafe"
kind = "systemone"
base_url = "https://api.typesafe.ai"          # VERIFY from spec
# api_key = "" or env ONEGW_PROVIDER_TYPESAFE_KEY

[[providers.accounts]]
name = "default"
api_key = ""                                  # fill or use env
```

Then add a combo that maps a tier to Jev, so agentloop's existing tier logic
(`planning` → strong, `execution` → mid, `tiny` → small) picks it up with no
agentloop code change:

```toml
# Agentloop tiers (see agentloop PRD §3.1):
#   planning   → onegw combo incl. systemone (reasoning-heavy)
#   execution  → onegw combo (tool-calling turns)
#   tiny       → onegw combo (cheap, fast)
```

The agentloop PRD already says this is the contract: *"agentloop sends the
tier (`planning`/`execution`/`tiny`, the middle one added by us) per step;
onegw picks the leg"* (§4.3). Jev is one leg onegw may pick, per tier.

**Status:** the `[[providers]]` block is live in `onegw.toml.example`. The
combo mapping is **not** — that is what PR #110 adds.

Jev is not only a model: its Noul (yes/no probability) and Score (harm severity) primitives are exactly the LLM guardrail recipe in the TypeSafe cookbook (https://docs.typesafe.ai/cookbooks/llm_guardrails). agentloop already sends user goals and model replies to the loop — those are the two surfaces the cookbook screens. Live experiments on 2026-09-19 (`typesafe_experiments.sh`, `internal/experiments/experiments.go`) ran the cookbook's full battery — 4 Noul hazard questions + 1 severity Score, one call per message, via `POST /v1/systemone` with `model = "jev-latest"` — against 10 user goals and 5 model replies under both strict and permissive policies:

| What | Result |
|---|---|
| 5 harmful inputs (3 jailbreaks, 2 dangerous requests) | All `block` under strict; 1 routes to `review` under permissive (proof the thresholds are a product decision, not a default) |
| 5 benign inputs | All `pass` under both policies |
| 5 model replies (2 benign, 1 refusal, 2 harmful) | 2 `pass`, 3 `block` (dosage advice at sev 2.05, jailbreak-compliance at sev 1.44, harmful lockpick at sev 2.12) |
| Per-call cost | ~740 ms, ~535 tokens in + 90 out (jev-1.13.0) |
| Policy decision | strict is the default; permissive is operator-selectable; both published in PRD §17 as calibrated priors |


---

## 7. Verification checklist (what "done" looks like)

| # | Check | Status |
|---|-------|--------|
| 1 | `feat/systemone-provider` merged into onegw master | ✅ Done (`9faea01`, verified 2026-09-21) |
| 2 | `go build ./...` + `go test ./internal/provider/...` pass on master | ✅ Done |
| 3 | `systemone` row in `onegw.toml.example` | ✅ Done (lines 177–181) |
| 4 | Jev listed in onegw `docs/ARCHITECTURE.md` + `README.md` | ✅ Done |
| 5 | `onegw/systemone-<model>` selector in xdev `models.yml` (if xdev consumes it) | ⬜ Not done |
| 6 | `GET /v1/models` on a running onegw returns the systemone model | ⬜ Not verified |
| 7 | Verdict-driven combo reorder merged (PR #110) | ❌ **Open** — `9390e2b` on `origin/feat/systemone-provider` only |
| 8 | TypeSafe API spec fetched (`https://docs.typesafe.ai/introduction`) | ❌ **Open** — auth scheme, model catalog, error codes unverified |
| 9 | agentloop M1 containment case 3 (model from new provider tier) added + passing | ⬜ Not done |
| 10 | agentloop → xdev execution (`AgentBase.execute`) wired | ❌ **Open** — 5 v1 tools are stubs |

### What "Jev is usable by agentloop" means

Items 1–4 are done. Items 7–8 are the real blockers. Without 7, agentloop's
`tierForStep` returns a tier that onegw cannot route to a systemone leg — the
provider exists but the router's combo table has no systemone entry. Without 8,
the `base_url` and auth assumptions in `onegw.toml.example` are guesses.

Items 5–6 and 9–10 are downstream of 7–8 and are tracked in `todo.md`.
