// Package eval's parity half: the paired comparison that decides whether a
// cheaper configuration is actually cheaper.
//
// PRD §11.4 requires it and M3's acceptance depends on it: "a 5+-step task is
// ≥40% cheaper than single-tier ReAct with no case scoring below the baseline
// by more than its eval tolerance". Cost is measurable on its own; the second
// half is what stops a cheaper loop that quietly got worse from passing. Until
// this file existed, that half was prose — the PRD's own Appendix F (F1) said
// so: "parity is not measured anywhere in §11.2".
//
// The same comparison is the book's framework-migration bar (App. C): run both
// configurations through the same suite and ship the candidate only within
// tolerance. That is what makes §15's D9 falsifiable rather than asserted.
package eval

import "fmt"

// ParityTolerance is how far a single case may fall below the baseline before
// the comparison fails, in score points. It is a *tolerance*, not a target:
// the book's ship bar is "within 3%", and a case that loses more than this is
// a regression the headline cost number is not allowed to hide.
const ParityTolerance = 0.03

// ParityResult is the paired verdict for one configuration pair.
type ParityResult struct {
	Baseline    string   `json:"baseline"`
	Candidate   string   `json:"candidate"`
	Paired      int      `json:"paired"`
	Unpaired    []string `json:"unpaired,omitempty"`
	Regressions []string `json:"regressions,omitempty"`
	CostDelta   float64  `json:"cost_delta"` // fraction, negative = candidate cheaper
	Parity      bool     `json:"parity"`     // no case lost more than the tolerance
	Cheaper     bool     `json:"cheaper"`    // candidate cost is strictly lower
	Ship        bool     `json:"ship"`       // parity AND cheaper — the App. C bar
}

// CompareParity pairs two reports case by case and reports whether the
// candidate is a legitimate improvement.
//
// Pairing is by CaseID, not by index: two reports of different lengths or
// orders would otherwise compare case 3 against case 7 and call it parity.
// A case present in only one report is reported as unpaired and fails the
// comparison — a case that silently vanished is not a case that did not
// regress.
func CompareParity(baseline, candidate *Report, tolerance float64) ParityResult {
	res := ParityResult{
		Baseline:  baseline.SuiteID,
		Candidate: candidate.SuiteID,
	}
	if tolerance <= 0 {
		tolerance = ParityTolerance
	}

	byID := make(map[string]Result, len(candidate.Results))
	for _, r := range candidate.Results {
		byID[r.CaseID] = r
	}
	seen := make(map[string]bool, len(baseline.Results))

	for _, b := range baseline.Results {
		c, ok := byID[b.CaseID]
		if !ok {
			res.Unpaired = append(res.Unpaired, b.CaseID+" (missing from candidate)")
			continue
		}
		seen[b.CaseID] = true
		res.Paired++
		if c.Score < b.Score-tolerance {
			res.Regressions = append(res.Regressions,
				fmt.Sprintf("%s: %.3f → %.3f (lost %.3f, tolerance %.3f)",
					b.CaseID, b.Score, c.Score, b.Score-c.Score, tolerance))
		}
	}
	for _, c := range candidate.Results {
		if !seen[c.CaseID] {
			res.Unpaired = append(res.Unpaired, c.CaseID+" (new in candidate)")
		}
	}

	if baseline.AvgCostUSD > 0 {
		res.CostDelta = (candidate.AvgCostUSD - baseline.AvgCostUSD) / baseline.AvgCostUSD
	}
	res.Cheaper = candidate.AvgCostUSD < baseline.AvgCostUSD
	res.Parity = len(res.Regressions) == 0 && len(res.Unpaired) == 0
	res.Ship = res.Parity && res.Cheaper
	return res
}
