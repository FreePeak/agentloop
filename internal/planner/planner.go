// Package planner emits the plan for a run (PRD §5.1 FR-3, design.md §3).
//
// The Planner is deterministic in v1 — it produces 3–7 one-sentence steps
// from the goal's word count, each with success criteria and dependency
// marks. Real model-driven planning (tier-2 "planning" combo through onegw)
// is M3's production path; the fallback here is a rule-based shape so the
// containment suite can run without onegw.
//
// Tiering (PRD §13.1 M3): the plan's steps carry a tier each; the runner
// routes the tier through onegw combos (planning → planning combo, everything
// else → tiny). Routing is onegw's job; agentloop only attaches the tier.
package planner

import (
	"fmt"
	"strings"
	"sync"
)

// Plan is the output of the Planner — the mutable object the loop replans.
// Design.md §3: "Planner/Executor/Replanner — explicit plan object the loop
// mutates; executor ReAct-inside; replanner binary check".
type Plan struct {
	Goal         string       `json:"goal"`
	Context      string       `json:"context,omitempty"`
	Steps        []PlanStep   `json:"steps"`
	Tier         string       `json:"tier"` // routing tier: planning | tiny | execution
	Frame        string       `json:"frame"` // LOOP | AGENT | CHAIN | REFINE | SCALE
	ReplanNeeded bool         `json:"replan_needed,omitempty"` // true if Replan decided the plan must change
}

// PlanStep is one sentence in the plan. FR-3: 3–7 one-sentence steps with
// success criteria + dependency marks.
type PlanStep struct {
	Index        int      `json:"index"`
	Phase        string   `json:"phase"`         // decompose | reason | act | evaluate | synthesize (Ch.5)
	Instruction  string   `json:"instruction"`   // one sentence
	Success      string   `json:"success_criteria"` // predicate the loop evaluates after this step
	Dependencies []int    `json:"dependencies"`  // zero-based step indices this step reads from
	Tier         string   `json:"tier"`          // per-step tier override (planning/tiny/execution)
}

// PlannerConfig holds the tunables a plan is built from.
type PlannerConfig struct {
	Goal      string
	Context   string
	Tier      string // routing tier for the whole plan
	Frame     string // LOOP by default (§2: LOOP is the default single-agent run path)
	StepCount int    // test hook: set >0 to force a step count (0 = auto from goal length)
}

// PlanStepDone carries the result of one plan step back to the Replanner.
type PlanStepDone struct {
	Index      int     `json:"index"`
	Confidence float64 `json:"confidence"` // step-level confidence after execution
	NewData    bool    `json:"new_data"`   // did the step produce new state?
}

// NewPlanner returns a ready Planner. All behavior is deterministic
// in v1 (no model calls); tier routing is config, not code.
func NewPlanner() *Planner {
	return &Planner{}
}

// Planner is stateless in v1 — the plan is rebuilt from goal + context
// on every call so replans are reproducible.
type Planner struct {
	mu   sync.Mutex // guards the running plan across Replan calls
	plan *Plan
}

// Plan builds a Plan from goal + context. The step count follows the
// goal's length band (3–7 steps, per FR-3 "3–7 one-sentence steps").
// Independent steps (no data dependency) are marked with empty
// Dependencies so the runner can fan them out in parallel (P11).
func (p *Planner) Plan(cfg PlannerConfig) *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()

	tier := cfg.Tier
	if tier == "" {
		tier = "tiny"
	}
	frame := cfg.Frame
	if frame == "" {
		frame = "LOOP"
	}

	words := len(strings.Fields(cfg.Goal))
	stepCount := planStepCount(words)
	if cfg.StepCount > 0 {
		stepCount = cfg.StepCount
	}

	steps := make([]PlanStep, 0, stepCount)
	for i := 0; i < stepCount; i++ {
		phase := phaseFor(i, stepCount)
		deps := dependencies(i, stepCount)
		steps = append(steps, PlanStep{
			Index:        i,
			Phase:        phase,
			Instruction:  instruction(i, cfg.Goal, phase),
			Success:      successCriteria(i, phase),
			Dependencies: deps,
			Tier:         tier,
		})
	}

	p.plan = &Plan{
		Goal:    cfg.Goal,
		Context: cfg.Context,
		Steps:   steps,
		Tier:    tier,
		Frame:   frame,
	}
	return p.plan
}

// Replan takes the results of the executed steps and returns a new plan.
// Implements design.md §3's "binary replan check": Replanner runs a
// CONTINUE/REPLAN decision after every surprising step.
func (p *Planner) Replan(done []PlanStepDone, confidenceFloor float64) *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plan == nil {
		return nil
	}

	replan := false
	for _, d := range done {
		if !d.NewData || (d.Confidence > 0 && d.Confidence < confidenceFloor) {
			replan = true
			break
		}
	}

	newPlan := &Plan{
		Goal:         p.plan.Goal,
		Context:      p.plan.Context,
		Tier:         p.plan.Tier,
		Frame:        p.plan.Frame,
		ReplanNeeded: replan,
	}
	if !replan {
		// CONTINUE: carry the original plan forward unchanged.
		newPlan.Steps = copySteps(p.plan.Steps)
		return newPlan
	}

	// REPLAN: tighten instructions for every step after the first
	// surprising one, keep steps before it.
	steps := make([]PlanStep, 0, len(p.plan.Steps))
	tightened := false
	for i, s := range p.plan.Steps {
		stepDone := stepDone(done, i)
		if !tightened && (!stepDone.NewData || (stepDone.Confidence > 0 && stepDone.Confidence < confidenceFloor)) {
			tightened = true
		}
		cp := s
		if tightened {
			cp.Instruction = tighterInstruction(cp.Instruction)
			cp.Success = tighterSuccess(cp.Success)
		}
		steps = append(steps, cp)
	}
	newPlan.Steps = steps
	return newPlan
}

// Steps returns a copy of the current plan.
func (p *Planner) Steps() *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plan == nil {
		return nil
	}
	out := &Plan{
		Goal:  p.plan.Goal,
		Context: p.plan.Context,
		Tier:  p.plan.Tier,
		Frame: p.plan.Frame,
	}
	out.Steps = copySteps(p.plan.Steps)
	return out
}

// ParallelPhases groups plan steps into phases where independent
// steps can run concurrently. Steps that depend only on earlier
// phases (not on each other) share a phase. Returns a slice of
// phase groups, each a slice of step indices, in execution order.
// A phase group is what the runner fans out via errgroup
// (design.md §3: "one goroutine per phase, errgroup for fan-out").
func ParallelPhases(plan *Plan) [][]int {
	if plan == nil || len(plan.Steps) == 0 {
		return nil
	}

	prereqs := make(map[int][]int, len(plan.Steps))
	for i, s := range plan.Steps {
		prereqs[i] = s.Dependencies
	}

	var phases [][]int
	assigned := make(map[int]int, len(plan.Steps))
	remaining := len(plan.Steps)

	for remaining > 0 {
		phase := make([]int, 0)
		for i := range plan.Steps {
			if _, done := assigned[i]; done {
				continue
			}
			ready := true
			for _, dep := range prereqs[i] {
				if depPhase, ok := assigned[dep]; !ok || depPhase >= len(phases) {
					ready = false
					break
				}
			}
			if ready {
				phase = append(phase, i)
				assigned[i] = len(phases)
				remaining--
			}
		}
		if len(phase) == 0 {
			break
		}
		phases = append(phases, phase)
	}
	return phases
}

// planStepCount returns the number of plan steps for a goal of
// `words` words. 3–7 per FR-3 ("3–7 one-sentence steps").
func planStepCount(words int) int {
	switch {
	case words > 25:
		return 7
	case words > 6:
		return 5
	default:
		return 3
	}
}

func phaseFor(index, total int) string {
	switch {
	case index == 0:
		return "decompose"
	case index == total-1:
		return "synthesize"
	case total == 3:
		return "reason"
	case total == 5:
		switch index {
		case 1:
			return "reason"
		case 2:
			return "act"
		case 3:
			return "evaluate"
		}
		return "act"
	case total == 7:
		switch index {
		case 1:
			return "reason"
		case 5:
			return "evaluate"
		}
		return "act"
	}
	return "act"
}

// dependencies returns the step indices this step reads from.
// Independent steps (no predecessor dependency) have empty deps so the
// runner fans them out in parallel (P11 Parallel Loop).
func dependencies(index, total int) []int {
	if index == 0 {
		return nil
	}
	// Linear chain by default — step i depends on step i-1.
	// This is the conservative v1 shape; the runner collapses
	// dependency chains into phases for parallelism (design.md §3).
	return []int{index - 1}
}

func instruction(index int, goal, phase string) string {
	return instructionForPhase(index, phase) + " for: " + goal
}

func instructionForPhase(index int, phase string) string {
	switch phase {
	case "decompose":
		return "Break the goal into sub-goals"
	case "reason":
		return "Reason about the best approach"
	case "act":
		return "Execute the next action"
	case "evaluate":
		return "Evaluate the result against the goal"
	case "synthesize":
		return "Synthesize the final answer"
	default:
		return "Work on the goal"
	}
}

func successCriteria(index int, phase string) string {
	switch phase {
	case "decompose":
		return "Sub-goals enumerated and ordered"
	case "reason":
		return "Approach selected with rationale"
	case "act":
		return "Action completed without error"
	case "evaluate":
		return "Success criteria predicate holds"
	case "synthesize":
		return "Answer addresses the goal"
	default:
		return "Step completed"
	}
}

func tighterInstruction(inst string) string {
	return inst + " (tightened — revisit with narrower scope)"
}

func tighterSuccess(s string) string {
	return s + " — stricter threshold"
}

func copySteps(steps []PlanStep) []PlanStep {
	out := make([]PlanStep, len(steps))
	for i, s := range steps {
		cp := s
		deps := make([]int, len(s.Dependencies))
		copy(deps, s.Dependencies)
		cp.Dependencies = deps
		out[i] = cp
	}
	return out
}

func stepDone(done []PlanStepDone, index int) PlanStepDone {
	for _, d := range done {
		if d.Index == index {
			return d
		}
	}
	return PlanStepDone{Index: index}
}

// ValidateStepCount is a check function used in tests to prove
// that step counts are within the 3–7 band.
func ValidateStepCount(count int) error {
	if count < 3 || count > 7 {
		return fmt.Errorf("step count %d outside 3–7 band", count)
	}
	return nil
}
