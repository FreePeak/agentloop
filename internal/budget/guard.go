// Package budget holds the per-run cost circuit breaker (P3).
// BudgetGuard checks spend BEFORE an action runs and reserves a
// synthesis buffer so a forced answer is already paid for when the
// budget fires. Enforcement is agentloop's — onegw reports usage,
// agentloop enforces (PRD §4.3).
package budget

import "fmt"

// PreSynthReserve is the fraction of budget held back to fund a
// forced synthesis when the ceiling fires. PRD §17: 10%.
const PreSynthReserve = 0.10

// ErrBudgetExceeded is returned by Check when spend has reached
// the pre-action threshold (costBudget * (1 - PreSynthReserve)).
var ErrBudgetExceeded = fmt.Errorf("budget exceeded: forced synthesis")

// Guard tracks spend against the per-run and per-day ceilings and
// owns the pre-synthesis reserve.
type Guard struct {
	CostBudget    float64 // per-run ceiling in USD
	DailyCeiling  float64 // per-tenant-day ceiling in USD
	SpendSoFar    float64 // cumulative spend this run
	DailySpend    float64 // cumulative spend this tenant-day
	SynthReserved bool    // true once the synthesis reserve is taken
	DailyExceeded bool    // true once daily ceiling is breached
}

// New returns a Guard for the given per-run budget and daily ceiling (USD).
func New(costBudget, dailyCeiling float64) *Guard {
	return &Guard{
		CostBudget:   costBudget,
		DailyCeiling: dailyCeiling,
	}
}

// Check returns ErrBudgetExceeded if spend has reached the pre-action
// threshold (costBudget * (1 - PreSynthReserve)). It also sets SynthReserved
// so the caller knows the synthesis buffer is committed.
func (g *Guard) Check() error {
	if g.SpendSoFar >= g.CostBudget*(1-PreSynthReserve) {
		return ErrBudgetExceeded
	}
	// First check past the threshold reserves the synthesis budget.
	if !g.SynthReserved {
		g.SynthReserved = true
	}
	return nil
}

// ReserveForSynthesis commits the 10% synthesis reserve. Returns false
// if it was already taken.
func (g *Guard) ReserveForSynthesis() bool {
	if g.SynthReserved {
		return false
	}
	g.SynthReserved = true
	return true
}

// RecordSpend adds amount to run and daily accumulators, then checks
// whether either ceiling was crossed.
func (g *Guard) RecordSpend(amount float64) {
	g.SpendSoFar += amount
	g.DailySpend += amount
	if g.DailySpend >= g.DailyCeiling {
		g.DailyExceeded = true
	}
}

// RunPct returns spend as a fraction of the per-run budget (0..1+).
func (g *Guard) RunPct() float64 {
	if g.CostBudget == 0 {
		return 0
	}
	return g.SpendSoFar / g.CostBudget
}

// CheckDaily fires ExitDailyBudget: the per-tenant-day ceiling was
// crossed by a prior RecordSpend. Pre-action check at the loop boundary.
func (g *Guard) CheckDaily() error {
	if g.DailyExceeded {
		return ErrDailyExceeded
	}
	return nil
}

// ErrDailyExceeded is returned by CheckDaily when the per-tenant-day
// ceiling was crossed.
var ErrDailyExceeded = fmt.Errorf("daily budget exceeded: forced synthesis")
