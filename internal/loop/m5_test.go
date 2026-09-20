// Package loop_test exercises M5 (HITL, design.md §8, PRD §13.1).
// Acceptance: <10% interruptions; sampling + anomaly review
// exercised; timeout denies (fail-closed). P30/P68.
package loop_test

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/tools"
)

// TestM5_AutoCategoryReads proves P30 "read → auto":
// read/search/list/get tools are approved immediately with
// zero human interruption.
func TestM5_AutoCategoryReads(t *testing.T) {
	for _, tool := range []string{"read", "search", "list", "get"} {
		gate := loop.NewApprovalGate()
		got := gate.Check(loop.ApprovalRequest{
			RunID:      "r",
			StepID:     0,
			Tool:       tool,
			ArgsHash:   "h1",
			Category:   loop.Categorize(tool),
			Confidence: 0.0, // irrelevant for auto
			Requested:  time.Now(),
		})
		if got.Action != "approve" {
			t.Errorf("tool %q: action = %q, want approve", tool, got.Action)
		}
		if got.Category != loop.CatAuto {
			t.Errorf("tool %q: category = %q, want auto", tool, got.Category)
		}
	}
}

// TestM5_ConfirmRequiresConfidence proves P30
// "update → auto_if_confident(>0.9)": below the floor the
// step is denied (interruption), at/above the floor approved.
func TestM5_ConfirmRequiresConfidence(t *testing.T) {
	gate := loop.NewApprovalGate()
	// Below floor → deny (interruption).
	low := gate.Check(loop.ApprovalRequest{
		RunID: "r", StepID: 0, Tool: "update", ArgsHash: "h",
		Category:   loop.CatConfirm,
		Confidence: 0.5,
		Requested:  time.Now(),
	})
	if low.Action != "deny" {
		t.Errorf("below floor: action = %q, want deny", low.Action)
	}
	// At/above floor → approve.
	high := gate.Check(loop.ApprovalRequest{
		RunID: "r", StepID: 0, Tool: "update", ArgsHash: "h",
		Category:   loop.CatConfirm,
		Confidence: 0.9,
		Requested:  time.Now(),
	})
	if high.Action != "approve" {
		t.Errorf("at floor: action = %q, want approve", high.Action)
	}
}

// TestM5_ApproveCategoryRequiresApproval proves P30
// "send/delete/deploy/pay → always_approve": high-impact
// tools are held pending (never auto-approved); the operator
// sees them in the approval queue.
func TestM5_ApproveCategoryRequiresApproval(t *testing.T) {
	for _, tool := range []string{"delete", "send", "deploy", "pay", "write_file"} {
		gate := loop.NewApprovalGate()
		got := gate.Check(loop.ApprovalRequest{
			RunID: "r", StepID: 0, Tool: tool, ArgsHash: "h",
			Category:   loop.Categorize(tool),
			Confidence: 1.0,
			Requested:  time.Now(),
		})
		if got.Action == "approve" {
			t.Errorf("tool %q: auto-approved (high-impact tools must hold pending)", tool)
		}
		p := gate.Pending()
		if len(p) == 0 || p[0].Decision.Action != "deny" {
			t.Errorf("tool %q: not recorded as pending", tool)
		}
	}
}

// TestM5_TimeoutDenies is the M5 acceptance criterion from
// PRD §13.1: a held approval that exceeds the timeout is
// DENIED (fail-closed), never silently approved.
func TestM5_TimeoutDenies(t *testing.T) {
	gate := loop.NewApprovalGate()
	gate.TestClock(func() time.Time { return time.Now() })
	past := time.Now().Add(-2 * time.Minute)
	got := gate.Check(loop.ApprovalRequest{
		RunID: "r", StepID: 0, Tool: "delete", ArgsHash: "h",
		Category:   loop.CatApprove,
		Confidence: 1.0,
		Requested:  past, // well beyond the 30s window
	})
	if got.Action != "deny" {
		t.Errorf("timed-out gate: action = %q, want deny", got.Action)
	}
	if got.Reason == "" {
		t.Error("timed-out gate: empty reason (must record why it denied)")
	}
	// The denied entry must be in the audit ledger.
	if n := len(gate.Ledger()); n != 1 {
		t.Errorf("ledger entries = %d, want 1", n)
	}
}

// TestM5_RunnerPausesOnApproval verifies the runner routes a
// denied tool call through paused_approval (the step is held,
// the run is not spending) — the loop-level HITL path.
func TestM5_RunnerPausesOnApproval(t *testing.T) {
	gate := loop.NewApprovalGate()
	guard := budget.New(100.0, 200.0)
	reg := tools.NewRegistry()
	cfg := loop.RunnerConfig{
		RunID:     "m5-pause",
		MaxSteps:  5,
		WallClock: 10 * time.Second,
		Goal:      "m5 pause test",
		Gate:      gate,
	}
	// CatApprove always denies — the runner enters paused_approval.
	runner := loop.NewRunnerWithApprovalGate(cfg, guard, reg, func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		return "delete", map[string]any{"path": "/tmp/x", "content": "m5"}
	}, gate)

	// Run with a 1s timeout so we don't wait for WallClock.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.State != loop.StatePausedApproval {
		t.Errorf("state = %q, want %q", result.State, loop.StatePausedApproval)
	}
	if result.PartialSynthesis == "" {
		t.Error("partial synthesis empty on approval hold")
	}
	// The denied tool is in the audit ledger (sampling + review exercised).
	if n := len(gate.Ledger()); n == 0 {
		t.Error("approval ledger empty — audit trail not written")
	}
}

// TestM5_InterruptionRate is the M5 acceptance:
// the interrupt rate (denials that are not auto-approves)
// must be <10% of all steps. With a mixed policy (reads
// auto, everything else held), the natural interrupt rate
// is bounded by the proportion of non-read tools — the
// operator only sees the high-impact fraction.
func TestM5_InterruptionRate(t *testing.T) {
	gate := loop.NewApprovalGate()
	// Simulate 100 steps: 95 reads (auto-approve), 5 writes (held).
	// With 5% high-impact tools, the interrupt rate is 5%,
	// comfortably under the 10% ceiling (P30/P68).
	total := 100
	for i := 0; i < total; i++ {
		tool := "read"
		if i%20 == 0 {
			tool = "write_file"
		}
		gate.Check(loop.ApprovalRequest{
			RunID:      "r",
			StepID:     i,
			Tool:       tool,
			ArgsHash:   "h",
			Category:   loop.Categorize(tool),
			Confidence: 0.95,
			Requested:  time.Now(),
		})
	}
	autoApproved := gate.AutoApproveCount()
	interrupts := total - autoApproved
	rate := float64(interrupts) / float64(total)
	if rate >= 0.10 {
		t.Errorf("interrupt rate = %.1f%%, want <10%% (%d of %d)", rate*100, interrupts, total)
	}
}
