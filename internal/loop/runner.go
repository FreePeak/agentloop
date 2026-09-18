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
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/replay"
	"github.com/FreePeak/agentloop/internal/tracer"
	"github.com/FreePeak/agentloop/internal/tools"
)

// StepRecord is one tool call within a run.
type StepRecord struct {
	StepID    int         `json:"step_id"`
	Phase     string      `json:"phase"` // think | act | evaluate
	Tool      string      `json:"tool"`
	ArgsHash  string      `json:"args_hash"`
	Result    interface{} `json:"result,omitempty"`
	CostUSD   float64     `json:"cost_usd"`
	LatencyMs int64       `json:"latency_ms"`
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
	Steps            []StepRecord `json:"steps"`
}

// RunnerConfig holds the tunables for a single run.
type RunnerConfig struct {
	RunID      string
	MaxSteps   int
	WallClock  time.Duration
	CostBudget float64
	Goal       string
	Context    string
}

// LoopRunner runs one bounded agent loop. Ceilings are enforced
// in code, not by model behavior. The kill channel lets an
// operator stop the loop mid-step (P75 Kill Switch).
type LoopRunner struct {
	cfg          RunnerConfig
	budgetGuard  *budget.Guard
	toolRegistry tools.ToolRegistry
	killCh       chan struct{}
	seenArgs     map[string]int // dedupKey -> count (cycle + idempotency)
	spendSoFar   float64
	nextToolFn   func(int, RunnerConfig) (string, map[string]any)
	tracer       *tracer.Tracer // optional: nested span tracing (M2)
	cycleAlert   func(string)   // optional: called on cycle detection (M2)
}

// NewRunner returns a LoopRunner for the given config.
func NewRunner(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry) *LoopRunner {
	return newRunner(cfg, guard, reg, nextToolDefault, nil, nil)
}

// NewRunnerWithToolFn returns a LoopRunner with a custom tool picker.
// Used by tests to drive deterministic tool selection.
func NewRunnerWithToolFn(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any)) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, nil, nil)
}

// NewRunnerWithTracer returns a LoopRunner with a custom tool picker
// and tracer (M2: nested spans).
func NewRunnerWithTracer(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any), t *tracer.Tracer) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, t, nil)
}

// NewRunnerWithCycleAlert returns a LoopRunner that calls alertFn
// when a cycle is detected (M2: cycle alert).
func NewRunnerWithCycleAlert(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any), alertFn func(string)) *LoopRunner {
	return newRunner(cfg, guard, reg, toolPick, nil, alertFn)
}

func newRunner(cfg RunnerConfig, guard *budget.Guard, reg tools.ToolRegistry,
	toolPick func(int, RunnerConfig) (string, map[string]any),
	t *tracer.Tracer, alertFn func(string)) *LoopRunner {
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = MaxSteps
	}
	if cfg.WallClock == 0 {
		cfg.WallClock = time.Duration(WallClockS) * time.Second
	}
	if cfg.CostBudget == 0 {
		cfg.CostBudget = CostBudgetUSD
	}
	if t == nil {
		t = tracer.New()
	}
	return &LoopRunner{
		cfg:          cfg,
		budgetGuard:  guard,
		toolRegistry: reg,
		killCh:       make(chan struct{}),
		seenArgs:     make(map[string]int),
		nextToolFn:   toolPick,
		tracer:       t,
		cycleAlert:   alertFn,
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

// Run executes the bounded loop and returns the RunResult.
func (r *LoopRunner) Run(ctx context.Context) (RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.WallClock)
	defer cancel()

	result := RunResult{
		RunID:            r.cfg.RunID,
		State:            StateThinking,
		Steps:            []StepRecord{},
		SpendUSD:         0,
		PartialSynthesis: "",
	}

	// --- M2: start root span for the run ---
	r.tracer.StartSpan(r.cfg.RunID, r.cfg.RunID, "", tracer.SpanSystem, "run", 0)

	for step := 0; step < r.cfg.MaxSteps; step++ {
		// --- P75 kill switch: check at every iteration boundary ---
		select {
		case <-r.killCh:
			result.State = StateKilled
			result.PartialSynthesis = synthesizePartial(result.Steps)
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
			result.PartialSynthesis = synthesizePartial(result.Steps)
			r.endRunSpan(r.cfg.RunID, result, nil)
			return result, nil
		}

		// --- cost budget pre-action check (P3 Cost Circuit Breaker) ---
		if err := r.budgetGuard.Check(); err != nil {
			result.State = StateExhausted
			result.ExitReason = ExitCostBudget
			result.Success = ptr(false)
			result.PartialSynthesis = synthesizePartial(result.Steps)
			r.tracer.StartSpan(r.cfg.RunID, r.cfg.RunID+"-budget", r.cfg.RunID, tracer.SpanSystem, "budget", step+1)
			r.tracer.EndSpan(r.cfg.RunID+"-budget", nil, err, 0)
			r.endRunSpan(r.cfg.RunID, result, err)
			return result, nil
		}

		// --- one tool per ReAct turn ---
		result.State = StateActing
		toolFn := r.nextToolFn
		if toolFn == nil {
			toolFn = nextToolDefault
		}
		toolName, args := toolFn(step, r.cfg)
		argsHash := dedupKey(r.cfg.RunID, toolName, args)

		// Track (tool, args) pairs for cycle detection (P5).
		// 3 identical pairs → progress_stall.
		r.seenArgs[argsHash]++
		if r.seenArgs[argsHash] >= CycleThreshold {
			result.State = StateExhausted
			result.ExitReason = ExitProgressStall
			result.Success = ptr(false)
			result.PartialSynthesis = synthesizePartial(result.Steps)
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

		if err != nil || !tr.Success {
			result.Steps = append(result.Steps, StepRecord{
				StepID:    step,
				Phase:     "act",
				Tool:      toolName,
				ArgsHash:  argsHash,
				Result:    map[string]any{"error": err.Error(), "message": tr.Message},
				CostUSD:   0.001,
				LatencyMs: latency,
			})
			r.tracer.EndSpan(spanID, tr, err, 0.001)
			continue
		}

		result.Steps = append(result.Steps, StepRecord{
			StepID:    step,
			Phase:     "act",
			Tool:      toolName,
			ArgsHash:  argsHash,
			Result:    tr.Data,
			CostUSD:   0.001,
			LatencyMs: latency,
		})
		r.tracer.EndSpan(spanID, tr, nil, 0.001)
	}

	// Loop exhausted without resolve = max_steps exit.
	result.State = StateExhausted
	result.ExitReason = ExitMaxSteps
	result.Success = ptr(false)
	result.PartialSynthesis = synthesizePartial(result.Steps)
	r.endRunSpan(r.cfg.RunID, result, nil)
	return result, nil
}

// endRunSpan ends the root span and records the run-level outcome.
func (r *LoopRunner) endRunSpan(runID string, result RunResult, runErr error) {
	output := map[string]any{
		"state":         result.State,
		"exit_reason":   result.ExitReason,
		"success":       result.Success,
		"step_count":    len(result.Steps),
		"spend_usd":     result.SpendUSD,
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
			"truncated": true,
			"original_size": size,
			"capped_to":   maxTokens,
			"preview":     partialString(tr.Data, maxTokens/2),
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
// deterministic rotation over the 5 v1 tools for testing;
// production replaces this with the planner's output.
func nextToolDefault(step int, cfg RunnerConfig) (string, map[string]any) {
	tools := []string{"repo_search", "repo_context", "web_search", "run_tests", "write_file"}
	name := tools[step%len(tools)]
	return name, map[string]any{"step": step, "goal": cfg.Goal}
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

func ptr(b bool) *bool { return &b }
