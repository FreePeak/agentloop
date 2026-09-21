package loop

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/experiments"
	"github.com/FreePeak/agentloop/internal/tools"
)

// stubScreen is a ScreenFunc with a fixed verdict — the live transport's
// shape without an endpoint.
func stubScreen(nouls map[string]float64, severity float64) ScreenFunc {
	return func(string) (map[string]float64, float64, error) { return nouls, severity, nil }
}

func guardrailCfg(runID string, policy experiments.Policy) (RunnerConfig, *budget.Guard, tools.ToolRegistry) {
	return RunnerConfig{
		RunID:     runID,
		MaxSteps:  3,
		WallClock: 10 * time.Second,
		Goal:      "test",
		Policy:    policy,
	}, budget.New(100.0, 200.0), tools.NewRegistry()
}

// TestScreen_VerdictIsRecorded: a block names the hazard that fired, so the
// run record answers "what did it see?" rather than "noul_battery".
func TestScreen_VerdictIsRecorded(t *testing.T) {
	tests := []struct {
		name     string
		nouls    map[string]float64
		severity float64
		want     string // the hazard the record must name
	}{
		// jailbreak 0.91 ≥ action 0.70 → block, on the hazard itself.
		{"hazard block", map[string]float64{"jailbreak": 0.91}, 0.5, "jailbreak"},
		// medical_advice routes to review, and severity ≥ 2.0 escalates it
		// to block — the severity decided it, so the severity is recorded.
		{"severity block", map[string]float64{"medical_advice": 0.99}, 2.05, severityID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, guard, reg := guardrailCfg("test-screen-"+tc.name, experiments.Strict)
			runner := NewRunner(cfg, guard, reg).WithGuardrailScreen(stubScreen(tc.nouls, tc.severity))

			result, err := runner.Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if result.ExitReason != ExitGuardrailBlock {
				t.Fatalf("ExitReason = %q, want %q", result.ExitReason, ExitGuardrailBlock)
			}
			var seen []ScreenResult
			var named bool
			for _, s := range result.Steps {
				for _, sc := range s.Screens {
					seen = append(seen, sc)
					if sc.Hazard == tc.want {
						named = true
					}
				}
			}
			if !named {
				t.Errorf("no screen row names %q; got %+v", tc.want, seen)
			}
		})
	}
}

// TestScreen_UnavailableIsRecordedNotClean is the load-bearing check for a
// screen that cannot run: the failure must be recorded on the run and the
// step must not be dropped, because "nothing was flagged" and "nothing was
// checked" are different facts and only one of them is a containment claim.
func TestScreen_UnavailableIsRecordedNotClean(t *testing.T) {
	cfg, guard, reg := guardrailCfg("test-screen-down", experiments.Strict)
	runner := NewRunner(cfg, guard, reg).WithGuardrailScreen(
		func(string) (map[string]float64, float64, error) {
			return nil, 0, context.DeadlineExceeded
		})

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.ScreenErrors) == 0 {
		t.Fatal("ScreenErrors is empty: a run that was never screened looks exactly like a clean one")
	}
	if result.ExitReason == ExitGuardrailBlock {
		t.Error("an outage was reported as a policy block")
	}
	var recorded bool
	for _, s := range result.Steps {
		for _, sc := range s.Screens {
			if sc.Action == "unavailable" && sc.Error != "" {
				recorded = true
			}
		}
	}
	if !recorded {
		t.Error("no step records the screen failure with its cause")
	}
}

// TestScreen_PolicySelectsThresholds: the same verdict routes differently
// under the two measured policies (§7.2's jailbreak-as-medical case), so the
// run must be able to say which policy it screened under.
func TestScreen_PolicySelectsThresholds(t *testing.T) {
	// Between the two action thresholds: ≥0.70 strict, <0.85 permissive.
	between := map[string]float64{"jailbreak": 0.75}

	cfg, guard, reg := guardrailCfg("test-policy-strict", experiments.Strict)
	runner := NewRunner(cfg, guard, reg).WithGuardrailScreen(stubScreen(between, 0.5))
	strict, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if strict.ExitReason != ExitGuardrailBlock {
		t.Errorf("strict ExitReason = %q, want %q for p=0.75", strict.ExitReason, ExitGuardrailBlock)
	}

	cfg, guard, reg = guardrailCfg("test-policy-permissive", experiments.Permissive)
	runner = NewRunner(cfg, guard, reg).WithGuardrailScreen(stubScreen(between, 0.5))
	permissive, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if permissive.ExitReason == ExitGuardrailBlock {
		t.Error("permissive blocked at p=0.75, below its 0.85 action threshold")
	}
}

// TestTopHazard_StableOnTie: map iteration is randomised in Go, so an
// unstable tie-break would make the recorded reason differ run to run.
func TestTopHazard_StableOnTie(t *testing.T) {
	nouls := map[string]float64{"self_harm": 0.8, "jailbreak": 0.8}
	for i := 0; i < 50; i++ {
		if h, p := topHazard(nouls); h != "jailbreak" || p != 0.8 {
			t.Fatalf("topHazard = (%q, %v), want (jailbreak, 0.8) on every iteration", h, p)
		}
	}
	if h, _ := topHazard(nil); h != "" {
		t.Errorf("empty battery hazard = %q, want \"\" (no hazard was reported)", h)
	}
	if rows := ScreenRowsFor(nil, 1.5, "review"); len(rows) != 1 || rows[0].Hazard != "noul_battery" {
		t.Errorf("rows for an unreported battery = %+v, want the single legacy row", rows)
	}
}
