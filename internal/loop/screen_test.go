package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/experiments"
	"github.com/FreePeak/agentloop/internal/tools"
)

// stubScreen is a GuardrailClient with a fixed verdict or failure — the
// live path's shape without an endpoint.
type stubScreen struct {
	nouls    map[string]float64
	severity float64
	err      error
}

func (s stubScreen) Screen(context.Context, any) (map[string]float64, float64, error) {
	return s.nouls, s.severity, s.err
}

func guardrailCfg(runID string) (RunnerConfig, *budget.Guard, tools.ToolRegistry) {
	return RunnerConfig{
		RunID:     runID,
		MaxSteps:  3,
		WallClock: 10 * time.Second,
		Goal:      "test",
	}, budget.New(100.0, 200.0), tools.NewRegistry()
}

// TestScreen_UnavailableFailsClosed is the load-bearing check for the live
// transport: when the battery cannot be evaluated, the run must stop with a
// reason that says so. Collapsing it into "pass" is how a guardrail outage
// becomes an unscreened run, and collapsing it into guardrail_block hides
// the outage as a policy hit (PRD §4.3 fail-closed).
func TestScreen_UnavailableFailsClosed(t *testing.T) {
	cfg, guard, reg := guardrailCfg("test-screen-down")
	cfg.Guardrail = stubScreen{err: errors.New("systemone: connection refused")}

	result, err := NewRunner(cfg, guard, reg).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitReason != ExitGuardrailUnavailable {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, ExitGuardrailUnavailable)
	}
	if result.ExitReason == ExitGuardrailBlock {
		t.Error("an outage was reported as a policy block")
	}
	if result.Success == nil || *result.Success {
		t.Error("Success = true, want false (screen unavailable)")
	}
	if result.ScreenError == "" {
		t.Error("ScreenError empty — the trajectory cannot say why the screen failed")
	}
	if result.PartialSynthesis == "" {
		t.Error("PartialSynthesis empty on a fail-closed exit")
	}
}

// TestScreen_VerdictIsRecorded: a block names the hazard that fired and the
// severity that decided it, so the run record answers "what did it see?"
// rather than "noul_battery".
func TestScreen_VerdictIsRecorded(t *testing.T) {
	tests := []struct {
		name     string
		nouls    map[string]float64
		severity float64
		want     string // the hazard the record must name
		rows     int
	}{
		// jailbreak 0.91 ≥ action 0.70 → block, on the hazard itself.
		{"hazard block", map[string]float64{"jailbreak": 0.91}, 0.5, "jailbreak", 1},
		// medical_advice routes to review, and severity ≥ 2.0 escalates it
		// to block — the severity decided it, so the severity is recorded.
		{"severity block", map[string]float64{"medical_advice": 0.99}, 2.05, severityID, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, guard, reg := guardrailCfg("test-screen-" + tc.name)
			cfg.Guardrail = stubScreen{nouls: tc.nouls, severity: tc.severity}

			result, err := NewRunner(cfg, guard, reg).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if result.ExitReason != ExitGuardrailBlock {
				t.Fatalf("ExitReason = %q, want %q", result.ExitReason, ExitGuardrailBlock)
			}
			var named bool
			var rows int
			var seen []ScreenResult
			for _, s := range result.Steps {
				for _, sc := range s.Screens {
					rows++
					seen = append(seen, sc)
					if sc.Hazard == tc.want {
						named = true
					}
				}
			}
			if !named {
				t.Errorf("no screen row names %q; got %+v", tc.want, seen)
			}
			if rows != tc.rows {
				t.Errorf("recorded %d screen rows, want %d", rows, tc.rows)
			}
		})
	}
}

// TestScreen_PolicySelectsThresholds: the same verdict routes differently
// under the two measured policies (§7.2's jailbreak-as-medical case), so a
// run must be able to say which policy it ran under.
func TestScreen_PolicySelectsThresholds(t *testing.T) {
	// medical_advice → review; severity below the block line.
	nouls, severity := map[string]float64{"medical_advice": 0.63}, 1.0

	strictCfg, guard, reg := guardrailCfg("test-policy-strict")
	strictCfg.Guardrail = stubScreen{nouls: nouls, severity: severity}
	strict, err := NewRunner(strictCfg, guard, reg).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	permissiveCfg, guard, reg := guardrailCfg("test-policy-permissive")
	permissiveCfg.Guardrail = stubScreen{nouls: nouls, severity: severity}
	permissiveCfg.Policy = experiments.Permissive
	permissive, err := NewRunner(permissiveCfg, guard, reg).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Both hold the step (review is review under either), and both must be
	// the same state — the difference shows up on the action thresholds,
	// which is why the policy is a config field rather than a constant.
	if strict.State != StatePausedApproval || permissive.State != StatePausedApproval {
		t.Errorf("states = %q / %q, want paused_approval under both policies", strict.State, permissive.State)
	}
	// A block verdict under permissive's action threshold must not block
	// under strict's when the probability sits between them.
	between := map[string]float64{"jailbreak": 0.75} // ≥0.70 strict, <0.85 permissive
	strictCfg.Guardrail = stubScreen{nouls: between, severity: 0.5}
	if r, err := NewRunner(strictCfg, guard, reg).Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	} else if r.ExitReason != ExitGuardrailBlock {
		t.Errorf("strict ExitReason = %q, want %q for p=0.75", r.ExitReason, ExitGuardrailBlock)
	}
	permissiveCfg.Guardrail = stubScreen{nouls: between, severity: 0.5}
	if r, err := NewRunner(permissiveCfg, guard, reg).Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	} else if r.ExitReason == ExitGuardrailBlock {
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
	if h, _ := topHazard(nil); h != "noul_battery" {
		t.Errorf("empty battery hazard = %q, want noul_battery", h)
	}
}
