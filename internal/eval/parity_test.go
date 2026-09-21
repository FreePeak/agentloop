package eval

import "testing"

// The parity comparison is the check M3's acceptance has been missing: "≥40%
// cheaper at eval parity" is only testable if a case that lost quality can
// fail the comparison. Each test below is a way the verdict must say no.
func TestParityRejectsACheaperButWorseCandidate(t *testing.T) {
	base := &Report{SuiteID: "baseline", AvgCostUSD: 0.40, Results: []Result{
		{CaseID: "a", Score: 0.90},
		{CaseID: "b", Score: 0.85},
	}}
	// Cheaper by 50%, but case b lost 0.10 — more than the 0.03 tolerance.
	cand := &Report{SuiteID: "candidate", AvgCostUSD: 0.20, Results: []Result{
		{CaseID: "a", Score: 0.90},
		{CaseID: "b", Score: 0.75},
	}}

	got := CompareParity(base, cand, ParityTolerance)
	if got.CostDelta >= 0 {
		t.Errorf("CostDelta = %v, want negative (candidate is cheaper)", got.CostDelta)
	}
	if got.Parity {
		t.Error("Parity = true, want false — a case lost more than the tolerance")
	}
	if got.Ship {
		t.Error("Ship = true, want false — the App. C bar is parity AND cheaper")
	}
	if len(got.Regressions) != 1 {
		t.Fatalf("Regressions = %v, want exactly one", got.Regressions)
	}
}

func TestParityShipsWithinTolerance(t *testing.T) {
	base := &Report{SuiteID: "baseline", AvgCostUSD: 0.40, Results: []Result{
		{CaseID: "a", Score: 0.90},
		{CaseID: "b", Score: 0.85},
	}}
	cand := &Report{SuiteID: "candidate", AvgCostUSD: 0.24, Results: []Result{
		{CaseID: "a", Score: 0.90},
		{CaseID: "b", Score: 0.83}, // lost 0.02, inside the 0.03 tolerance
	}}

	got := CompareParity(base, cand, ParityTolerance)
	if !got.Parity || !got.Ship {
		t.Errorf("Parity/Ship = %v/%v, want true/true (0.02 < 0.03 tolerance, 40%% cheaper)",
			got.Parity, got.Ship)
	}
	if got.Paired != 2 {
		t.Errorf("Paired = %d, want 2", got.Paired)
	}
}

// A case that disappeared is not a case that did not regress. Comparing by
// index would have paired a against a and silently dropped b.
func TestParityFailsOnAnUnpairedCase(t *testing.T) {
	base := &Report{SuiteID: "baseline", AvgCostUSD: 0.40, Results: []Result{
		{CaseID: "a", Score: 0.90},
		{CaseID: "b", Score: 0.85},
	}}
	cand := &Report{SuiteID: "candidate", AvgCostUSD: 0.20, Results: []Result{
		{CaseID: "a", Score: 0.90},
	}}

	got := CompareParity(base, cand, ParityTolerance)
	if got.Parity || got.Ship {
		t.Error("a dropped case must fail the comparison, not pass it")
	}
	if len(got.Unpaired) != 1 {
		t.Errorf("Unpaired = %v, want exactly one", got.Unpaired)
	}
}

// A candidate that costs more is not a migration — even at perfect parity.
func TestParityDoesNotShipAPricierCandidate(t *testing.T) {
	base := &Report{SuiteID: "baseline", AvgCostUSD: 0.20, Results: []Result{{CaseID: "a", Score: 0.9}}}
	cand := &Report{SuiteID: "candidate", AvgCostUSD: 0.30, Results: []Result{{CaseID: "a", Score: 0.9}}}

	got := CompareParity(base, cand, ParityTolerance)
	if !got.Parity {
		t.Error("Parity = false, want true — no case regressed")
	}
	if got.Cheaper || got.Ship {
		t.Errorf("Cheaper/Ship = %v/%v, want false/false", got.Cheaper, got.Ship)
	}
	if got.CostDelta <= 0 {
		t.Errorf("CostDelta = %v, want positive", got.CostDelta)
	}
}
