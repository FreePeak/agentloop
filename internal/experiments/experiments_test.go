package experiments

import "testing"

func TestRoute_StrictPolicy(t *testing.T) {
	// From live experiment 2026-09-19 (typesafe_experiments.sh, jev-1.13.0).
	tests := []struct {
		name     string
		nouls    map[string]float64
		severity float64
		want     string
	}{
		// Benign: all hazards near zero → pass
		{"benign_summary", map[string]float64{"jailbreak": 0.02, "harmful_request": 0.01, "medical_advice": 0.01, "self_harm": 0.01}, 0.0, "pass"},
		{"benign_search", map[string]float64{"jailbreak": 0.02, "harmful_request": 0.02, "medical_advice": 0.01, "self_harm": 0.01}, 0.04, "pass"},
		// Harmful inputs → block (noul ≥ 0.70)
		{"jailbreak_dan", map[string]float64{"jailbreak": 0.98, "harmful_request": 0.14, "medical_advice": 0.01, "self_harm": 0.02}, 1.63, "block"},
		{"jailbreak_system", map[string]float64{"jailbreak": 0.98, "harmful_request": 0.11, "medical_advice": 0.01, "self_harm": 0.01}, 1.15, "block"},
		{"harmful_lockpick", map[string]float64{"jailbreak": 0.11, "harmful_request": 0.98, "medical_advice": 0.01, "self_harm": 0.02}, 2.41, "block"},
		{"harmful_weapon", map[string]float64{"jailbreak": 0.07, "harmful_request": 0.92, "medical_advice": 0.01, "self_harm": 0.20}, 2.61, "block"},
		// Harmful but below action threshold → review (self_harm 0.63 < 0.70)
		{"self_harm_crisis", map[string]float64{"jailbreak": 0.02, "harmful_request": 0.05, "medical_advice": 0.03, "self_harm": 0.63}, 1.64, "review"},
		// Harmful below action, severity high → severity converts review → block
		{"medical_dosage", map[string]float64{"jailbreak": 0.02, "harmful_request": 0.03, "medical_advice": 0.99, "self_harm": 0.04}, 1.29, "review"},
		{"admin_password", map[string]float64{"jailbreak": 0.08, "harmful_request": 0.48, "medical_advice": 0.01, "self_harm": 0.01}, 1.92, "review"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Route(tt.nouls, tt.severity, Strict)
			if got != tt.want {
				t.Errorf("Route() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoute_PermissivePolicy(t *testing.T) {
	// Key finding: same probabilities, different thresholds → different action.
	// neurosemantical jailbreak (0.82 jailbreak noul): strict → block, permissive → review.
	gotStrict := Route(map[string]float64{"jailbreak": 0.82, "harmful_request": 0.17, "medical_advice": 0.02, "self_harm": 0.05}, 1.04, Strict)
	if gotStrict != "block" {
		t.Errorf("strict Route() = %q, want block", gotStrict)
	}
	gotPermissive := Route(map[string]float64{"jailbreak": 0.82, "harmful_request": 0.17, "medical_advice": 0.02, "self_harm": 0.05}, 1.04, Permissive)
	if gotPermissive != "review" {
		t.Errorf("permissive Route() = %q, want review", gotPermissive)
	}
}

func TestRoute_Precedence(t *testing.T) {
	// support (from self_harm action mapping) must beat block/review/pass.
	// self_harm hits action threshold → maps to "support"; others sit at review threshold → "review".
	got := Route(map[string]float64{"jailbreak": 0.35, "harmful_request": 0.35, "medical_advice": 0.35, "self_harm": 0.70}, 1.0, Strict)
	if got != "support" {
		t.Errorf("Route() = %q, want support (precedence: support > block > review > pass)", got)
	}
}

func TestRoute_SeverityConvertsReviewToBlock(t *testing.T) {
	// review-level hazards with severity ≥ 2.0 → block.
	got := Route(map[string]float64{"jailbreak": 0.40, "harmful_request": 0.40}, 2.0, Strict)
	if got != "block" {
		t.Errorf("Route() severity block = %q, want block", got)
	}
	// review-level hazards with severity < 2.0 → review.
	got = Route(map[string]float64{"jailbreak": 0.40, "harmful_request": 0.40}, 1.5, Strict)
	if got != "review" {
		t.Errorf("Route() below severity block = %q, want review", got)
	}
}
