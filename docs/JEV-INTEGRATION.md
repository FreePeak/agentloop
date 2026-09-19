# Jev provider — integration guide for agentloop

**Status:** blocked on onegw merge · **Date:** 2026-09-18
**Repo:** `github.com/FreePeak/agentloop`
**Onegw branch:** `feat/systemone-provider` (head `b2cfd88`, **not merged into master**)
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

### 3.1 onegw branch is unmerged

`feat/systemone-provider` exists only in the worktree at
`onegw/.worktrees/feat/systemone-provider` (head `b2cfd88`). Verified:

```
git show master:internal/provider/systemone.go   → does not exist
git show master:internal/server/server.go       → no systemone
git show master:internal/translat/openai.go     → no FmtSystemOne
```

The code is self-contained (one `Kind`, one executor function, one route, one
format constant) and has a test file (`systemone.go` sits next to
`cursor.go`, `searxng.go`, `opencode_test.go`), but it is not on any branch
that ships. **Step 1 is merge it into onegw master.**

### 3.2 No config template

`onegw.toml.example` has **no `systemone` provider block**. Every other kind
(`openai`, `anthropic`, `gemini`, `opencode`, `cline`, etc.) has a `[[providers]]`
row with `kind`, `base_url`, `api_key` (via env), and `[[providers.accounts]]`.
Jev has none — so a user has no documented way to enable it.

### 3.3 No docs

Zero mentions across onegw:

```
grep -rni "systemone\|jev\|typesafe" onegw/docs/ onegw/README.md
→ (no output)
```

### 3.4 TypeSafe API spec not fetched

Memory recall says *"read full docs at https://docs.typesafe.ai/introduction
then config the jev provider"*. The code assumes OpenAI wire (verbatim
forward), which is consistent with how `KindOpenAI`, `KindOpenCode`,
`KindCline` work — but the spec is authoritative for auth scheme, model
catalog shape, error codes, and rate-limit headers. **Fetch it before
writing the config block.**

---

## 4. The concrete onegw change (once merged + spec fetched)

Mirror the `anthropic` block in `onegw.toml.example`:

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

---

## 5. xdev side — consumer only

xdev already consumes onegw as a provider (`xdev/internal/ai/provider.go` —
`Name()` returns the key from `models.yml`, `HealthCheck` probes `/v1/models`
which onegw serves unauthenticated). To make xdev use Jev:

1. Add `onegw/systemone-<model>` to `xdev`'s `models.yml` under the onegw
   provider — same shape as existing `onegw/openai-...` entries.
2. xdev's `OpenAICompletionsProvider` (`internal/ai/openai_completions.go`)
   handles it with **no adapter change** — same wire, same SSE framing, same
   tool schema. The `KindSystemOne.Format()` → `translat.FmtSystemOne` path
   in onegw ensures the response is OpenAI-shaped on the way out.
3. xdev never builds its own Jev HTTP client. That would be the audit's
   "rebuild an existing seam" failure mode (§3 of
   [`docs/DUPLICATION-AUDIT.md` §3](#3-confirmed-duplications-to-cut-three-all-in-approval-catalog-territory)).

If a reviewer asks "does xdev need a `KindSystemOne` path?" — the answer is
no. xdev is a client of onegw, not a client of TypeSafe. Onegw is the
transport layer; xdev sends the same `POST /v1/chat/completions` to onegw
whether the upstream is OpenAI, Gemini, or Jev, and onegw translates to the
upstream. That is the inheritance manifest `design.md` §3.1 and the PRD
§4.3 already state.

---

## 6. agentloop side — nothing to do (today)

Agentloop's relevant lines, verbatim:

- §4.3: *"Model routing / token saving / fallback | **onegw** | agentloop sends
  the tier (`planning`/`execution`/`tiny`, the middle one added by us) per
  step; onegw picks the leg, saves tokens, records usage"*
- §3.1: *"Models | talk to **onegw** … | Tiers / routing | **onegw combos**,
  not agentloop code … the model list comes from `GET /v1/models`"*

Agentloop has one task when Jev ships: make sure its tier combo that maps to
Jev exists in onegw and is tested (§11.2 containment case 3 — a model from a
new provider tier — should cover it). No agentloop code change required.

---

## 7. Verification checklist (what "done" looks like)
- [ ] `feat/systemone-provider` merged into onegw master
- [ ] `go build ./...` and `go test ./internal/provider/...` in onegw master pass
  (the `systemone` `Kind` routes through the shared `provider.go` dispatch,
  so it is exercised by existing provider tests once the kind is on master)
- [ ] `systemone` row added to `onegw.toml.example` (block above, spec-verified)
- [ ] Jev listed in onegw `docs/ARCHITECTURE.md` provider table and `README.md`
  provider list
- [ ] `onegw/systemone-<model>` selector added to xdev `models.yml` (if xdev is
  to use it)
- [ ] `GET /v1/models` on a running onegw returns the systemone model
  (confirms `KindSystemOne` catalog path, line 2156 in `provider.go`)
- [ ] agentloop M1 containment case 3 (model from new provider tier) added and
  passing in `docs/check-prd.py`

---

## 8. Guardrail screening — the other half of Jev's job (agentloop scope)

Jev is not only a model: its Noul (yes/no probability) and Score (harm severity) primitives are exactly the LLM guardrail recipe in the TypeSafe cookbook (https://docs.typesafe.ai/cookbooks/llm_guardrails). agentloop already sends user goals and model replies to the loop — those are the two surfaces the cookbook screens. Live experiments on 2026-09-19 (`typesafe_experiments.sh`, `internal/experiments/experiments.go`) ran the cookbook's full battery — 4 Noul hazard questions + 1 severity Score, one call per message, via `POST /v1/systemone` with `model = "jev-latest"` — against 10 user goals and 5 model replies under both strict and permissive policies:

| What | Result |
|---|---|
| 5 harmful inputs (3 jailbreaks, 2 dangerous requests) | All `block` under strict; 1 routes to `review` under permissive (proof the thresholds are a product decision, not a default) |
| 5 benign inputs | All `pass` under both policies |
| 5 model replies (2 benign, 1 refusal, 2 harmful) | 2 `pass`, 3 `block` (dosage advice at sev 2.05, jailbreak-compliance at sev 1.44, harmful lockpick at sev 2.12) |
| Per-call cost | ~740 ms, ~535 tokens in + 90 out (jev-1.13.0) |
| Policy decision | strict is the default; permissive is operator-selectable; both published in PRD §17 as calibrated priors |

Integration point: unchanged — agentloop still sends a `tier` per step and onegw still picks the leg. Guardrail screening rides the same `systemone` wire as a per-step call; the `POST /v1/systemone` above is the exact shape agentloop sends. Agentloop owns the threshold policy (PRD §4.3 separation of powers); TypeSafe owns the probabilities.
