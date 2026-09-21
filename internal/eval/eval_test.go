// Package eval_test exercises the M6 eval harness.
package eval_test

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/eval"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/planner"
	"github.com/FreePeak/agentloop/internal/tools"
)

// serviceFactory builds the SAME runner the service builds: a planner, an
// approval gate, a fresh registry, and no model (the deploy gate must stay
// deterministic and offline, PRD §11.4). The eval suite's cases are premised on
// service behaviour — the adversarial case is "the gate holds" — so a factory
// that omits the gate cannot score them. TestEval_DefaultSuiteIsGreen below
// fails if the two ever drift apart again.
func serviceFactory(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
	g := budget.New(cfg.CostBudget, float64(loop.DailyCeilingMult)*cfg.CostBudget)
	reg := tools.NewRegistry()
	gate := loop.NewApprovalGate()
	cfg.Gate = gate
	return loop.NewRunnerWithPlannerAndGate(cfg, g, reg, planner.NewPlanner(), gate), g, reg, nil
}

// TestEval_DefaultSuiteIsGreen is the M6 acceptance in one assertion: the suite
// the deploy gate runs must pass against the runner the service actually
// builds.
//
// This is the check that was missing. The endpoint reported 0.5 (blocked) for
// days while `go test ./...` was green, because every test here used its own
// hand-built factory or its own score functions — nothing asserted that the
// DEFAULT suite passes.
func TestEval_DefaultSuiteIsGreen(t *testing.T) {
	runner := eval.NewRunner(serviceFactory)
	report, err := runner.Run(context.Background(), "default", eval.DefaultSuite())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.DeployBlocked() {
		for _, r := range report.Results {
			t.Logf("  %s (%s): passed=%v score=%.2f", r.CaseID, r.Category, r.Passed, r.Score)
		}
		t.Fatalf("the default suite is blocked at pass_rate = %.2f (threshold %.2f) — "+
			"either the eval factory no longer matches the service runner, or the "+
			"scoring no longer describes real behaviour",
			report.PassRate, eval.PassRateThreshold)
	}
	if !report.AllPassed() {
		t.Errorf("AllPassed() = false at pass_rate %.2f with %d/%d passing",
			report.PassRate, report.Passed, report.Total)
	}
}

// TestEval_DefaultSuiteCanFail is the other half: a suite that always passes is
// not a gate. Points the adversarial case at a runner with no gate — the shape
// the service had before M5 was wired — and requires the suite to notice.
func TestEval_DefaultSuiteCanFail(t *testing.T) {
	// No gate, no planner: the runner the eval factory used to build.
	gateless := func(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
		g := budget.New(cfg.CostBudget, float64(loop.DailyCeilingMult)*cfg.CostBudget)
		reg := tools.NewRegistry()
		return loop.NewRunner(cfg, g, reg), g, reg, nil
	}

	// Only the adversarial case, so the assertion is about the gate and
	// nothing else: with no gate there is no pause, and a pause is its pass.
	var adversarial eval.Case
	for _, c := range eval.DefaultSuite() {
		if c.Category == eval.CatAdversarial {
			adversarial = c
		}
	}
	if adversarial.ID == "" {
		t.Fatal("the default suite has no adversarial case — the guardrail is untested")
	}

	report, err := eval.NewRunner(gateless).Run(context.Background(), "gateless", []eval.Case{adversarial})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.Passed == report.Total {
		t.Errorf("the adversarial case passes against a GATELESS runner (score %.2f) — "+
			"the case no longer tests the guardrail", report.Results[0].Score)
	}
}

func TestEval_RunAllCategories(t *testing.T) {
	runner := eval.NewRunner(serviceFactory)

	cases := []eval.Case{
		{
			ID:         "happy-1",
			Category:   eval.CatHappy,
			Goal:       "happy path test",
			Context:    "test",
			ScoreFn:    func(_ loop.RunResult) float64 { return 0.9 },
			LatencyCap: 10 * time.Second,
			CostCap:    1.00,
		},
		{
			ID:         "edge-1",
			Category:   eval.CatEdge,
			Goal:       "edge case test",
			Context:    "test",
			ScoreFn:    func(_ loop.RunResult) float64 { return 0.85 },
			LatencyCap: 10 * time.Second,
			CostCap:    1.00,
		},
		{
			ID:         "adversarial-1",
			Category:   eval.CatAdversarial,
			Goal:       "adversarial test",
			Context:    "test",
			ScoreFn:    func(_ loop.RunResult) float64 { return 0.6 },
			LatencyCap: 10 * time.Second,
			CostCap:    1.00,
		},
		{
			ID:         "regression-1",
			Category:   eval.CatRegression,
			Goal:       "regression test",
			Context:    "test",
			ScoreFn:    func(_ loop.RunResult) float64 { return 0.75 },
			LatencyCap: 10 * time.Second,
			CostCap:    1.00,
		},
	}

	report, err := runner.Run(context.Background(), "suite-1", cases)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.Total != 4 {
		t.Errorf("total = %d, want 4", report.Total)
	}
	// 0.9, 0.85 pass; 0.6, 0.75 do not (threshold 0.8).
	if report.Passed != 2 {
		t.Errorf("passed = %d, want 2 (score threshold 0.8)", report.Passed)
	}
	if report.PassRate != 0.5 {
		t.Errorf("pass_rate = %f, want 0.5", report.PassRate)
	}
	if !report.DeployBlocked() {
		t.Error("50%% pass rate must block a deploy")
	}
	if report.AllPassed() {
		t.Error("AllPassed should be false at 50% pass rate")
	}
	// Every category must be represented in the report, even at 0%.
	for _, cat := range []string{"happy", "edge", "adversarial", "regression"} {
		if _, ok := report.ByCategory[cat]; !ok {
			t.Errorf("by_category missing %q", cat)
		}
	}
}

// TestEval_DeployGate proves the deploy is blocked when pass rate
// is below threshold (PRD §11.4).
func TestEval_DeployGate(t *testing.T) {
	runner := eval.NewRunner(serviceFactory)

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
	runner := eval.NewRunner(serviceFactory)

	cases := []eval.Case{
		{
			ID:         "real-1",
			Category:   eval.CatHappy,
			Goal:       "run the real loop",
			Context:    "test",
			ScoreFn:    nil, // scoreByState
			LatencyCap: 30 * time.Second,
			CostCap:    1.00,
		},
	}
	report, err := runner.Run(context.Background(), "real-suite", cases)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.Total != 1 {
		t.Errorf("total = %d, want 1", report.Total)
	}
	// The real loop must produce a scored result, not an error.
	if report.Results[0].Error != "" {
		t.Errorf("case error: %s", report.Results[0].Error)
	}
	_ = report.PassRate // may be blocked or not depending on loop state; both are valid
}
