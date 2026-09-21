// The guardrail screen's wiring: what the loop hands the screen, what the
// verdict records, and which hazard is named as the reason. The policy itself
// (thresholds, precedence, hazard→action) stays in internal/experiments; the
// transport stays behind the loop's ScreenFunc.
package loop

// severityID is the id the severity Score carries in the guardrail battery
// (the battery internal/guardrail sends, and the one typesafe_experiments.sh
// sends). A constant rather than an import: this package must not depend on
// the transport that happens to define the battery.
const severityID = "severity"

// topHazard names the hazard a verdict is mostly about, and its
// probability: the highest-scoring Noul, so a blocked run's record says
// which question fired rather than "noul_battery". A record that only
// carries the severity cannot answer the first question anyone asks of a
// block — "what did it think it saw?".
//
// A nil or empty map — a caller that screens without reporting hazards —
// keeps the old single-row shape rather than inventing a hazard.
func topHazard(nouls map[string]float64) (string, float64) {
	best, prob := "", -1.0
	for hazard, p := range nouls {
		// Ties break on the hazard id so the recorded reason is stable
		// across map iteration order (a Go map is randomised).
		if p > prob || (p == prob && hazard < best) {
			best, prob = hazard, p
		}
	}
	if best == "" {
		return "", 0
	}
	return best, prob
}

// screenRows is the audit record of one verdict: the hazard that fired with
// its probability, plus — when the severity Score is what escalated the
// action — the severity itself. A single hazard row on a severity-driven
// block would claim a low probability caused it.
func screenRows(hazard string, prob, severity float64, action string) []ScreenResult {
	if hazard == "" {
		return []ScreenResult{{Hazard: "noul_battery", Prob: severity, Action: action}}
	}
	rows := []ScreenResult{{Hazard: hazard, Prob: prob, Action: action}}
	if action == "block" && prob < severity {
		rows = append(rows, ScreenResult{Hazard: severityID, Prob: severity, Action: "block"})
	}
	return rows
}
