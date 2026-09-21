// Package replay_test verifies trace replay (M2).
// Replay analyzes a trace for cycles, dedup, and anomalies.
package replay_test

import (
	"fmt"
	"testing"

	"github.com/FreePeak/agentloop/internal/replay"
	"github.com/FreePeak/agentloop/internal/tracer"
)

func newTracedRun() *tracer.Tracer {
	tr := tracer.New()
	tr.StartSpan("run-1", "run-1", "", tracer.SpanSystem, "run", 0)
	tr.StartSpan("run-1", "s1", "run-1", tracer.SpanThink, "think", 1)
	tr.EndSpan("s1", map[string]any{"plan": "step1"}, nil, 0.001)
	tr.StartSpan("run-1", "s2", "run-1", tracer.SpanAct, "query", 1)
	tr.EndSpan("s2", map[string]any{"result": "data"}, nil, 0.001)
	tr.StartSpan("run-1", "s3", "run-1", tracer.SpanAct, "run_tests", 2)
	tr.EndSpan("s3", map[string]any{"result": "ok"}, nil, 0.001)
	tr.StartSpan("run-1", "s4", "run-1", tracer.SpanEvaluate, "evaluate", 3)
	tr.EndSpan("s4", map[string]any{"score": 0.9}, nil, 0.001)
	tr.EndSpan("run-1", map[string]any{"state": "success"}, nil, 0.005)
	return tr
}

// TestReplay_Structure checks span counts and kind distribution.
func TestReplay_Structure(t *testing.T) {
	tr := newTracedRun()
	r := replay.Replay(tr, "run-1")

	if r.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", r.RunID)
	}
	if r.SpanCount != 5 { // s1-s4 + run-1 root
		t.Errorf("SpanCount = %d, want 5", r.SpanCount)
	}
	if r.Kinds["system"] != 1 {
		t.Errorf("kind system = %d, want 1", r.Kinds["system"])
	}
	if r.Kinds["think"] != 1 {
		t.Errorf("kind think = %d, want 1", r.Kinds["think"])
	}
	if r.Kinds["act"] != 2 {
		t.Errorf("kind act = %d, want 2", r.Kinds["act"])
	}
	if r.Kinds["evaluate"] != 1 {
		t.Errorf("kind evaluate = %d, want 1", r.Kinds["evaluate"])
	}
}

// TestReplay_ValidRun passes validation on a clean trace.
func TestReplay_ValidRun(t *testing.T) {
	tr := newTracedRun()
	r := replay.Replay(tr, "run-1")
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// TestReplay_CycleDetected flags a cycle when a tool
// appears more than 2 times.
func TestReplay_CycleDetected(t *testing.T) {
	tr := tracer.New()
	tr.StartSpan("run-cycle", "run-cycle", "", tracer.SpanSystem, "run", 0)
	for i := 0; i < 3; i++ {
		sid := tr.StartSpan("run-cycle", fmt.Sprintf("cycle-%d", i), "run-cycle", tracer.SpanAct, "write_file", i+1)
		tr.EndSpan(sid.SpanID, map[string]any{"written": true}, nil, 0.001)
	}
	tr.EndSpan("run-cycle", nil, nil, 0)

	r := replay.Replay(tr, "run-cycle")
	if !r.CycleDetected {
		t.Error("CycleDetected = false, want true")
	}
	if err := r.Validate(); err == nil {
		t.Error("Validate() = nil, want error")
	}
}

// TestReplay_Diff compares two replay results.
func TestReplay_Diff(t *testing.T) {
	tr := newTracedRun()
	r1 := replay.Replay(tr, "run-1")

	// Create a modified trace with extra spans.
	tr2 := tracer.New()
	tr2.StartSpan("run-1", "run-1", "", tracer.SpanSystem, "run", 0)
	tr2.StartSpan("run-1", "s1", "run-1", tracer.SpanThink, "think", 1)
	tr2.EndSpan("s1", nil, nil, 0.001)
	tr2.StartSpan("run-1", "s2", "run-1", tracer.SpanAct, "query", 1)
	tr2.EndSpan("s2", nil, nil, 0.001)
	tr2.StartSpan("run-1", "s3", "run-1", tracer.SpanAct, "run_tests", 2)
	tr2.EndSpan("s3", nil, nil, 0.001)
	tr2.StartSpan("run-1", "s4", "run-1", tracer.SpanAct, "web_search", 3) // extra act
	tr2.EndSpan("s4", nil, nil, 0.001)
	tr2.StartSpan("run-1", "s5", "run-1", tracer.SpanEvaluate, "evaluate", 4) // extra evaluate
	tr2.EndSpan("s5", nil, nil, 0.001)
	tr2.EndSpan("run-1", nil, nil, 0)
	r2 := replay.Replay(tr2, "run-1")

	diff := r1.Diff(r2)
	if diff == "no diff" {
		t.Error("Diff = no diff, want differences")
	}
	// Inverted (same result) should return "no diff".
	if r1.Diff(r1) != "no diff" {
		t.Error("Diff of identical results != no diff")
	}
}
