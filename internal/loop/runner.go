// Package loop holds the bounded loop runner (P1 Bounded Loop).
// LoopRunner enforces every ceiling as a pre-action check:
// step, wall-clock, cost, confidence floor, progress stall,
// consecutive failures. No ceiling is checked after the fact.
package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/experiments"
	"github.com/FreePeak/agentloop/internal/memory"
	"github.com/FreePeak/agentloop/internal/planner"
	"github.com/FreePeak/agentloop/internal/replay"
	"github.com/FreePeak/agentloop/internal/store"
	"github.com/FreePeak/agentloop/internal/tools"
	"github.com/FreePeak/agentloop/internal/tracer"
)

// ScreenResult is one Noul/Score screening result from
// the TypeSafe battery (M2.x).
type ScreenResult struct {
	Hazard string  `json:"hazard"`
	Prob   float64 `json:"prob"`
	Action string  `json:"action"` // pass | review | block
}

// StepRecord is one tool call within a run.
type StepRecord struct {
	StepID   int    `json:"step_id"`
	Phase    string `json:"phase"` // think | act | evaluate
	Tool     string `json:"tool"`
	ArgsHash string `json:"args_hash"`
	// Args is what the step was called with. Recorded because a trace that
	// shows only an args *hash* cannot answer "what did it actually try?"
	// — the first question anyone asks of a surprising step, and the thing
	// a replay needs to reproduce it.
	Args map[string]any `json:"args,omitempty"`
	// Why is the reasoner's one-sentence rationale when a model chose this
	// step ("" when the deterministic rotation did). The trajectory is
	// readable only if the reasons are in it.
	Why        string         `json:"why,omitempty"`
	Result     interface{}    `json:"result,omitempty"`
	CostUSD    float64        `json:"cost_usd"`
	Confidence float64        `json:"confidence"`
	LatencyMs  int64          `json:"latency_ms"`
	Screens    []ScreenResult `json:"screens,omitempty"` // M2.x: guardrail screen results
}

// RunResult is the outcome of a run. State says where it
// ended; ExitReason says why. They are independent:
// success=true with exitReason=max_steps is a valid outcome
// (goal met, budget hit at the same time).
type RunResult struct {
	RunID            string       `json:"run_id"`
	State            State        `json:"state"`
	ExitReason       ExitReason   `json:"exit_reason"`
	Success          *bool        `json:"success"` // nil until final evaluate
	PartialSynthesis string       `json:"partial_synthesis,omitempty"`
	SpendUSD         float64      `json:"spend_usd"`
	CurrentTier      string       `json:"current_tier"`
	Steps            []StepRecord `json:"steps"`
	// M8: what the model transport did for this run. Tokens observed
	// from onegw's usage report; the leg that answered (not the combo
	// we asked for); and, if synthesis failed, why — the deterministic
	// partial is still returned in that case.
	Usage TokenUsage `json:"usage,omitempty"`
	// ReasonErrors records each step where the model could not be asked
	// for a decision (transport failure, unreadable reply). The run keeps
	// going on the deterministic rotation, and the trajectory says why it
	// had to — a silent fallback is how a run looks "fine" while the
	// model is unreachable.
	ReasonErrors   []string `json:"reason_errors,omitempty"`
	AnsweredBy     string   `json:"answered_by,omitempty"`
	SynthesisError string   `json:"synthesis_error,omitempty"`
	// ScreenError is why the guardrail screen could not be evaluated
	// when it failed closed (exit_reason=guardrail_unavailable). An
	// outage and a policy block are different incidents and must not
	// be reported as the same one.
	ScreenError string `json:"screen_error,omitempty"`
}

// RunnerConfig holds the tunables for a single run.
type RunnerConfig struct {
	RunID               string
	MaxSteps            int
	WallClock           time.Duration
	CostBudget          float64
	Goal                string
	Context             string
	ConfidenceFloor     float64
	EscalationThreshold float64
	Gate                *ApprovalGate   // M5: fail-closed HITL gate (nil = bypass, tests)
	SandboxDir          string          // workspace a sandboxed turn runs in (empty = tool's own default)
	Model               ModelClient     // M8: outbound model transport (nil = deterministic synthesis only)
	Guardrail           GuardrailClient // M2.x: System One screen (nil = screening off)
	// Policy is the guardrail threshold set. The zero value is the
	// measured strict default (PRD §17), so a deploy that never sets it
	// screens strictly; Permissive is the operator-selectable
	// alternative (a jailbreak dressed as a medical accommodation
	// scores block under strict and review under permissive — measured
	// 2026-09-19, §7.2).
	Policy experiments.Policy
}

// LoopRunner runs one bounded agent loop. Ceilings are enforced
// in code, not by model behavior. The kill channel lets an
// operator stop the loop mid-step (P75 Kill Switch).
// M4 adds memory (four tiers) and SQLite checkpointing.
type LoopRunner struct {
	memory              *memory.Store // four-tier state (M4)
	checkpointStore     *store.Store  // SQLite WAL persistence (M4)
	startStep           int           // resume point (checkpoint step_idx)
	restoredSteps       []StepRecord  // steps carried over from checkpoint
	resumed             bool          // set true when restored from checkpoint
	cfg                 RunnerConfig
	budgetGuard         *budget.Guard
	toolRegistry        tools.ToolRegistry
	killCh              chan struct{}
	seenArgs            map[string]int // dedupKey -> count
	consecutiveFailures int
	spendSoFar          float64
	bestConfidence      float64
	currentConfidence   float64
	nextToolFn          func(int, RunnerConfig) (string, map[string]any)
	tracer              *tracer.Tracer                                  // optional: nested span tracing (M2)
	cycleAlert          func(string)                                    // optional: called on cycle detection (M2)
	planner             *planner.Planner                                // optional: drives tool selection (M3)
	plan                *planner.Plan                                   // current plan (M3)
	gate                *ApprovalGate                                   // M5: fail-closed HITL gate (nil = bypass, tests)
	model               ModelClient                                     // M8: outbound model transport (nil = deterministic synthesis)
	guardrail           GuardrailClient                                 // M2.x: live System One screen (nil = screening off)
	guardrailScreen     func(string, any) (map[string]float64, float64) // M2.x: injected screen (tests)
	pausedStep          int                                             // step held at paused_approval (M5)
	// heldChoice is the decision already made for the held step. Resume
	// replays it rather than re-asking the model: a second call can choose
	// a DIFFERENT tool, which would run something the operator never saw,
	// under an approval for something else. Cost is the smaller reason.
	heldChoice *StepChoice
	lastResult RunResult // partial result at pause (M5 resume)
}

// NewRunner returns a LoopRunner for the given config.
func NewRunner(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry) *LoopRunner {
	return newRunner(cfg, guard, reg, nextToolDefault, nil, nil, nil, nil, nil)
}

// NewRunnerWithToolFn returns a LoopRunner with a custom tool picker.
// Used by tests to drive deterministic tool selection.
func NewRunnerWithToolFn(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any)) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, nil, nil, nil, nil, nil)
}

// GuardrailClient is the System One evaluation surface the loop screens
// with: one state in, a Noul hazard map plus the severity Score out.
//
// Both halves come back from one call on purpose. The screen is one
// request either way, and a caller that had to fetch the severity
// separately would either double the cost or invent a default for it —
// and a defaulted severity silently turns every review into a pass.
//
// internal/systemone.Client satisfies it; tests use fakes. Same rule as
// ModelClient: the loop consumes the transport, it never becomes one
// (no retry ladder, no key pool, no backend switch).
type GuardrailClient interface {
	Screen(ctx context.Context, state any) (map[string]float64, float64, error)
}

// NewRunnerWithTracer returns a LoopRunner with a custom tool picker
// and tracer (M2: nested spans).
func NewRunnerWithTracer(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any), t *tracer.Tracer) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, t, nil, nil, nil, nil)
}

// NewRunnerWithCycleAlert returns a LoopRunner that calls alertFn
// when a cycle is detected (M2: cycle alert).
func NewRunnerWithCycleAlert(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any), alertFn func(string)) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, nil, alertFn, nil, nil, nil)
}

// NewRunnerWithPlanner returns a LoopRunner that uses the Planner
// to drive tool selection (M3). When planner is set, Run() calls
// planner.Plan() before the first step; nil falls back to nextToolDefault.
func NewRunnerWithPlanner(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	p *planner.Planner) *LoopRunner {
	return newRunner(cfg, guard, reg, nextToolDefault, nil, nil, p, nil, nil)
}

// NewRunnerWithPlannerAndGate combines the M3 Planner and the M5
// ApprovalGate in one runner (issue #12: gate-in-runner).
// Pass nil for either to disable that feature.
func NewRunnerWithPlannerAndGate(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	p *planner.Planner, gate *ApprovalGate) *LoopRunner {
	// nil picker on purpose: this is the production constructor, and the
	// production preference order is reasoner -> rotation, not rotation
	// first. Passing nextToolDefault here would make the model unreachable
	// (the service would rotate forever while a gateway sat idle).
	return newRunner(cfg, guard, reg, nil, nil, nil, p, nil, gate)
}

// NewRunnerWithCheckpointer returns a LoopRunner that persists
// every 5th iteration to the SQLite WAL checkpoint store (P8)
// and resumes from the last checkpoint on a fault (NFR-4).
// Pass nil for both to disable memory tracking (default).
func NewRunnerWithCheckpointer(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any),
	cps *store.Store) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, nil, nil, nil, cps, nil)
}

// NewRunnerWithApprovalGate returns a LoopRunner that
// enforces every tool call through the fail-closed HITL
// gate (M5: P30/P68). Pass nil for the gate to bypass
// HITL (default for all existing tests/constructors).
func NewRunnerWithApprovalGate(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any),
	gate *ApprovalGate) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, nil, nil, nil, nil, gate)
}

// NewRunnerWithTypeSafeScreen returns a LoopRunner with a
// guardrail screening function (M2.x: TypeSafe). The function
// produces Noul scores from a tool result; Route() decides
// block/review/pass at each step boundary.
func NewRunnerWithTypeSafeScreen(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	screen func(string, any) (map[string]float64, float64)) *LoopRunner {
	r := newRunner(cfg, guard, reg, nextToolDefault, nil, nil, nil, nil, nil)
	r.guardrailScreen = screen
	return r
}

func newRunner(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any),
	t *tracer.Tracer, alertFn func(string), planner *planner.Planner,
	cps *store.Store, gate *ApprovalGate) *LoopRunner {
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = MaxSteps
	}
	if cfg.WallClock == 0 {
		cfg.WallClock = time.Duration(WallClockS) * time.Second
	}
	if cfg.CostBudget == 0 {
		cfg.CostBudget = CostBudgetUSD
	}
	if cfg.ConfidenceFloor == 0 {
		cfg.ConfidenceFloor = ConfidenceFloorDefault
	}
	if cfg.EscalationThreshold == 0 {
		cfg.EscalationThreshold = EscalationThreshold
	}
	if cfg.Policy == (experiments.Policy{}) {
		cfg.Policy = experiments.Strict
	}
	if t == nil {
		t = tracer.New()
	}
	return &LoopRunner{
		cfg:             cfg,
		budgetGuard:     guard,
		toolRegistry:    reg,
		killCh:          make(chan struct{}),
		seenArgs:        make(map[string]int),
		nextToolFn:      toolPick,
		tracer:          t,
		cycleAlert:      alertFn,
		planner:         planner,
		checkpointStore: cps,
		gate:            gate,
		model:           cfg.Model,
		guardrail:       cfg.Guardrail,
		pausedStep:      -1,
		lastResult:      RunResult{},
	}
}

// Kill closes the kill channel — the loop checks it at every
// iteration boundary and exits state=killed within one step.
func (r *LoopRunner) Kill() {
	select {
	case <-r.killCh:
	default:
		close(r.killCh)
	}
}

func (r *LoopRunner) tierForStep(step int, cfg RunnerConfig) string {
	if r.plan != nil && step < len(r.plan.Steps) {
		return r.plan.Steps[step].Tier
	}
	return "planning"
}

func tierCombo(tier string) string {
	switch tier {
	case "planning":
		return "planning"
	case "execution":
		return "execution"
	default:
		return "tiny"
	}
}

// Run executes the bounded loop and returns the RunResult.
func (r *LoopRunner) Run(ctx context.Context) (RunResult, error) {
	return r.runWith(ctx, true)
}

// Resume re-enters a run that paused for operator approval
// (M5). The runner rewinds to the held step and re-checks
// the gate; if the operator approved, the step runs and the
// loop continues. Returns the final result. Safe to call
// only from a paused run; calling it on a finished run
// returns the last result unchanged.
func (r *LoopRunner) Resume(ctx context.Context) (RunResult, error) {
	if r.pausedStep < 0 {
		return r.lastResult, nil
	}
	return r.runWith(ctx, false)
}

// runWith builds the run state and delegates the loop to
// runLoop. fresh=true (Run) initializes; fresh=false
// (Resume) rewinds to the paused step from r.lastResult.
func (r *LoopRunner) runWith(ctx context.Context, fresh bool) (RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.WallClock)
	defer cancel()

	var result RunResult
	if fresh {
		result = RunResult{
			RunID:            r.cfg.RunID,
			State:            StateThinking,
			Steps:            []StepRecord{},
			SpendUSD:         0,
			CurrentTier:      r.tierForStep(0, r.cfg),
			PartialSynthesis: "",
		}
		// --- M3: build the plan once before the first step ---
		if r.planner != nil {
			r.plan = r.planner.Plan(planner.PlannerConfig{
				Goal:    r.cfg.Goal,
				Context: r.cfg.Context,
				Tier:    "", // routing tier: filled by onegw combo
				Frame:   "", // default: LOOP
			})
		}
		// --- M4: resume from checkpoint if present ---
		if r.checkpointStore != nil {
			r.prepareResume()
			result.Steps = append(result.Steps, r.restoredSteps...)
		}
	} else {
		// Resume: pick up exactly where the approval
		// hold stopped. The held step is re-checked
		// against the gate; an approved request runs.
		result = r.lastResult
		result.State = StateActing
	}

	// --- M2: start root span for the run ---
	r.tracer.StartSpan(r.cfg.RunID, r.cfg.RunID, "", tracer.SpanSystem, "run", 0)

	return r.runLoop(ctx, result)
}

// runLoop is the bounded ReAct loop body, shared by Run and
// Resume. It starts at r.startStep (fresh) or r.pausedStep
// (resume) and exits on the first ceiling, approval hold, or
// the step budget. Every exit ends the root span.
func (r *LoopRunner) runLoop(ctx context.Context, result RunResult) (RunResult, error) {
	startStep := r.startStep
	if r.pausedStep >= 0 {
		startStep = r.pausedStep
	}
	for step := startStep; step < r.cfg.MaxSteps; step++ {
		// --- P75 kill switch: check at every iteration boundary ---
		select {
		case <-r.killCh:
			result.State = StateKilled
			_ = r.synthesize(ctx, &result)
			r.tracer.StartSpan(r.cfg.RunID, r.cfg.RunID+"-kill", r.cfg.RunID, tracer.SpanSystem, "kill", step+1)
			r.tracer.EndSpan(r.cfg.RunID+"-kill", nil, nil, 0)
			r.endRunSpan(r.cfg.RunID, result, nil)
			return result, nil
		default:
		}

		// --- wall-clock ceiling (pre-action) ---
		if ctx.Err() != nil {
			result.State = StateExhausted
			result.ExitReason = ExitWallClock
			result.Success = ptr(false)
			_ = r.synthesize(ctx, &result)
			r.endRunSpan(r.cfg.RunID, result, nil)
			return result, nil
		}

		// --- M3: daily ceiling check (ExitDailyBudget) ---
		if err := r.budgetGuard.CheckDaily(); err != nil {
			result.State = StateExhausted
			result.ExitReason = ExitDailyBudget
			result.Success = ptr(false)
			_ = r.synthesize(ctx, &result)
			r.endRunSpan(r.cfg.RunID, result, err)
			return result, nil
		}

		// --- M3: confidence floor check (ExitConfidenceFloor) ---
		r.bestConfidence = math.Max(r.bestConfidence, r.currentConfidence)
		if r.currentConfidence > 0 && r.bestConfidence < r.cfg.ConfidenceFloor {
			result.State = StateExhausted
			result.ExitReason = ExitConfidenceFloor
			result.Success = ptr(false)
			_ = r.synthesize(ctx, &result)
			r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("confidence floor"))
			return result, nil
		}

		// --- cost budget pre-action check (P3 Cost Circuit Breaker) ---
		if err := r.budgetGuard.Check(); err != nil {
			result.State = StateExhausted
			result.ExitReason = ExitCostBudget
			result.Success = ptr(false)
			_ = r.synthesize(ctx, &result)
			r.tracer.StartSpan(r.cfg.RunID, r.cfg.RunID+"-budget", r.cfg.RunID, tracer.SpanSystem, "budget", step+1)
			r.tracer.EndSpan(r.cfg.RunID+"-budget", nil, err, 0)
			r.endRunSpan(r.cfg.RunID, result, err)
			return result, nil
		}
		// --- one tool per ReAct turn ---
		result.State = StateActing
		// M3: use planner step tier for routing
		if r.plan != nil && step < len(r.plan.Steps) {
			result.CurrentTier = tierCombo(r.plan.Steps[step].Tier)
		}

		// --- M9: choose the action ---
		// Order of preference: the reasoner (a model that can see the last
		// result), an injected picker (tests), then the deterministic
		// rotation. Every fallback is recorded in the step's Why, because a
		// trajectory that cannot say why a tool was chosen cannot be
		// debugged.
		var toolName string
		var args map[string]any
		var why string
		var held *StepChoice // the decision for this step, if a model made one
		switch {
		case r.heldChoice != nil && step == r.pausedStep:
			// Resuming an approved hold: replay the decision that was
			// approved. Re-asking would let the model pick a different
			// tool, so the operator would have approved one action and a
			// different one would run.
			toolName, args = r.heldChoice.Tool, r.heldChoice.Args
			why = "approved (replayed): " + r.heldChoice.Why
			r.heldChoice = nil
		case r.nextToolFn != nil:
			toolName, args = r.nextToolFn(step, r.cfg)
			why = "picker: injected"
		case r.model != nil:
			name, a, reason, err := r.decide(ctx, step, &result)
			if err != nil {
				// A failed decision must not be silent and must not be
				// fatal: the run falls back to the rotation and the step
				// records what went wrong. (Same rule as synthesis.)
				result.ReasonErrors = append(result.ReasonErrors, fmt.Sprintf("step %d: %v", step, err))
				toolName, args = nextToolDefault(step, r.cfg)
				why = "reasoner unavailable: " + err.Error()
			} else if name == "" {
				// The model said the goal is established. That is the goal
				// predicate firing, not a bound: the run is done, and it
				// says so with StateSuccess rather than spinning to
				// max_steps.
				result.Success = ptr(true)
				result.State = StateSuccess
				result.ExitReason = ExitGoalMet
				result.Steps = append(result.Steps, StepRecord{
					StepID: step,
					Phase:  "evaluate",
					Tool:   "(none)",
					Why:    reason,
				})
				_ = r.synthesize(ctx, &result)
				r.endRunSpan(r.cfg.RunID, result, nil)
				return result, nil
			} else {
				toolName, args = name, a
				why = reason
				held = &StepChoice{Tool: name, Args: a, Why: reason}
			}
		default:
			toolName, args = nextToolDefault(step, r.cfg)
			why = "rotation: no model client"
		}
		argsHash := dedupKey(r.cfg.RunID, toolName, args)
		// --- M5: fail-closed HITL gate (P30/P68) ---
		// Categorize the tool; auto -> approve, confirm -> auto-if-confident,
		// approve/unknown -> hold for the queue. Gate may be nil in tests.
		if r.gate != nil {
			conf := r.currentConfidence
			if step == 0 {
				conf = 1.0 // first step has no evidence yet - trust it
			}
			req := ApprovalRequest{
				RunID:      r.cfg.RunID,
				StepID:     step,
				Tool:       toolName,
				ArgsHash:   argsHash,
				Category:   Categorize(toolName),
				Confidence: conf,
				Requested:  time.Now(),
			}
			dec := r.gate.Check(req)
			if !dec.ShouldRun() {
				// Hold the step. Resume() rewinds here and
				// re-checks the gate; an approved request
				// continues the loop.
				result.State = StatePausedApproval
				_ = r.synthesize(ctx, &result)
				r.pausedStep = step
				r.heldChoice = held
				r.lastResult = result
				r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("approval required: %s", dec.Reason))
				return result, nil
			}
		}

		// Track (tool, args) pairs for cycle detection (P5).
		// 3 identical pairs → progress_stall.
		r.seenArgs[argsHash]++
		if r.seenArgs[argsHash] >= CycleThreshold {
			result.State = StateExhausted
			result.ExitReason = ExitProgressStall
			result.Success = ptr(false)
			_ = r.synthesize(ctx, &result)
			if r.cycleAlert != nil {
				r.cycleAlert(fmt.Sprintf("cycle: tool %q repeated %d times at step %d", toolName, r.seenArgs[argsHash], step))
			}
			r.tracer.StartSpan(r.cfg.RunID, r.cfg.RunID+"-cycle", r.cfg.RunID, tracer.SpanSystem, "cycle", step+1)
			r.tracer.EndSpan(r.cfg.RunID+"-cycle", nil, fmt.Errorf("cycle detected"), 0)
			r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("cycle detected"))
			return result, nil
		}

		// --- P26 idempotency: skip if already executed this exact call ---
		// Count 2 = second occurrence (first was actual execution at count 1).
		if r.seenArgs[argsHash] == 2 {
			result.Steps = append(result.Steps, StepRecord{
				StepID:   step,
				Phase:    "act",
				Tool:     toolName,
				ArgsHash: argsHash,
				Result:   "already executed — skipped",
			})
			r.tracer.StartSpan(r.cfg.RunID, fmt.Sprintf("span-%d", step), r.cfg.RunID, tracer.SpanAct, "idempotent-skip", step+1)
			r.tracer.EndSpan(fmt.Sprintf("span-%d", step), "skipped", nil, 0)
			continue
		}

		// --- M2: start child span for this tool call ---
		spanID := fmt.Sprintf("span-%d", step)
		r.tracer.StartSpan(r.cfg.RunID, spanID, r.cfg.RunID, tracer.SpanAct, toolName, step+1)

		start := time.Now()
		tr, err := r.toolRegistry.Execute(ctx, toolName, args)
		latency := time.Since(start).Milliseconds()

		r.spendSoFar += 0.001 // stub cost per call (real: CostTracker.estimate)
		r.budgetGuard.RecordSpend(0.001)

		// --- M2: validate result (2K token cap) ---
		tr = validateResult(tr)

		// --- M2.x: System One guardrail screen ---
		//
		// The screen runs on the tool result before it can reach the
		// model, and it fails closed: a battery that could not be
		// evaluated is not a pass. Recording the verdict on the step is
		// what makes a blocked run auditable — "why did it stop" has to
		// be answerable from the trajectory (PRD §7.2, §4.3).
		var screens []ScreenResult
		if r.screening() {
			nouls, sev, serr := r.screen(ctx, toolName, tr.Data)
			if serr != nil {
				result.State = StateExhausted
				result.ExitReason = ExitGuardrailUnavailable
				result.Success = ptr(false)
				result.ScreenError = serr.Error()
				_ = r.synthesize(ctx, &result)
				r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("guardrail unavailable: %w", serr))
				return result, nil
			}
			hazard, prob := topHazard(nouls)
			action := experiments.Route(nouls, sev, r.cfg.Policy)
			screens = screenRows(hazard, prob, sev, action)

			if action == "review" || action == "block" {
				// The screened content never reaches the model, so this
				// step is the only record of what the screen saw: without
				// it a blocked run is an exit reason with no evidence
				// attached (PRD §7.2 — the trajectory must answer "what
				// was it blocked on?").
				result.Steps = append(result.Steps, StepRecord{
					StepID:    step,
					Phase:     "act",
					Tool:      toolName,
					ArgsHash:  argsHash,
					Args:      args,
					Why:       why,
					Result:    tr.Data,
					CostUSD:   0.001,
					LatencyMs: latency,
					Screens:   screens,
				})
				if action == "review" {
					result.State = StatePausedApproval
					_ = r.synthesize(ctx, &result)
					r.pausedStep = step
					r.heldChoice = held
					r.lastResult = result
					r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("guardrail: review required"))
					return result, nil
				}
				result.State = StateExhausted
				result.ExitReason = ExitGuardrailBlock
				result.Success = ptr(false)
				_ = r.synthesize(ctx, &result)
				r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("guardrail: blocked"))
				return result, nil
			}
		}

		if err != nil || !tr.Success {
			// A tool may report failure as an observation (Success=false,
			// nil error) — the LeanKG client does exactly that on an
			// outage. Dereferencing err here unconditionally panicked on
			// that path, so the reason is rendered defensively.
			result.Steps = append(result.Steps, StepRecord{
				StepID:    step,
				Phase:     "act",
				Tool:      toolName,
				ArgsHash:  argsHash,
				Args:      args,
				Why:       why,
				Result:    map[string]any{"error": stepError(err, tr), "message": tr.Message},
				CostUSD:   0.001,
				LatencyMs: latency,
				Screens:   screens,
			})
			r.tracer.EndSpan(spanID, tr, err, 0.001)
			r.rememberStep(step, toolName, tr.Message, true)
			r.maybeCheckpoint(result.Steps)
			continue
		}
		// --- M3: score + track consecutive failures ---
		stepConf := 1.0
		if err != nil || !tr.Success {
			stepConf = 0.0
		}
		r.currentConfidence = stepConf
		result.Steps = append(result.Steps, StepRecord{
			StepID:     step,
			Phase:      "act",
			Tool:       toolName,
			ArgsHash:   argsHash,
			Args:       args,
			Why:        why,
			Result:     tr.Data,
			CostUSD:    0.001,
			LatencyMs:  latency,
			Confidence: stepConf,
			Screens:    screens,
		})
		r.tracer.EndSpan(spanID, tr, nil, 0.001)
		r.rememberStep(step, toolName, tr.Data, false)
		r.maybeCheckpoint(result.Steps)

		if err != nil || !tr.Success {
			r.consecutiveFailures++
		} else {
			r.consecutiveFailures = 0
		}
		if r.consecutiveFailures >= ConsecutiveFailures {
			result.State = StateExhausted
			result.ExitReason = ExitConsecutiveFailures
			result.Success = ptr(false)
			_ = r.synthesize(ctx, &result)
			r.endRunSpan(r.cfg.RunID, result, fmt.Errorf("consecutive failures"))
			return result, nil
		}
	}

	// Loop exhausted without resolve = max_steps exit.
	// The run is no longer paused: a second Resume() is a no-op.
	r.pausedStep = -1
	r.heldChoice = nil
	result.State = StateExhausted
	result.ExitReason = ExitMaxSteps
	result.Success = ptr(false)
	_ = r.synthesize(ctx, &result)
	r.lastResult = result
	r.endRunSpan(r.cfg.RunID, result, nil)
	return result, nil
}

// endRunSpan ends the root span and records the run-level outcome.
func (r *LoopRunner) endRunSpan(runID string, result RunResult, runErr error) {
	output := map[string]any{
		"state":       result.State,
		"exit_reason": result.ExitReason,
		"success":     result.Success,
		"step_count":  len(result.Steps),
		"spend_usd":   result.SpendUSD,
	}
	var err error
	if runErr != nil {
		err = runErr
	}
	r.tracer.EndSpan(runID, output, err, result.SpendUSD)
}

// validateResult enforces the M2 2K token cap on tool results.
// Results exceeding the cap are truncated and flagged.
func validateResult(tr tools.ToolResult) tools.ToolResult {
	const maxTokens = 2000
	size := estimateTokens(tr.Data)
	if size > maxTokens {
		tr.Data = map[string]any{
			"truncated":     true,
			"original_size": size,
			"capped_to":     maxTokens,
			"preview":       partialString(tr.Data, maxTokens/2),
		}
		tr.Message = "result truncated to 2K tokens (M2 cap)"
	}
	return tr
}

// estimateTokens is a rough token estimate for a tool result.
func estimateTokens(v any) int {
	b, _ := json.Marshal(v)
	return len(b) / 4 // ~4 chars per token
}

// partialString returns a truncated string representation
// of v at maxChars.
func partialString(v any, maxChars int) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > maxChars {
		return s[:maxChars] + "..."
	}
	return s
}

// Replay runs the replay analysis on the tracer's trace for this run.
func (r *LoopRunner) Replay() replay.ReplayResult {
	return replay.Replay(r.tracer, r.cfg.RunID)
}

// synthesizePartial builds a labelled partial synthesis from
// the steps executed so far. Used by kill switch, budget
// ceiling, and cycle detector — never returns empty string.
func synthesizePartial(steps []StepRecord) string {
	if len(steps) == 0 {
		return "No steps executed."
	}
	out := fmt.Sprintf("Synthesis after %d steps: ", len(steps))
	for i, s := range steps {
		if i > 0 {
			out += " | "
		}
		out += fmt.Sprintf("step %d: %s", s.StepID, s.Tool)
	}
	return out
}

// nextToolDefault returns the tool and args for a step. M1 uses a
// deterministic rotation over the 4 v1 tools for testing;
// production replaces this with the planner's output.
// nextToolDefault is the deterministic stand-in for model-driven tool
// selection (PRD §4.3: "agentloop owns the loop, xdev owns the turn").
// It rotates the v1 surface and gives each tool the arguments it needs to
// be a real call rather than a shape:
//
//   - the readers get the goal as their query text;
//   - run_tests gets the workspace to verify;
//   - write_file gets a target path, because a write with no target now
//     fails closed — the previous rotation passed only step/goal, so every
//     write was rejected before the model inside the sandbox ever saw it.
//
// ponytail: the path is derived from cfg.RunID, not from what the run
// learned. A real planner names the file; this exists so the write path is
// exercisable end to end until that lands.
func nextToolDefault(step int, cfg RunnerConfig) (string, map[string]any) {
	tools := []string{"query", "web_search", "run_tests", "write_file"}
	name := tools[step%len(tools)]
	args := map[string]any{"step": step, "goal": cfg.Goal}
	switch name {
	case "run_tests":
		if cfg.SandboxDir != "" {
			args["cwd"] = cfg.SandboxDir
		}
	case "write_file":
		args["path"] = fmt.Sprintf("agentloop-%s-step-%d.txt", cfg.RunID, step)
		args["content"] = fmt.Sprintf("step %d of run %s: %s\n", step, cfg.RunID, cfg.Goal)
	}
	return name, args
}

// dedupKey is the agentloop idempotency fingerprint:
// sha256(run_id:tool:canonical_args).
func dedupKey(runID, tool string, args map[string]any) string {
	b, _ := json.Marshal(canonicalArgs(args))
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s", runID, tool, string(b))))
	return hex.EncodeToString(h[:])
}

func canonicalArgs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = canonicalArgs(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = canonicalArgs(val)
		}
		return out
	default:
		return v
	}
}

// stepError renders the reason a step failed. A tool can fail two ways:
// a Go error (transport, validation) or a typed ToolResult with
// Success=false and no error (a dependency answering with a failure).
// Both are ordinary observations; neither may panic.
func stepError(err error, tr tools.ToolResult) string {
	if err != nil {
		return err.Error()
	}
	if tr.Message != "" {
		return tr.Message
	}
	return "tool reported failure with no detail"
}
func ptr(b bool) *bool { return &b }
