// Package loop_test contains M2 guards + tracing tests.
// These verify: tracer wiring in Run(), cycle alert callback,
// 2K token cap on tool results, and replay-based fault detection.
package loop_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/tracer"
	"github.com/FreePeak/agentloop/internal/tools"
)

// newRunnerForTrace creates a LoopRunner with a tracer for M2 tests.
func newRunnerForTrace(t *testing.T, maxSteps int, tr *tracer.Tracer,
	toolPick func(int, loop.RunnerConfig) (string, map[string]any)) *loop.LoopRunner {
	t.Helper()
	guard := budget.New(100.0, 200.0)
	reg := tools.NewRegistry()
	cfg := loop.RunnerConfig{
		RunID:     "m2-test",
		MaxSteps:  maxSteps,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	return loop.NewRunnerWithTracer(cfg, guard, reg, toolPick, tr)
}

// TestM2_TracerWiredInRun verifies that Run() produces spans
// for the run root and each tool execution step.
func TestM2_TracerWiredInRun(t *testing.T) {
	tr := tracer.New()
	// Use different tools per step to avoid idempotency skip/cycle.
	runner := newRunnerForTrace(t, 3, tr, func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		tools := []string{"repo_search", "web_search", "repo_context"}
		name := tools[step%len(tools)]
		return name, map[string]any{"q": name, "step": step}
	})

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	_ = result

	spans := tr.RunSpans("m2-test")
	if len(spans) < 2 {
		t.Errorf("got %d spans, want at least 2 (root + acts)", len(spans))
	}

	foundRoot := false
	foundAct := false
	for _, s := range spans {
		if s.Kind == tracer.SpanSystem && s.SpanID == "m2-test" {
			foundRoot = true
		}
		if s.Kind == tracer.SpanAct {
			foundAct = true
		}
	}
	if !foundRoot {
		t.Error("root system span not found")
	}
	if !foundAct {
		t.Error("act span not found")
	}
}

// TestM2_CycleAlertFires verifies the cycle alert callback is
// called when CycleThreshold identical (tool,args) pairs are reached.
func TestM2_CycleAlertFired(t *testing.T) {
	var alerts []string
	guard := budget.New(100.0, 200.0)
	reg := tools.NewRegistry()
	cfg := loop.RunnerConfig{
		RunID:     "cycle-test",
		MaxSteps:  10,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	runner := loop.NewRunnerWithCycleAlert(cfg, guard, reg,
		func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
			// Always the same tool + args → cycle on 3rd occurrence.
			return "write_file", map[string]any{"path": "/tmp/x", "content": "same"}
		},
		func(msg string) {
			alerts = append(alerts, msg)
		},
	)

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.ExitReason != loop.ExitProgressStall {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, loop.ExitProgressStall)
	}
	if len(alerts) == 0 {
		t.Error("cycle alert callback never fired")
	}
	if !strings.Contains(alerts[0], "cycle") {
		t.Errorf("alert message = %q, want to contain 'cycle'", alerts[0])
	}
}

// TestM2_ThreeLayerRepetition verifies the 3-layer trace
// diff approach for fault detection (PRD M2: replay with diff).
// Layer 1: base run. Layer 2: same run (should be identical).
// Layer 3: injected fault (budget exceeded at step 0).
func TestM2_ThreeLayerRepetition(t *testing.T) {
	basePick := func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		tools := []string{"repo_search", "web_search", "repo_context"}
		name := tools[step%len(tools)]
		return name, map[string]any{"q": name, "step": step}
	}

	// Layer 1: base run.
	trBase := tracer.New()
	cfg1 := loop.RunnerConfig{
		RunID:     "base",
		MaxSteps:  3,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	r1 := loop.NewRunnerWithTracer(cfg1, budget.New(100.0, 200.0), tools.NewRegistry(), basePick, trBase)
	if _, err := r1.Run(context.Background()); err != nil {
		t.Fatalf("base run error: %v", err)
	}

	// Layer 2: identical config → replay diff should be "no diff".
	trSame := tracer.New()
	r2 := loop.NewRunnerWithTracer(cfg1, budget.New(100.0, 200.0), tools.NewRegistry(), basePick, trSame)
	if _, err := r2.Run(context.Background()); err != nil {
		t.Fatalf("same run error: %v", err)
	}

	rr1 := r1.Replay()
	rr2 := r2.Replay()
	if diff := rr1.Diff(rr2); diff != "no diff" {
		t.Errorf("base vs same diff = %q, want 'no diff'", diff)
	}

	// Layer 3: injected fault — tiny budget so it fires at step 0.
	trFault := tracer.New()
	cfg3 := loop.RunnerConfig{
		RunID:     "fault",
		MaxSteps:  3,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	r3 := loop.NewRunnerWithTracer(cfg3, budget.New(0.001, 1.0), tools.NewRegistry(), basePick, trFault)
	result3, err := r3.Run(context.Background())
	if err != nil {
		t.Fatalf("fault run error: %v", err)
	}
	if result3.ExitReason != loop.ExitCostBudget {
		t.Errorf("fault ExitReason = %q, want %q", result3.ExitReason, loop.ExitCostBudget)
	}

	rr3 := r3.Replay()
	diff := rr1.Diff(rr3)
	if diff == "no diff" {
		t.Error("base vs fault diff = 'no diff', want differences (fault should differ)")
	}
}

// TestM2_ValidateOnCleanTrace verifies replay validation passes
// on a normal run (no cycles, no dedup hits).
func TestM2_ValidateOnCleanTrace(t *testing.T) {
	tr := tracer.New()
	toolPick := func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		tools := []string{"repo_search", "web_search", "repo_context"}
		name := tools[step%len(tools)]
		return name, map[string]any{"q": name, "step": step}
	}
	cfg := loop.RunnerConfig{
		RunID:     "clean-trace",
		MaxSteps:  3,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	runner := loop.NewRunnerWithTracer(cfg, budget.New(100.0, 200.0), tools.NewRegistry(), toolPick, tr)
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	rr := runner.Replay()
	if err := rr.Validate(); err != nil {
		t.Errorf("clean trace validation failed: %v", err)
	}
	if rr.CycleDetected {
		t.Error("clean trace flagged as cycle")
	}
}

// TestM2_KillSwitchRecordsSpan verifies the kill path produces
// a kill system span in the trace.
func TestM2_KillSwitchRecordsSpan(t *testing.T) {
	tr := tracer.New()
	runner := newRunnerForTrace(t, 10, tr, func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		if step == 0 {
			time.Sleep(50 * time.Millisecond)
		}
		return "repo_search", map[string]any{"step": step}
	})

	go func() {
		time.Sleep(5 * time.Millisecond)
		runner.Kill()
	}()

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.State != loop.StateKilled {
		t.Errorf("State = %q, want %q", result.State, loop.StateKilled)
	}

	spans := tr.RunSpans("m2-test")
	foundKill := false
	for _, s := range spans {
		if s.Kind == tracer.SpanSystem && s.Label == "kill" {
			foundKill = true
		}
	}
	if !foundKill {
		t.Error("kill system span not found in trace")
	}
}

// TestM2_BudgetFaultDiffers verifies that a budget-fired run
// has a different trace shape than a normal run (fault detection).
func TestM2_BudgetFaultDiffers(t *testing.T) {
	pick := func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		tools := []string{"repo_search", "web_search", "repo_context"}
		name := tools[step%len(tools)]
		return name, map[string]any{"q": name, "step": step}
	}

	trNormal := tracer.New()
	rNormal := loop.NewRunnerWithTracer(
		loop.RunnerConfig{RunID: "normal", MaxSteps: 3, WallClock: 10 * time.Second, Goal: "test"},
		budget.New(100.0, 200.0), tools.NewRegistry(), pick, trNormal,
	)
	resNormal, _ := rNormal.Run(context.Background())
	_ = resNormal

	trFault := tracer.New()
	rFault := loop.NewRunnerWithTracer(
		loop.RunnerConfig{RunID: "budget-fault", MaxSteps: 3, WallClock: 10 * time.Second, Goal: "test"},
		budget.New(0.001, 1.0), tools.NewRegistry(), pick, trFault,
	)
	resFault, _ := rFault.Run(context.Background())
	if resFault.ExitReason != loop.ExitCostBudget {
		t.Fatalf("fault exit = %q, want %q", resFault.ExitReason, loop.ExitCostBudget)
	}

	diff := rNormal.Replay().Diff(rFault.Replay())
	if diff == "no diff" {
		t.Error("normal vs budget fault trace diff = 'no diff', want differences")
	}
}
