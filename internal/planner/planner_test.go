package planner_test

import (
	"testing"

	"github.com/FreePeak/agentloop/internal/planner"
)

func TestPlan_StepCount(t *testing.T) {
	p := planner.NewPlanner()
	cases := []struct {
		name    string
		goal    string
		wantMin int
		wantMax int
	}{
		{"short", "Fix the bug", 3, 3},
		{"medium", "Research the impact of the new API and produce a report", 5, 5},
		{"long", "Redesign the authentication system to support SSO and OAuth2 and MFA and session management and token refresh across web and mobile and API clients while maintaining backward compatibility and improving the user experience across all platforms and services", 7, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := p.Plan(planner.PlannerConfig{Goal: c.goal})
			if plan == nil {
				t.Fatal("Plan returned nil")
			}
			if got := len(plan.Steps); got < c.wantMin || got > c.wantMax {
				t.Errorf("step count = %d, want %d–%d", got, c.wantMin, c.wantMax)
			}
			if err := planner.ValidateStepCount(len(plan.Steps)); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestPlan_StepShape(t *testing.T) {
	p := planner.NewPlanner()
	plan := p.Plan(planner.PlannerConfig{Goal: "Test goal", StepCount: 5})
	for i, step := range plan.Steps {
		if step.Index != i {
			t.Errorf("step %d has index %d", i, step.Index)
		}
		if step.Instruction == "" {
			t.Errorf("step %d: empty instruction", i)
		}
		if step.Success == "" {
			t.Errorf("step %d: empty success criteria", i)
		}
		for _, dep := range step.Dependencies {
			if dep < 0 || dep >= i {
				t.Errorf("step %d: dependency %d out of range", i, dep)
			}
		}
	}
}

func TestPlan_Phases(t *testing.T) {
	p := planner.NewPlanner()
	plan := p.Plan(planner.PlannerConfig{Goal: "Research the impact of the new API and produce a report", StepCount: 5})
	for i, w := range []string{"decompose", "reason", "act", "evaluate", "synthesize"} {
		if plan.Steps[i].Phase != w {
			t.Errorf("step %d phase = %q, want %q", i, plan.Steps[i].Phase, w)
		}
	}
}

func TestPlan_TierAndFrame(t *testing.T) {
	p := planner.NewPlanner()
	plan := p.Plan(planner.PlannerConfig{Goal: "Goal", Tier: "planning", Frame: "LOOP", StepCount: 5})
	if plan.Tier != "planning" {
		t.Errorf("tier = %q, want planning", plan.Tier)
	}
	plan2 := p.Plan(planner.PlannerConfig{Goal: "Goal", StepCount: 3})
	if plan2.Tier != "tiny" {
		t.Errorf("default tier = %q, want tiny", plan2.Tier)
	}
}

func TestReplan_Continue(t *testing.T) {
	p := planner.NewPlanner()
	p.Plan(planner.PlannerConfig{Goal: "Test goal with enough words for five steps to trigger the medium plan", StepCount: 5})

	done := []planner.PlanStepDone{
		{Index: 0, Confidence: 0.9, NewData: true},
		{Index: 1, Confidence: 0.85, NewData: true},
	}
	replan := p.Replan(done, 0.7)
	if replan == nil {
		t.Fatal("Replan returned nil")
	}
	if replan.ReplanNeeded {
		t.Error("ReplanNeeded = true, want false (CONTINUE)")
	}
	if len(replan.Steps) != len(p.Steps().Steps) {
		t.Errorf("replan step count = %d, want %d", len(replan.Steps), len(p.Steps().Steps))
	}
}

func TestReplan_Replan(t *testing.T) {
	p := planner.NewPlanner()
	orig := p.Plan(planner.PlannerConfig{Goal: "Test goal with enough words for five steps to trigger the medium plan", StepCount: 5})

	done := []planner.PlanStepDone{
		{Index: 0, Confidence: 0.9, NewData: true},
		{Index: 1, Confidence: 0.5, NewData: false},
	}
	replan := p.Replan(done, 0.7)
	if replan == nil {
		t.Fatal("Replan returned nil")
	}
	if !replan.ReplanNeeded {
		t.Error("ReplanNeeded = false, want true (REPLAN)")
	}
	if replan.Steps[0].Instruction != orig.Steps[0].Instruction {
		t.Error("step 0 instruction changed on REPLAN")
	}
	if replan.Steps[2].Instruction == orig.Steps[2].Instruction {
		t.Error("step 2 instruction not tightened")
	}
}

func TestReplan_NilPlan(t *testing.T) {
	p := planner.NewPlanner()
	replan := p.Replan(nil, 0.7)
	if replan != nil {
		t.Error("Replan on empty planner returned non-nil")
	}
}

func TestParallelPhases_LinearChain(t *testing.T) {
	p := planner.NewPlanner()
	plan := p.Plan(planner.PlannerConfig{Goal: "Test goal with enough words for five steps to trigger the medium plan", StepCount: 5})
	phases := planner.ParallelPhases(plan)
	if len(phases) != len(plan.Steps) {
		t.Fatalf("phases = %d, want %d", len(phases), len(plan.Steps))
	}
	for i, phase := range phases {
		if len(phase) != 1 || phase[0] != i {
			t.Errorf("phase %d = %v, want [%d]", i, phase, i)
		}
	}
}

func TestParallelPhases_IndependentSteps(t *testing.T) {
	plan := &planner.Plan{
		Goal: "independent",
		Tier: "tiny",
		Steps: []planner.PlanStep{
			{Index: 0, Dependencies: nil},
			{Index: 1, Dependencies: nil},
			{Index: 2, Dependencies: []int{0, 1}},
		},
	}
	phases := planner.ParallelPhases(plan)
	if len(phases) != 2 {
		t.Fatalf("phases = %d, want 2", len(phases))
	}
	if len(phases[0]) != 2 {
		t.Errorf("phase 0 = %v, want [0,1]", phases[0])
	}
	if phases[1][0] != 2 {
		t.Errorf("phase 1 = %v, want [2]", phases[1])
	}
}

func TestValidateStepCount(t *testing.T) {
	for _, c := range []struct {
		count int
		err   bool
	}{
		{3, false}, {5, false}, {7, false}, {2, true}, {8, true}, {0, true},
	} {
		err := planner.ValidateStepCount(c.count)
		if c.err && err == nil {
			t.Errorf("count %d: expected error, got nil", c.count)
		}
		if !c.err && err != nil {
			t.Errorf("count %d: expected nil, got %v", c.count, err)
		}
	}
}
