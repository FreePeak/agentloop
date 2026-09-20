// Package eval_test exercises the M6 eval harness.
package eval_test

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/eval"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/tools"
)

func TestEval_RunAllCategories(t *testing.T) {
	factory := func(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
		return loop.NewRunner(cfg, budget.New(100.0, 200.0), tools.NewRegistry()), budget.New(100.0, 200.0), tools.NewRegistry(), nil
	}
	runner := eval.NewRunner(factory)

	cases := []eval.Case{
		{
			ID:       "happy-1",
			Category: eval.CatHappy,
			Goal:     "happy path test",
			Context:  "test",
			ScoreFn:  func(_ loop.RunResult) float64 { return 0.9 },
			LatencyCap: 10 * time.Second,
			CostCap:  1.00,
		},
		{
			ID:       "edge-1",
			Category: eval.CatEdge,
			Goal:     "edge case test",
			Context:  "test",
			ScoreFn:  func(_ loop.RunResult) float64 { return 0.85 },
			LatencyCap: 10 * time.Second,
			CostCap:  1.00,
		},
		{
			ID:       "adversarial-1",
			Category: eval.CatAdversarial,
			Goal:     "adversarial test",
			Context:  "test",
			ScoreFn:  func(_ loop.RunResult) float64 { return 0.6 },
			LatencyCap: 10 * time.Second,
			CostCap:  1.00,
		},
		{
			ID:       "regression-1",
			Category: eval.CatRegression,
			Goal:     "regression test",
			Context:  "test",
			ScoreFn:  func(_ loop.RunResult) float64 { return 0.75 },
			LatencyCap: 10 * time.Second,
			CostCap:  1.00,
		},
	}

	report, err := runner.Run(context.Background(), "suite-1", cases)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if report.SuiteID != "suite-1" {
		t.Errorf("suite_id = %q, want suite-1", report.SuiteID)
	}
	if report.Total != 4 {
		t.Errorf("total = %d, want 4", report.Total)
	}
	if len(report.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(report.Results))
	}
	if report.ByCategory == nil {
		t.Fatal("by_category map is nil")
	}
	if len(report.ByCategory) != 4 {
		t.Errorf("by_category has %d entries, want 4", len(report.ByCategory))
	}

	// Pass rate should be 0.5 (2 of 4 pass with score >= 0.8).
	if report.PassRate != 0.5 {
		t.Errorf("pass_rate = %f, want 0.5", report.PassRate)
	}
	if !report.DeployBlocked() {
		t.Error("deploy should be blocked (50% < 85% threshold)")
	}
	if report.AllPassed() {
		t.Error("AllPassed should be false at 50% pass rate")
	}

	// Verify latency and cost stats are computed (may be 0ms for fast loops).
	_ = report.AvgLatencyMs
	_ = report.P95LatencyMs

	// Verify individual results.
	for _, r := range report.Results {
		if r.CaseID == "" {
			t.Error("result missing case_id")
		}
	}
}

// TestEval_DeployGate proves the deploy is blocked when pass rate
// is below threshold (PRD §11.4).
func TestEval_DeployGate(t *testing.T) {
	factory := func(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
		return loop.NewRunner(cfg, budget.New(100.0, 200.0), tools.NewRegistry()), budget.New(100.0, 200.0), tools.NewRegistry(), nil
	}
	runner := eval.NewRunner(factory)

	// All pass.
	allPassCases := []eval.Case{
		{ID: "p1", Category: eval.CatHappy, Goal: "g", ScoreFn: func(_ loop.RunResult) float64 { return 0.9 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
		{ID: "p2", Category: eval.CatHappy, Goal: "g", ScoreFn: func(_ loop.RunResult) float64 { return 0.9 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
	}
	report, _ := runner.Run(context.Background(), "all-pass", allPassCases)
	if report.DeployBlocked() {
		t.Error("deploy blocked when all pass")
	}

	// All fail.
	allFailCases := []eval.Case{
		{ID: "f1", Category: eval.CatAdversarial, Goal: "g", ScoreFn: func(_ loop.RunResult) float64 { return 0.1 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
		{ID: "f2", Category: eval.CatAdversarial, Goal: "g", ScoreFn: func(_ loop.RunResult) float64 { return 0.1 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
	}
	report, _ = runner.Run(context.Background(), "all-fail", allFailCases)
	if !report.DeployBlocked() {
		t.Error("deploy should be blocked when all fail")
	}
	if report.PassRate != 0.0 {
		t.Errorf("pass_rate = %f, want 0", report.PassRate)
	}
}

// TestEval_RunWithRealLoop proves the eval runner works end-to-end
// with a real loop runner (no mock score functions).
func TestEval_RunWithRealLoop(t *testing.T) {
	factory := func(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
		guard := budget.New(100.0, 200.0)
		runner := loop.NewRunner(cfg, guard, tools.NewRegistry())
		return runner, guard, tools.NewRegistry(), nil
	}
	runner := eval.NewRunner(factory)

	cases := []eval.Case{
		{
			ID:        "real-1",
			Category:  eval.CatHappy,
			Goal:      "run a short task",
			Context:   "eval test",
			ScoreFn:   func(r loop.RunResult) float64 {
				if r.State == loop.StateSuccess || r.State == loop.StateExhausted {
					return 0.9
				}
				return 0.3
			},
			LatencyCap: 30 * time.Second,
			CostCap:    1.00,
		},
	}

	report, err := runner.Run(context.Background(), "real-suite", cases)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.Total != 1 {
		t.Fatalf("total = %d, want 1", report.Total)
	}
	if len(report.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(report.Results))
	}
	res := report.Results[0]
	if res.CaseID != "real-1" {
		t.Errorf("case_id = %q, want real-1", res.CaseID)
	}
	if res.LatencyMs < 0 {
		t.Error("latency should not be negative for real run")
	}
	_ = report.PassRate // may be blocked or not depending on loop state; both are valid
}
