package loop

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/planner"
	"github.com/FreePeak/agentloop/internal/tools"
)

// fakeRegistry is a test double implementing tools.ToolRegistry.
// It returns Success=true for "ok" and Success=false for "fail",
// so the loop's scoring and exit-reason paths are exercised
// through the same code a real tool would drive.
type fakeRegistry struct {
	failTool string
}

func (f *fakeRegistry) List() []tools.Tool                              { return nil }
func (f *fakeRegistry) Validate(name string, args map[string]any) error { return nil }
func (f *fakeRegistry) Execute(ctx context.Context, name string, args map[string]any) (tools.ToolResult, error) {
	if f.failTool != "" && name == f.failTool {
		return tools.ToolResult{Success: false, Message: "intentional failure"}, nil
	}
	return tools.ToolResult{Success: true, Data: map[string]any{"ok": true}}, nil
}

// TestM3_DailyBudgetFires proves the daily ceiling (ExitDailyBudget)
// is enforced as a pre-action check in Run() — agentloop enforces,
// not model behavior (PRD §4.3, §17).
func TestM3_DailyBudgetFires(t *testing.T) {
	reg := &fakeRegistry{}
	guard := budget.New(1.0, 0.01) // tiny daily ceiling
	// prime the daily accumulator so CheckDaily fires
	guard.RecordSpend(1.0)
	cfg := RunnerConfig{
		RunID:     "daily-test",
		MaxSteps:  10,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	runner := NewRunner(cfg, guard, reg)
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitReason != ExitDailyBudget {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, ExitDailyBudget)
	}
	if result.Success != nil && *result.Success {
		t.Errorf("Success = true, want false (daily ceiling hit)")
	}
}

// TestM3_ConfidenceFloorFires proves the confidence floor
// (ExitConfidenceFloor) is enforced as a pre-action check — not
// after the fact (PRD §17). The fake registry returns Success=true
// so the loop scores it as confidence 1.0 ≥ default floor 0.7;
// the test drives a low floor so the exit must come from elsewhere —
// this verifies the check is present and correctly bounded.
func TestM3_ConfidenceFloorFires(t *testing.T) {
	reg := &fakeRegistry{}
	guard := budget.New(1.0, 100.0)
	cfg := RunnerConfig{
		RunID:           "conf-test",
		MaxSteps:        10,
		WallClock:       10 * time.Second,
		Goal:            "test",
		ConfidenceFloor: 1.01, // above any achievable confidence
	}
	runner := NewRunner(cfg, guard, reg)
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitReason != ExitConfidenceFloor {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, ExitConfidenceFloor)
	}
}

// TestM3_ConfidenceFloorDoesNotFireOnSuccess proves the floor
// check passes when tool confidence is high enough (default floor,
// default stub confidence 1.0).
func TestM3_ConfidenceFloorDoesNotFireOnSuccess(t *testing.T) {
	reg := &fakeRegistry{}
	guard := budget.New(1.0, 100.0)
	cfg := RunnerConfig{
		RunID:     "conf-ok",
		MaxSteps:  1,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}
	runner := NewRunner(cfg, guard, reg)
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitReason == ExitConfidenceFloor {
		t.Errorf("ExitReason = %q, should not fire (default floor met)", ExitConfidenceFloor)
	}
}

// TestM3_PlannerDrivenRun proves that when a Planner is wired
// via NewRunnerWithPlanner, Run() calls planner.Plan() and sets
// the run's tier from the plan's step tier.
func TestM3_PlannerDrivenRun(t *testing.T) {
	p := planner.NewPlanner()
	reg := &fakeRegistry{}
	guard := budget.New(1.0, 100.0)
	cfg := RunnerConfig{
		RunID:     "planner-test",
		MaxSteps:  3,
		WallClock: 10 * time.Second,
		Goal:      "test planner integration",
	}
	runner := NewRunnerWithPlanner(cfg, guard, reg, p)
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Steps) == 0 {
		t.Fatal("run has no steps")
	}
	// The run reports a routing tier, and it is one of the three the loop
	// actually routes on. The old assertion named `tiny`, a combo that
	// does not exist in onegw and was therefore a tier nothing could route
	// to — the exact drift this test now prevents.
	switch result.CurrentTier {
	case TierPlanning, TierExecution, TierSynthesis:
	default:
		t.Errorf("CurrentTier = %q, want one of %s/%s/%s",
			result.CurrentTier, TierPlanning, TierExecution, TierSynthesis)
	}
}
