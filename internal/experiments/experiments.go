// Package experiments computes TypeSafe guardrail routing decisions from
// pre-screened results. Used by the TypeSafe experiment script and kept
// as a runnable check (see the comment at the bottom).
package experiments

// HazardAction maps each Noul hazard id to its configured action.
var HazardAction = map[string]string{
	"jailbreak":      "block",
	"harmful_request": "block",
	"medical_advice":  "review",
	"self_harm":       "support",
	"broke_policy":    "block",
}

// Precedence order: highest first.
var Precedence = []string{"support", "block", "review", "pass"}

// Route takes per-hazard probabilities, the severity score, and a policy's
// thresholds, then returns the routed action (cookbook logic).
func Route(nouls map[string]float64, severity float64, policy Policy) string {
	triggered := make([]string, 0)
	for hazard, prob := range nouls {
		if prob >= policy.ActionThreshold {
			triggered = append(triggered, HazardAction[hazard])
		} else if prob >= policy.ReviewThreshold {
			triggered = append(triggered, "review")
		}
	}
	// severity converts review → block
	for i, a := range triggered {
		if a == "review" && severity >= policy.SeverityBlock {
			triggered[i] = "block"
		}
	}
	for _, p := range Precedence {
		for _, a := range triggered {
			if a == p {
				return p
			}
		}
	}
	return "pass"
}

// Policy holds the two thresholds + severity block line.
type Policy struct {
	ReviewThreshold   float64
	ActionThreshold   float64
	SeverityBlock     float64
}

var Strict = Policy{ReviewThreshold: 0.35, ActionThreshold: 0.70, SeverityBlock: 2.0}
var Permissive = Policy{ReviewThreshold: 0.35, ActionThreshold: 0.85, SeverityBlock: 2.0}
