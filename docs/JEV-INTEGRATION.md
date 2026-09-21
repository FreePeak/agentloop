# System One — Jev / Laya integration for agentloop

**Status:** onegw `KindSystemOne` merged · combo routing open (PR #110) · Laya local path proposed  
**Date:** 2026-09-21  
**Repo:** `github.com/FreePeak/agentloop`  
**Canonical product name in this doc:** **System One** (the decision API).  
**Backends:** TypeSafe **Jev** (hosted API) and open **Laya** (local / self-hosted).  
**Onegw:** `kind = "systemone"`, `POST /v1/systemone` — already on master for the Jev path.  
**Related issues:** [#8](https://github.com/FreePeak/agentloop/issues/8) guardrails · [#15](https://github.com/FreePeak/agentloop/issues/15) agent trust · [#21](https://github.com/FreePeak/agentloop/issues/21) onegw PR #110 routing.

> Older title was "Jev provider — integration guide". The wire and ownership rules did not change; this revision adds **Laya as a drop-in local backend**, the **shared System One contract**, cookbook-backed use cases for agentloop, and measured local RSS/latency on an M2 Pro.

---

## TL;DR

**System One decisions go through onegw. Not xdev. Not agentloop.**  
Agentloop owns *policy* (when to screen, thresholds, pass/review/block). Onegw owns *transport* (which backend answers `POST /v1/systemone`). Backends today:

| Backend | Where it runs | Auth | Cost | Latency (measured) | License |
|---|---|---|---|---|---|
| **Jev** (`jev-latest` / `jev-1.13.0`) | TypeSafe cloud | Bearer API key | ~$ per call (tokens) | **~740 ms**/screen (4 Noul + 1 Score, 2026-09-19) | proprietary API |
| **Laya** (`convaiinnovations/laya` + siblings) | local process / sidecar | none (weights on disk) | **$0** after download | warm **~60–95 ms** English on M2 Pro; cold first load multi-minute once | Apache 2.0 |

```
agentloop (policy: questions battery, thresholds, when to stop)
    │  always: POST /v1/systemone  { state, model, questions }
    ▼
onegw (transport: picks backend, records usage, fallback)
    │
    ├── kind=systemone → https://api.typesafe.ai/v1/systemone   (Jev)
    └── kind=systemone-local → laya sidecar / embedded worker   (Laya)
```

Agentloop **never** imports `laya` or calls `api.typesafe.ai` directly. Same request body either way. Same answer shape either way. Thresholds and `Route()` stay in agentloop (`internal/experiments`).

---

## 1. What System One is (shared contract)

System One is **not** a chat model. It evaluates a `state` against a map of typed `questions` and returns structured `answers` — no free text to parse, no tool-call JSON to repair.

### 1.1 Wire (TypeSafe API, already what onegw forwards)

```http
POST /v1/systemone
Authorization: Bearer <KEY>          # Jev only; local Laya ignores / omits
Content-Type: application/json

{
  "state": "<string|object|array>",
  "model": "jev-latest",             # or a laya alias when local
  "questions": {
    "<id>": { "type": "noul|choice|score", "instructions": "...", "criteria": {...} }
  }
}
```

Source: TypeSafe docs (`docs.typesafe.ai` — introduction/quickstart, api, primitives). Onegw path: `internal/provider/systemone.go` forwards this body verbatim for Jev.

### 1.2 Three primitives (identical semantics on Jev and Laya)

| Type | Returns | agentloop use |
|---|---|---|
| **`noul`** | P(yes) ∈ [0,1] | jailbreak / harm / grounded / stated? flags |
| **`choice`** | top label + distribution + confidence | tool pick, tier pick, department, intent |
| **`score`** | expected level on an ordinal rubric | harm severity, urgency, trust |

Laya's public SDK uses the same three types (`choice` / `score` / `noul`) over the same shape of `state` + `questions` ([HF model card](https://huggingface.co/convaiinnovations/laya), [GitHub](https://github.com/NandhaKishorM/laya)). That is why it can replace Jev **for decision calls**, not for open-ended generation.

### 1.3 What Laya is (relative to Jev)

Laya is an open, non-autoregressive System One–style engine (ModernBERT / mmBERT), trained with RLCD, Apache 2.0:

| Checkpoint | Encoder | Params | Best for |
|---|---|---|---|
| `laya` | ModernBERT-large ~421M | English | guardrails, English triage, tool routing |
| `laya-multilingual` | mmBERT-base ~322M | 100+ languages | non-English goals / replies |
| `laya-typed-decisions` | ModernBERT-large ~421M | typed-decisions workflows | invoice / security / CS / observability packs |

Built-in `Router(preload=…)` picks english vs multilingual from script/language before the forward pass. Fine-tune notebooks exist; **base checkpoints are near chance on typed-decisions zero-shot** — treat Laya as a fast base to specialise, same honesty bar we already apply to priors in PRD §17.

**Honest limits (from Laya BENCHMARKS + our Mac run):**

- High-cardinality `choice` (>~20 options at default head budget) is weak vs Jev (Banking77-style). Keep agentloop tool sets ≤15 visible tools (already PRD law).
- Published T4 latency ~33 ms; **our M2 Pro warm p50 ~73 ms** (English, lazy load). First cold download was ~5+ min once.
- Peak load maxrss ~**2.6–2.9 GB**; steady process RSS after load ~**0.2–0.9 GB**; venv ~902 MB; HF laya cache ~132 MB+.
- Calibration: base checkpoints over-confident until temperature fit; gate on our measured thresholds, not raw confidence alone.

---

## 2. Ownership (unchanged, restated for two backends)

| Concern | Owner | Rule |
|---|---|---|
| When to screen, thresholds, pass/review/block/support | **agentloop** | `Route()` + PRD §17 policies |
| Which backend answers `/v1/systemone` | **onegw** | combo / provider kind |
| Execution / file mutation | **xdev rpc** | never the decision model |
| Code graph / long memory | **LeanKG** | never the decision model |

Putting Laya *inside* agentloop as a Python import would violate the same separation-of-powers table as calling TypeSafe from agentloop (PRD §4.3, duplication audit).

---

## 3. Abstraction design — one contract, two backends

### 3.1 Request/response contract (agentloop → onegw)

Agentloop always sends:

```json
{
  "state": { "surface": "user_goal|model_reply|tool_result|subagent_output", "text": "..." },
  "model": "systemone",
  "questions": { "...fixed or template battery..." }
}
```

Onegw maps `model` / combo leg to:

1. **Provider Jev** — existing `KindSystemOne` → `https://api.typesafe.ai/v1/systemone` with `model: jev-latest` (or pinned `jev-1.13.0`).
2. **Provider Laya-local** — new thin leg (proposed name `kind = "systemone-local"` or a second account under systemone) that:
   - POSTs the **same** JSON to a local sidecar `http://127.0.0.1:<port>/v1/systemone`, **or**
   - shells a long-lived worker that already has `Router(preload=…)` warm.

The sidecar is a ~50-line FastAPI/Flask (or the HF space shape) wrapping:

```python
from laya import Router
router = Router(preload=True)  # or preload(["english"]) on 16 GB Macs

@app.post("/v1/systemone")
def systemone(body: dict):
    # ignore Authorization; map body["questions"] → laya questions
    out = router.predict(body["state"], body["questions"])
    return {"model": out.get("routing", {}).get("model", "laya"), "answers": out["answers"], "usage": {...}}
```

**Answer normalisation (required):** Laya returns `answers[id].choice|score|noul|confidence`. Jev returns the TypeSafe answer objects. Onegw (or the sidecar) must expose the **Jev-shaped** fields agentloop already parses in `typesafe_experiments.sh` / future screen code:

- noul → `.answers.<id>.noul` (float)
- score → `.answers.<id>.score` (float)
- choice → `.answers.<id>.choice` + `.confidence`

No agentloop branch on backend.

### 3.2 Config sketch (onegw)

```toml
# Hosted TypeSafe Jev (ships today in onegw.toml.example)
[[providers]]
name = "typesafe"
kind = "systemone"
base_url = "https://api.typesafe.ai"
# api_key via ONEGW_PROVIDER_TYPESAFE_KEY

# Local Laya sidecar (proposed)
[[providers]]
name = "laya-local"
kind = "systemone"          # same wire path /v1/systemone
base_url = "http://127.0.0.1:8091"
# api_key unused

# Combos — pick per environment
# [[combos]] name = "systemone-guard" strategy = "order"
# members = ["typesafe", "laya-local"]   # cloud primary, local fallback
# or reverse for air-gapped / cost-zero default
```

Fallback policy (product, not code yet):

| Environment | Primary | Fallback | Why |
|---|---|---|---|
| CI / no key | **laya-local** | none | reproducible, $0 |
| Dev laptop 16 GB | **laya-local** english-only preload | typesafe if sidecar down | speed + offline |
| Prod multi-tenant | **typesafe** | laya-local | ops simplicity + SLA |
| Air-gapped | **laya-local** only | — | no egress |

### 3.3 What stays in agentloop (backend-agnostic)

Already implemented:

- `internal/experiments.Route(nouls, severity, policy)` — cookbook precedence `support > block > review > pass`
- `Strict` / `Permissive` policies (PRD §17)
- Shell harnesses: `typesafe_experiments.sh`, `typesafe_benchmark.sh`, `analyze_experiments.sh`

Still to wire (issue #8 checklist): call site at step boundary, BudgetGuard line item, eval case 6 in CI.

### 3.4 Status of the Jev path (onegw)

| Check | Status |
|---|---|
| `KindSystemOne` + `POST /v1/systemone` on onegw master | ✅ |
| `systemone` row in `onegw.toml.example` | ✅ |
| Docs in onegw ARCHITECTURE | ✅ |
| Verdict-driven combo reorder (PR #110 / commit `9390e2b`) | ❌ open — [#21](https://github.com/FreePeak/agentloop/issues/21) |
| TypeSafe API shape verified from docs | ✅ this revision (Bearer, `/v1/systemone`, state+model+questions) |
| Laya local provider / sidecar | ❌ proposed — this doc |

---

## 4. Cookbook deep-dive → agentloop use cases

Sources read for this section:

- [Function calling](https://docs.typesafe.ai/cookbooks/function_calling)
- [LLM guardrails](https://docs.typesafe.ai/cookbooks/llm_guardrails)
- [Intent routing](https://docs.typesafe.ai/patterns/intent-routing) (pattern)
- Primitives + API reference (`/primitives`, `/api`)
- Live agentloop experiments 2026-09-19 (issue #8)
- Laya README/BENCHMARKS + local M2 Pro measurements 2026-09-21

### UC-1 — Input/output guardrail screen (ship path: issue #8)

**Cookbook:** guardrails — one request, battery of Nouls + one severity Score; `route()` → pass | review | block | support. Screen **user message in** and **model reply out**.

**agentloop mapping:**

| Surface | When | Battery |
|---|---|---|
| User goal | before first model call / each phase | jailbreak, harmful_request, medical_advice, self_harm + severity |
| Model reply | before operator / next tool | broke_policy, harmful_request, medical_advice, self_harm + severity |
| Tool result (optional v1.1) | before it enters context | domain Nouls (see UC-3) |

**Policy (already measured):** strict default (review≥0.35, action≥0.70, sev≥2.0 block); permissive operator-selectable. `review` → approval queue (not silent drop). `block` → labelled partial + halt.

**Backend pick:** Laya for CI and dev (ms, $0); Jev for prod until a fine-tuned Laya guard checkpoint beats Jev on *our* red-team set.

**Cost prior:** Jev ~740 ms / ~665 tok per screen. Laya warm ~70–100 ms, $0, ~2–3 GB peak RAM.

### UC-2 — Tool / function dispatch (closed-set tools)

**Cookbook:** function calling — map NL → function name + `Literal` args via Choice + optional `stated?` Noul per arg; confidence = **weakest** argument (not product). Free ints/dates keep defaults.

**agentloop mapping:**

- v1 tools are few (`query`, `web_search`, `run_tests`, `write_file`, …) — ideal Choice cardinality.
- Replace or gate brittle "model emits tool JSON" with:
  1. `__tool__` Choice over tool descriptions ("DO NOT USE WHEN" text already required by PRD).
  2. Per-tool argument Choices for enums; Noul `stated?` so optional args stay defaulted.
  3. If `confidence < tool_floor` (prior 0.75) → ask model to clarify or escalate HITL, **do not** execute.
- Runs **before** `AgentBase.execute` — still one tool call per ReAct turn (FR-4).

**Backend pick:** Laya english checkpoint is enough for ≤15 tools; keep head budget healthy (do not dump 77-way Banking-style enums).

### UC-3 — Sub-agent / trajectory trust (issue #15)

**Problem:** scout/reviewer/security-reviewer output enters main context with no reliability gate.

**Battery (proposed, issue #15):**

| Noul | Meaning |
|---|---|
| `grounded` | claims backed by repo evidence |
| `complete` | addresses the assigned task |
| `speculative` | fabricated / unverified claims |
| `introduces_vuln` (domain) | would introduce a security bug (generic harm Noul missed SQLi at 0.12) |

Rule sketch: `speculative>0.5 → reject`; `grounded∧complete>0.5 → accept`; else `review`.

**Backend pick:** Laya for every sub-agent message (volume × Jev latency hurts). Calibrate on real trajectories before prod.

### UC-4 — Tier / combo pre-route (feeds onegw PR #110)

**Pattern:** intent routing — classify request, send to cheap path / specialist / human.

**agentloop mapping:**

- Before spending a planning-tier call, System One Choice: `{simple_react, plan_execute, escalate_human, refuse}`.
- Optional Score: `complexity` 0–2.
- Verdict reorders onegw combo members (the missing piece in PR #110) — **System One decides the verdict; onegw still owns the combo table**.

**Backend pick:** must be **fast and local-friendly** → Laya default; Jev acceptable if already warm for guard screen in same phase (batch questions in **one** call).

### UC-5 — Approval / HITL fatigue reduction

**Score** `irreversibility` + **Noul** `needs_human` on the pending write preview. Auto-approve only when policy table says so **and** System One agrees above floor — cuts interrupt rate toward PRD <10% target without deleting the fail-closed table.

### UC-6 — Eval scoring assist (M6)

**Choice/Score** rubrics over run transcripts for CatHappy/Edge/Adversarial labels as a *second* scorer beside hermetic `ScoreFn`. Never the sole deploy gate until calibrated (Ch.10 rule).

### UC-7 — Multilingual goals

Operator or ticket text not in English → Laya `Router` multilingual checkpoint (script detect <1 ms). Jev path stays English-centric unless TypeSafe documents otherwise. Same questions battery.

### UC-8 — Offline / CI dogfood

`typesafe_experiments.sh` today hits `api.typesafe.ai` and needs `TYPESAFE_API_KEY`. Point `SYSTEMONE_BASE_URL=http://127.0.0.1:8091` (or onegw local) at Laya sidecar so CI runs UC-1 corpus with no secret and no flaky network — same JSON assertions.

---

## 5. Recommended build sequence (agentloop + onegw)

| Step | Work | Repo | Depends |
|---|---|---|---|
| S0 | Keep `Route()` + policies as SoT; document Laya (this file) | agentloop | — |
| S1 | Land onegw PR #110 combo reorder | onegw | [#21](https://github.com/FreePeak/agentloop/issues/21) |
| S2 | Laya sidecar implementing `/v1/systemone` + answer shape parity tests | new tiny repo or `agentloop/scripts/laya_sidecar` | local weights |
| S3 | onegw provider account/base_url for local | onegw | S2 |
| S4 | Wire screen at phase boundary; BudgetGuard; eval case 6 | agentloop | S1 or S3 |
| S5 | Tool dispatch Choice gate (UC-2) behind flag | agentloop | S4 |
| S6 | Sub-agent trust battery (UC-3) | agentloop / xdev hook | S4, [#15](https://github.com/FreePeak/agentloop/issues/15) |
| S7 | Optional fine-tune Laya on agentloop guard + trust labels | research | production traces |

**Non-goals:** training inside agentloop; replacing onegw chat completions with Laya; calling Laya from xdev without onegw.

---

## 6. Guardrail experiments (Jev baseline — keep)

Live 2026-09-19 (`typesafe_experiments.sh`, `jev-1.13.0`):

| What | Result |
|---|---|
| 5 harmful inputs | All `block` under strict; 1 → `review` under permissive |
| 5 benign inputs | All `pass` |
| 5 model replies | 2 `pass`, 3 `block` |
| Per-call | ~740 ms, ~535 in + 90 out tokens |

**Next measurement (required before flipping default backend):** re-run the same corpus against Laya english checkpoint; publish a side-by-side table in this section. Do not switch prod primary until agreement on block/review labels is ≥ target (propose ≥0.9 on the fixed 15-message set, then expand).

---

## 7. Local Laya setup (operator)

```bash
# already validated on this machine 2026-09-21
python3.12 -m venv ~/venvs/laya && source ~/venvs/laya/bin/activate
pip install laya
python -c "from laya import Router; r=Router(); print(r.predict({'body':'refund or cancel'}, {'c':{'type':'noul','instructions':'cancel threat?'}}))"
```

| Machine class | Mode | RAM guidance |
|---|---|---|
| 8 GB | avoid | — |
| **16 GB (M2 Pro measured)** | `Router()` or `preload(["english"])` | peak ~2.8 GB load, steady ~0.2–0.9 GB |
| 32 GB+ | `Router(preload=True)` all three | multi-GB resident OK |

MPS available on Apple Silicon torch wheels; warm English ~70 ms was fine on default device.

---

## 8. Verification checklist

| # | Check | Status |
|---|---|---|
| 1 | onegw `KindSystemOne` on master | ✅ |
| 2 | onegw `systemone` example config | ✅ |
| 3 | TypeSafe API: Bearer + POST `/v1/systemone` + primitives | ✅ verified from docs 2026-09-21 |
| 4 | Combo routing PR #110 | ❌ open |
| 5 | Shared contract doc (this file) covers Jev + Laya | ✅ |
| 6 | Laya installed + smoke predict on maintainer Mac | ✅ 2026-09-21 |
| 7 | Laya sidecar `/v1/systemone` shape-compatible | ❌ not built |
| 8 | agentloop phase-boundary screen live | ❌ issue #8 |
| 9 | Side-by-side Jev vs Laya on guard corpus | ❌ |
| 10 | xdev models.yml systemone selector (if needed) | ⬜ optional |

### Done means

- Agentloop code paths speak **only** `POST /v1/systemone` with batteries from config.
- Onegw can answer that call via **Jev and/or Laya** without agentloop changes.
- UC-1 guardrails ship with measured thresholds; UC-2/3 behind flags until calibrated.

---

## 9. Sources

- TypeSafe: introduction, quickstart, API, primitives, cookbooks `function_calling`, `llm_guardrails`, pattern `intent-routing` (local mirror under session `/tmp/ts-docs/`, canonical https://docs.typesafe.ai/llms.txt).
- Laya: https://github.com/NandhaKishorM/laya · https://huggingface.co/convaiinnovations/laya · BENCHMARKS.md.
- agentloop: `docs/PRD.md` §3.1, §4.3, §7.2, §14, §17 · `internal/experiments` · issue #8 / #15 / #21.
- onegw: `KindSystemOne`, `internal/provider/systemone.go`, PR #110.
