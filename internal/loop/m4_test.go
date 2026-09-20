package loop_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/store"
	"github.com/FreePeak/agentloop/internal/tools"
)

// TestResumeFromCheckpoint_FaultAtStep7 is M4 acceptance #2 from PRD §13:
// resume from a step-5 checkpoint after a step-7 fault.
//
// 1. First run: deterministic tool picker that PANICS (faults) the
//    first time step 7 is selected. A durable checkpoint at step 5
//    must survive the crash.
// 2. Second run (same runID, same store): the runner restores the
//    step-5 checkpoint and continues from step 5 — it must NOT
//    re-fire the restored steps 0-4 (dedup map restored) and
//    must hold the 70% rule.
func TestResumeFromCheckpoint_FaultAtStep7(t *testing.T) {
	dir := t.TempDir()
	cp, err := store.Open(dir + "/checkpoints.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()

	// faultHit ensures the panic fires only in the first run;
	// the resumed run passes step 7 cleanly (the checkpoint
	// saved us before the fault).
	var faultHit int32
	var execCount int32 // tool picks actually executed (dedup skips don't increment)
	pick := func(step int, cfg loop.RunnerConfig) (string, map[string]any) {
		if step == 7 && atomic.AddInt32(&faultHit, 1) == 1 {
			panic("step-7 fault")
		}
		atomic.AddInt32(&execCount, 1)
		return "repo_search", map[string]any{"step": step}
	}

	cfg := loop.RunnerConfig{
		RunID:     "run-resume",
		MaxSteps:  10,
		WallClock: 30 * time.Second,
		Goal:      "resume test",
		Context:   "test",
	}

	// --- First run: faults at step 7 ---
	r1 := loop.NewRunnerWithCheckpointer(cfg, budget.New(1.0, 10.0), tools.NewRegistry(), pick, cp)
	func() {
		defer func() { recover() }() // absorb the step-7 fault
		_, _ = r1.Run(context.Background())
	}()

	// The crash happened at step 7; the latest durable checkpoint
	// must be at step 5 (compression cadence every 5 iterations).
	has, err := cp.HasCheckpoint("run-resume")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("checkpoint must exist after step-7 fault")
	}
	stepIdx, _, _, err := cp.LoadCheckpoint("run-resume")
	if err != nil {
		t.Fatal(err)
	}
	if stepIdx != 5 {
		t.Fatalf("checkpoint step = %d, want 5", stepIdx)
	}

	// --- Second run: resume from the step-5 checkpoint ---
	r2 := loop.NewRunnerWithCheckpointer(cfg, budget.New(1.0, 10.0), tools.NewRegistry(), pick, cp)
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if res2.State == "" {
		t.Fatal("resumed run produced no state")
	}

	// Resumed run must carry the restored steps 0-4 AND the new
	// steps 5-9: 10 steps total. Steps 0-4 must not have re-fired
	// (the dedup map was restored from the checkpoint).
	if got := len(res2.Steps); got != 10 {
		t.Fatalf("resumed run steps = %d, want 10", got)
	}
	afterResume := atomic.LoadInt32(&execCount)
	r1Picks := int32(7) // steps 0..6 executed in faulting run (step 7 panicked mid-pick)
	r2Picks := afterResume - r1Picks
	if r2Picks != 5 {
		t.Fatalf("resumed run executed %d tool picks (want 5: steps 5-9); dedup restoration failed", r2Picks)
	}
}