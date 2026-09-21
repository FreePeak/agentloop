// The guardrail screen's wiring: who evaluates, what the verdict records,
// and which hazard is named as the reason. The policy itself (thresholds,
// precedence, hazard→action) stays in internal/experiments — this file
// only carries the verdict between the loop and the transport.
package loop

import "context"

// severityID is the id the severity Score carries in the guardrail battery
// (internal/systemone.SeverityID, and the one typesafe_experiments.sh sends).
// A constant rather than an import: this package must not depend on the
// transport that happens to define the battery.
const severityID = "severity"

// screening reports whether a screen is configured. Two surfaces exist and
// both mean "screen every step": the injected function (M2.x tests, and any
// caller that already has Noul scores) and the live client (M8.x transport).
// Injection wins when both are set, so a test can pin a verdict on a runner
// the service built with a live client.
func (r *LoopRunner) screening() bool {
	return r.guardrailScreen != nil || r.guardrail != nil
}

// screen asks for the Noul hazard map and severity Score for one state.
//
// The state carries the tool name alongside the result: "which tool
// produced this" is part of what is being judged (a `write_file` payload
// and a `web_search` payload with the same text are not the same risk),
// and the injected-function shape has always taken both.
func (r *LoopRunner) screen(ctx context.Context, tool string, data any) (map[string]float64, float64, error) {
	if r.guardrailScreen != nil {
		nouls, sev := r.guardrailScreen(tool, data)
		return nouls, sev, nil
	}
	return r.guardrail.Screen(ctx, map[string]any{"tool": tool, "result": data})
}

// topHazard names the hazard a verdict is mostly about, and its
// probability: the highest-scoring Noul, so a blocked run's record says
// which question fired rather than "noul_battery". A record that only
// carries the severity cannot answer the first question anyone asks of a
// block — "what did it think it saw?".
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
		return "noul_battery", 0
	}
	return best, prob
}

// screenRows is the audit record of one verdict: the hazard that fired with
// its probability, plus — when the severity Score is what escalated the
// action — the severity itself. A single hazard row on a severity-driven
// block would claim a low probability caused it.
func screenRows(hazard string, prob, severity float64, action string) []ScreenResult {
	rows := []ScreenResult{{Hazard: hazard, Prob: prob, Action: action}}
	if action == "block" && prob < severity {
		rows = append(rows, ScreenResult{Hazard: severityID, Prob: severity, Action: "block"})
	}
	return rows
}
