# agentloop Operator Console — UI Design Document

**Status:** draft · **Version:** 0.1.0 · **Date:** 2026-09-19
**For:** M6 (Evals & console) — see `docs/PRD.md` §13 milestone table, §6.1 endpoint table, §12.5/§12.6
**Source:** PRD §12.5/§12.6 (page specs) + UI/UX Pro Max skill (design system, chart types, UX guidelines)

This document is a design document, not a product spec. Every page below is a wireframe-level description: what is on the page, where it goes, and why. Implementation details (component libraries, CSS framework version, exact breakpoint values) are M6's problem; this document's job is to make sure the console is an operator surface built on P91 Progressive Disclosure, P94 Explanation Mode, P92 Status Updates, and P100 Graceful Handoff — the same guarantees the API provides.

---

## 1. Design system (from UI/UX Pro Max `--design-system`)

| Dial | Setting | Why |
|------|---------|-----|
| `--density` | **8/10 — dense dashboard** | Operator surface: one screen must carry run list + trajectory + budget + approval queue. Standard spacing (16–64px) wastes vertical real estate at a cost operators pay in scroll. |
| `--variance` | 6/10 — balanced | Dark-neutral glassmorphism reads as "trustworthy admin tool", not "playful product". |
| `--motion` | 4/10 — standard | Data-dense UI: stagger on load is enough. No pinned/split-text choreography — operators need scan speed, not theater. |

**Pattern:** Real-Time / Operations Landing (dark, data-dense, scannable).
**Style:** Glassmorphism (frosted, layered, depth via backdrop blur) — supported light + dark; the skill flags accessibility as *conditional*: requires 4.5:1 contrast, visible focus, reduced-motion.

### Palette

| Role | Hex | CSS Var | Usage in console |
|------|-----|---------|------------------|
| Background | `#0F172A` | `--color-background` | Page chrome, sidebar |
| Foreground | `#F8FAFC` | `--color-foreground` | Body text, primary labels |
| Card | `#1B2336` | `--color-card` | Panel surfaces |
| Card Foreground | `#F8FAFC` | `--color-card-foreground` | Text on cards |
| Primary | `#1E293B` | `--color-primary` | Header bars, nav |
| On Primary | `#FFFFFF` | `--color-on-primary` | Nav labels, headings on dark |
| Secondary | `#334155` | `--color-secondary` | Dividers, nested panels |
| Muted | `#272F42` | `--color-muted` | Disabled, context rows |
| Muted Foreground | `#94A3B8` | `--color-muted-foreground` | Timestamps, secondary labels |
| Accent/CTA | `#22C55E` | `--color-accent` | Approval confirm, kill-shield "healthy" |
| On Accent | `#0F172A` | `--color-on-accent` | Button text on accent |
| Destructive | `#EF4444` | `--color-destructive` | Kill button, rejection, error state |
| Ring | `#FFFFFF` | `--color-ring` | Focus rings (keyboard nav) |
| Border | `#475569` | `--color-border` | Panel borders |

Status colors throughout: green (healthy/running), amber (queued/pending/attention), red (failed/killed/kill active). No status meaning is conveyed by color alone — every status label is a word (see chart guidance below).

**Key effects:** backdrop blur 10–20px on panels over the page background; 1px solid `rgba(255,255,255,0.2)` borders; Z-depth via card stacking. Anti-pattern: slow updates, no automation — every live region must have an explicit update cadence and a pause/hide control.

### Typography

| Role | Font | Weight | Notes |
|------|------|--------|-------|
| Headings | Fira Code | 500–700 | Monospace headings signal "this is an operators' tool". |
| Body | Fira Sans | 300–700 | Sans for readability at dense sizes. |

Google Fonts import:
```css
@import url('https://fonts.googleapis.com/css2?family=Fira+Code:wght@400;500;600;700&family=Fira+Sans:wght@300;400;500;600;700&display=swap');
```

**Stack:** HTML + Tailwind CSS (the PRD specifies HTMX over server-rendered templates, no CDN; Tailwind provides the utility layer client-side, HTMX provides partial-page swaps server-side). No client-side framework — confirmed by PRD §1.2 ("matches onegw's admin console discipline; the console's job is P91 Progressive Disclosure… both of which are just markup, so a client-side framework would buy nothing").

### Pre-delivery checklist

- [ ] No emojis as icons — SVG: Heroicons/Lucide
- [ ] `cursor-pointer` on all clickable elements
- [ ] Hover states with smooth transitions (150–300ms)
- [ ] Focus states visible for keyboard nav (`:focus:ring-2 :focus:ring-white` per skill guidance; PRD operator surface requires keyboard access to approval queue and kill)
- [ ] Light mode: text contrast ≥ 4.5:1 minimum (skill flags glassmorphism as conditional)
- [ ] `prefers-reduced-motion` respected (disable skeleton pulse, stagger, live-ticker animation)
- [ ] Responsive: 1024px primary target, 768px secondary (operator consoles are rarely used on phones; 375px/1440px tested for compatibility)

---

## 2. Pages (per PRD §12.5)

Every page is HTMX: the server renders partials, HTMX swaps them. Live regions are true `<meta>`-driven SSE or polling endpoints — never a full page reload.

### 2.1 Run list — `GET /admin/console/runs`

**Position:** Top-left by default; primary landing view.
**Layout:** Two-column split — run list (left, ~40%) + selected run summary (right, ~60%). On ≤1024px the split stacks (list above).
**Content per run row:**
- Run ID (mono) · status pill (running/queued/complete/failed/escalated/killed) · goal (1 line) · start timestamp · elapsed (live pulse on running) · cost so far (mono, `$x.xx`)
- Row hover → inline sparkline of spend vs budget (bullet-chart micro visualization, per chart guidance below)
**Interactions:**
- Click row → right panel shows selected run's summary + trajectory preview (link to 2.2)
- Click column header → sort (status first by default — operators triage by state)
- Header search → live filter by run ID or goal substring (debounced, skeleton rows shown while loading — loading-state guideline)
**Accessibility:** status is a `<span role="status" aria-atomic="true">` per live-badge guidance; counts never announce bare numbers.

### 2.2 Trajectory viewer — linked from run list row / run detail

**Position:** Modal or right-drawer (Progressive Disclosure P91 — the trajectory is the evidence; most operators want the summary first).
**Layout:** Vertical timeline, one node per step, connected top-to-bottom. Each node shows: step number (mono), tool call (name + one-line args summary), status (✓ success / ⟳ running / ✗ failed / ⊘ skipped), latency (mono), cost (mono), nested span count.
**Expandable:** each node expands inline (HTMX partial swap) to show: full tool input, tool output (truncated to 2,000 tokens per PRD §6 with "expand" affordance), error if any, span trace IDs.
**Badges:** a divergent trace (word-set similarity < 0.9 per PRD §6) gets a red amber flag icon + tooltip "divergence flagged — compare with replay".
**Scroll behavior:** sticky scroll-snap on node focus (keyboard: Tab through steps, Enter to expand). Progress indicator per UX guideline: "Step 3 of 7" — no bare step list.
**Accessibility:** every node has `aria-label="Step N: <tool_name> <status>"`. Expand buttons have text labels, not icon-only.

### 2.3 Live step feed — embedded in run detail / flyable out

**Position:** Right rail on run detail, expandable to full-panel.
**Layout:** Event-stream style, newest at top, timestamp left-aligned, event type badge (tool_call / tool_result / decision / escalation / error), one line per event.
**Behavior:** SSE-driven; each new event slides in with a 200ms fade (respects `prefers-reduced-motion`). "Pause live feed" button (glassmorphism-styled) freezes the feed and shows "paused at <time>"; resume clears back to live. — per the real-time pattern's own guidance: "provide pause/hide or update-frequency controls for tickers."
**Accessibility:** the feed container has `aria-live="polite"` with `aria-atomic="false"`; each event is a row in a `<table>` (semantic, keyboard-sortable) so screen readers get structure rather than a stream of unparseable announcements.

### 2.4 Budget & spend rollups — dashboard overview / sidebar widget

**Position:** Top-right of the operator overview, sticky while scrolling.
**Content:**
- Budget vs actual (bullet chart — see chart section): target band, current spend bar, target marker line. Color bands per chart guidance: `#FFCDD2` (over-budget/red), `#FFF9C4` (warning/amber), `#C8E6C9` (within budget/green). Spend bar `#1976D2`. Target: black 3px marker.
- Per-task cost summary: avg, p50, p95, max (line chart over last N runs, trend line).
- Per-step cost breakdown: stacked bar or bullet — tools that consume most budget surface at the top.
- Monthly run-rate extrapolation: "At current pace: $X/mo (vs budget $Y/mo)".
**Interactions:** date-range picker (HTMX partial swap — no JS date library). Click a band → highlights contributing tasks in the run list.
**Accessibility:** every chart has a visible data table below or a "view data" toggle (skill a11y fallbacks: "visible data table plus summary"). Color is supplementary — labels carry the number.

### 2.5 Approval queue — dedicated page + sidebar badge

**Position:** Standalone page at `/admin/console/approvals`, also a sidebar badge (count + pulsing dot when pending > 0).
**Layout:** Table, sorted by age (oldest first — fatigue guard is per PRD: median approve < 3s = rubber-stamping; surface time to make aging visible).
**Columns:** Approve button (green) | Reject (red) | Modify (secondary) | Run ID | Step | Action preview (one line, e.g. "write_file: `/src/main.go`") | Confidence (gauge/bullet, see chart section) | Sensitive-topic flag (badge) | Age (mm:ss, mono) | Status pill (pending | approved | modified | rejected | timed_out) | Decided at (or "—") | Decided by (or "—")
**Row interactions:**
- Approve/Reject/Modify → inline confirmation (no full-page navigation). Confirm button requires a deliberate click — not a single accidental link.
- After action, row replaces with result status and a one-line outcome summary (form-feedback guideline: "loading → success message").
- Rows older than 30 minutes show the timeout policy as a tooltip: "auto-denied at 30 min per FR-7".
**Accessibility:** every action button is a labeled `<button>` (not icon-only). Approve/Reject are `aria-pressed` toggles during the in-flight state. Focus rings visible on all three action buttons (modal-focus-ring guideline).
**Design note on FR-7 integration:** the PRD says timeouts deny — the console surface must make this *visible*: a pending row that hits 30 min animates to a `timed_out` pill with a hover tooltip carrying the partial synthesis ("denied, but here is what the loop learned before it stopped"). This is P100 Graceful Handoff made visible.

### 2.6 Eval report — `/admin/console/evals`

**Position:** Standalone page; linked from the operator overview and from run detail when a run has eval results.
**Layout:** Two sections — run selector (top, dropdown of last N runs or run ID input) + report below.
**Content:**
- Header stats: pass/fail counts (stacked horizontal bar), avg score, avg latency, avg cost, number of cases.
- Category breakdown (4 categories per PRD §6/FR-11): happy / edge / adversarial / regression — each as a card showing count, pass rate, and a mini-trend sparkline over the last 10 runs.
- Per-case results table: case ID | category | score (0–1) | latency | cost | pass/fail | message (failure reason). Table rows are interactive — click to expand for the case prompt + the scored response (collapsed by default, P91).
- Overall gate verdict banner: green "PASS — deployable" or red "FAIL — blocked" with the threshold text ("score ≥ 0.8 ∧ latency ≤ cap ∧ cost ≤ cap") so the operator sees *why*, not just *whether*. — Explanation Mode P94.
- JSON report download button (raw `runs/<id>/eval.json` per PRD API table).
**Accessibility:** table is keyboard-sortable; each category card has the number as text, not just the sparkline (no color-only meaning). Wide-table handling per guideline: `overflow-x-auto` wrapper on ≤768px.

### 2.7 Kill button — in every run row action column + run detail header

**Position:** P75 — visible on every run context. On run detail page: top-right of the run panel, red, with confirmation dialog. In run list: small icon-button in the actions column with hover label "Kill run <id>".
**Behavior:** single click → inline confirmation: "Kill run `<id>`? This will halt within one step boundary and return the partial synthesis." [Confirm] [Cancel]. Confirm sends `POST /v1/runs/{id}/kill` (PRD §6 API). Success: row transitions to `killed` pill with timestamp, and the partial synthesis appears as a collapsed disclosure below.
**Design note:** the button is red (`--color-destructive`) with white text, but the label is text — "Kill" is a word, not just a colored shape (skill anti-pattern: color alone to convey meaning; PRD §12.5 explicitly says "if you cannot see the trajectory, you cannot tell convergence from an expensive wrong answer" — the button must be unmissable in shape and text).
**Accessibility:** aria-label "Kill run <id>", focus ring visible, confirmation dialog traps focus until resolved.

---

## 3. Chart type mapping (from UI/UX Pro Max `--domain chart`)

| Console surface | Data type | Chart type | Why this one | A11y fallback |
|---|---|---|---|---|
| Run list spend sparkline | Performance vs Target (compact) | Bullet chart | Space-constrained KPI next to the run row. 3–10 bullets fit in a sidebar width. | Visible KPI/target text + status label per row |
| Budget vs spend | Performance vs Target | Bullet chart (large) | Target band + actual spend + target marker in one view; directly shows over/under. | Data table toggle per eval report pattern |
| Monthly spend trend | Trend over time | Line chart | Continuous spend over time, multiple series (actual vs budget vs forecast) — distinct line styles, not just colors. | Visible data table + trend summary text |
| Live step feed rate | Real-time streaming | Streaming area chart | Ops data updating ≥1 Hz; operator needs current value at a glance. Pause/zoom built in. | Data table + current-value summary, pause button |
| Cost per category (eval) | Performance vs Target | Gauge chart | Single KPI per category measured against a threshold (0.8 pass). One gauge per category, in the eval header cards. | Number + threshold text beside each gauge |
| Cost per step per run | Performance vs Target | Bullet chart grid | 3–10 steps per run, each a bullet showing cost vs cap. Fits the compact-grid layout. | Per-bullet text labels |
| Anomaly flags (trajectory) | Anomaly Detection | Line Chart with Highlights | Trace spans where latency spikes or errors cluster; marked as separate data layer. | Anomaly event list + narrative summary |

Color guidance per chart (from skill): don't distinguish series by hue alone — use line style (solid/dashed/dotted), markers, and direct labels. All chart colors are on-brand (the palette above is dark-neutral; no rainbow palettes).

---

## 4. Cross-cutting UX guidelines (from UI/UX Pro Max `--domain ux`)

### 4.1 Loading states (high severity)
- Every partial that takes >200ms shows a skeleton or spinner during load — skeleton preferred (matches skill guidance: "use skeleton screens or spinners", never "blank screen while loading").
- Run list search and eval report run-selector are the two places operators wait most; skeleton the row list, not a spinner overlay (keeps context).

### 4.2 Form feedback (high severity)
- Every approval action, kill, modify, and run-delete shows: loading → success/error inline (skill: "Confirm form submission status").
- Error messages near the action that failed, not just a top-of-page toast — per PRD §6, errors are typed and carry the failure mode; the console mirrors that: the inline message names the failure mode (timeout, policy-denied, conflict) in plain words.

### 4.3 Progress indicators (medium severity)
- Multi-step processes (trajectory viewer, eval run progress) show "Step N of M" (skill: "Step 2 of 4 indicator" — no bare step list).

### 4.4 Live badge updates (high severity, accessibility)
- Any count or status that changes async (run status pill, approval count, budget remaining) announces a meaningful contextual status, not a bare number (skill: `aria-atomic="true"` with "3 items in cart" style, not `aria-live="polite">3</>`. Implementation in HTMX: SSE events carry the new state; the DOM node has `role="status"`.

### 4.5 Focus states (high severity)
- Every interactive control has a visible focus ring (`:focus:ring-2 focus:ring-white`), including controls inside modals (kill confirm, approve confirm) — skill: "focus ring on every interactive control, including modal controls".
- Keyboard: Tab through run rows, Enter to expand; Tab through approval actions, Enter/Space to confirm. Escape closes any modal/drawer.

### 4.6 Table handling (medium severity)
- All tables (run list, eval report, approval queue, trajectory) are in `overflow-x-auto` wrappers — skill: "use horizontal scroll or card layout", not "wide tables breaking layout". On ≤768px the console suggests card layout for the run list at minimum.

### 4.7 No emojis as icons (skill pre-delivery)
- All icons are SVG: Heroicons or Lucide (per skill). Kill, approve, reject, modify, expand — all icon + text label. No icon-only buttons anywhere on an operator surface.

### 4.8 Hover states + transitions (skill)
- All hover states 150–300ms ease. Row hover in run list, hover tooltips on charts (budget bands, bullet chart targets), hover on approval action buttons.

---

## 5. HTMX-specific design notes

The PRD fixes the stack as HTMX over server-rendered templates, no CDN. The design document respects that: every interaction below is achievable with HTMX attributes on the server side.

| Interaction | HTMX pattern |
|---|---|
| Run list search/filter | `hx-get` on input (debounced server-side), `hx-target` = run list container, `hx-indicator` = skeleton spinner |
| Run row → trajectory | `hx-get` on row click, `hx-target` = trajectory drawer container, `hx-swap` = innerHTML |
| Trajectory expand per step | `hx-get` on step expand button, `hx-target` = step body, `hx-swap` = outerHTML |
| Live step feed | SSE endpoint (`/events/runs/{id}`) consumed by `hx-ext="sse"` with `sse- id` for resume (Last-Event-ID pattern from PRD §6 wire shapes) |
| Budget date range | `hx-get` on range select, `hx-target` = budget widgets container |
| Approve/Reject/Modify | `hx-post` to `/v1/runs/{id}/approvals/{approval_id}`, `hx-target` = row, `hx-swap` = outerHTML, `hx-on::after-request` shows toast |
| Kill | `hx-post` to `/v1/runs/{id}/kill`, confirmation via a small HTMX-rendered dialog partial |
| Eval report run selector | `hx-get` on select change, `hx-target` = report container |
| Sort columns | `hx-get` with header link carrying sort key + direction |

No client-side JS framework; no React/Vue/Svelte. All live behavior is HTMX + SSE + a small amount of vanilla JS only where HTMX can't express it (e.g. pause/resume on the live feed button — though even that can be an HTMX `hx-post` toggling a server-side "paused" flag).

---

## 6. Open questions (for M6)

These are recorded as decisions needed, not design conflicts:

| # | Question | Status | Owner |
|---|---|---|---|
| Q1 | Dark mode required at v1 or light-first? | Design system recommends dark (operator surface), skill flags conditional a11y — needs a contrast pass in light mode. | M6 |
| Q2 | Tailwind via CDN not allowed (PRD says no CDN). Build-time Tailwind or hand-written CSS? | Unblocked — PRD §1.2 says "HTMX over server-rendered templates, no CDN". Tailwind build step is fine; inline a CSS build in the M6 docker stage. | M6 |
| Q3 | HTMX or Turbo? PRD says HTMX explicitly. | Decided — HTMX. | M6 |
| Q4 | Console served from the same binary as the API (`GET /admin/api/v1/*` per PRD §6) or a separate admin service? | Same binary per PRD §6. Routes under `/admin/`. | M6 |

---

## 7. Relationship to PRD milestones

- **M1–M5:** console does not exist; operators use the API via curl/HTTPClient. FR-12 is listed as a non-functional requirement only from M6 onward.
- **M6:** This document is the reference. Acceptance for M6 is per PRD §13: "deploys blocked on the full-suite gate; +10 cases/week; knee table published." The console surfaces the knee table and the full eval suite results.
- **The console is 1:1 with the API** (PRD §23): every page is a view of the endpoints in §6.1, every action is a POST to those endpoints. Nothing in the console does the API doesn't already allow — so if the console is missing, an operator can still do everything by POSTing.
