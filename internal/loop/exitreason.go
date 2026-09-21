// Package loop holds the run lifecycle types: the exit reasons a loop can end with,
// the in-flight states a run passes through, and the constants that bound every loop.
package loop

// ExitReason names why a run stopped. Success and stopping are separate things:
// a run can end with exitReason=max_steps and success=false (ceiling hit, goal
// not met), or exitReason="" and success=true (goal met before any ceiling).
type ExitReason string

const (
	ExitMaxSteps            ExitReason = "max_steps"
	ExitWallClock           ExitReason = "wall_clock"
	ExitCostBudget          ExitReason = "cost_budget"
	ExitDailyBudget         ExitReason = "daily_budget"
	ExitConfidenceFloor     ExitReason = "confidence_floor"
	ExitProgressStall       ExitReason = "progress_stall"
	ExitConsecutiveFailures ExitReason = "consecutive_failures"
	ExitGuardrailBlock      ExitReason = "guardrail_block" // M2.x: TypeSafe screen blocked
	// ExitGuardrailUnavailable is the screen failing closed (M2.x,
	// PRD §4.3): the battery could not be evaluated, so nothing may act
	// on the unscreened content. It is a distinct exit from a block()
	// on purpose — "the screen said no" and "there was no screen" are
	// different incidents, and collapsing them would hide an outage as
	// a policy hit.
	ExitGuardrailUnavailable ExitReason = "guardrail_unavailable"
	// ExitGoalMet is the goal predicate firing, which is not a bound: the
	// run stopped because it was done. It is the one exit a reasoner can
	// reach on its own, and it is distinct from max_steps on purpose
	// (PRD move 2: "separate success from stopping").
	ExitGoalMet ExitReason = "goal_met"
)

// AllExitReasons lists every ExitReason in declaration order. Used by tests to
// prove the enum is exhaustive and by console code to render exit reasons.
func AllExitReasons() []ExitReason {
	return []ExitReason{
		ExitMaxSteps, ExitWallClock, ExitCostBudget, ExitDailyBudget,
		ExitConfidenceFloor, ExitProgressStall, ExitConsecutiveFailures,
		ExitGuardrailBlock, ExitGuardrailUnavailable, ExitGoalMet,
	}
}

// String implements fmt.Stringer.
func (e ExitReason) String() string { return string(e) }

// ParseExitReason returns the ExitReason matching s or ok=false.
func ParseExitReason(s string) (ExitReason, bool) {
	for _, r := range AllExitReasons() {
		if r.String() == s {
			return r, true
		}
	}
	return "", false
}

// State is where a run is in its lifecycle. Terminal states are success,
// exhausted, failed, escalated, killed. A run with exitReason="" and
// success=true means the goal predicate held before any ceiling fired.
type State string

const (
	StateQueued         State = "queued"
	StateThinking       State = "thinking"
	StateActing         State = "acting"
	StateEvaluating     State = "evaluating"
	StatePausedApproval State = "paused_approval"
	StateSuccess        State = "success"
	StateExhausted      State = "exhausted"
	StateFailed         State = "failed"
	StateEscalated      State = "escalated"
	StateKilled         State = "killed"
)

// AllStates lists every State in declaration order.
func AllStates() []State {
	return []State{
		StateQueued, StateThinking, StateActing, StateEvaluating,
		StatePausedApproval, StateSuccess, StateExhausted, StateFailed,
		StateEscalated, StateKilled,
	}
}

// IsTerminal returns true for states a run can end in.
func (s State) IsTerminal() bool {
	switch s {
	case StateSuccess, StateExhausted, StateFailed, StateEscalated, StateKilled:
		return true
	}
	return false
}

// Run constants — every value is a calibrated prior from PRD §17, none a spec.
const (
	MaxSteps            = 9    // PRD §17: p95 staging completions × 1.3
	WallClockS          = 120  // PRD §17: platform p95 × 1.3
	CostBudgetUSD       = 1.00 // PRD §17: App. G default tier
	DailyCeilingMult    = 20   // PRD §17: 20× per-run budget per tenant-day
	CycleThreshold      = 3    // PRD §17: 3 identical (tool,args) pairs = cycle
	ConsecutiveFailures = 3    // PRD §17: 3 consecutive failures → escalate
)

// M3 defaults — calibrated priors, not specs.
const (
	ConfidenceFloorDefault = 0.7 // PRD §17: floor below which the loop escalates
	EscalationThreshold    = 0.7 // PRD §17: run-level confidence floor
)
