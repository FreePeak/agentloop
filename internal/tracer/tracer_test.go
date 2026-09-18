// Package tracer_test verifies nested span tracing (M2).
// Spans nest via ParentID to form trees; the tracer must
// preserve hierarchy and thread-safety.
package tracer_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/FreePeak/agentloop/internal/tracer"
)

// TestTracer_NestedSpans verifies that a child span
// is linked to its parent and appears in the parent's Children.
func TestTracer_NestedSpans(t *testing.T) {
	tr := tracer.New()
	parent := tr.StartSpan("run-1", "s1", "", tracer.SpanThink, "plan", 1)
	child := tr.StartSpan("run-1", "s2", "s1", tracer.SpanAct, "exec", 1)
	tr.EndSpan("s2", map[string]any{"ok": true}, nil, 0.001)
	tr.EndSpan("s1", map[string]any{"ok": true}, nil, 0.002)

	if child.ParentID != "s1" {
		t.Errorf("child.ParentID = %q, want %q", child.ParentID, "s1")
	}
	if len(parent.Children) != 1 || parent.Children[0] != "s2" {
		t.Errorf("parent.Children = %v, want [s2]", parent.Children)
	}
	// Child end must populate latency and output.
	if child.LatencyMs < 0 {
		t.Errorf("child.LatencyMs = %d, want >=0", child.LatencyMs)
	}
	if child.Output == nil {
		t.Error("child.Output is nil, want set value")
	}
	// Parent end must populate latency and cost.
	if parent.LatencyMs < 0 {
		t.Errorf("parent.LatencyMs = %d, want >=0", parent.LatencyMs)
	}
	if parent.CostUSD != 0.002 {
		t.Errorf("parent.CostUSD = %v, want 0.002", parent.CostUSD)
	}
}

// TestTracer_RunSpans returns all spans for a run in ingestion order.
func TestTracer_RunSpans(t *testing.T) {
	tr := tracer.New()
	tr.StartSpan("run-A", "a1", "", tracer.SpanSystem, "run", 0)
	tr.StartSpan("run-A", "a2", "a1", tracer.SpanThink, "think", 1)
	tr.StartSpan("run-A", "a3", "a1", tracer.SpanAct, "exec", 1)
	tr.EndSpan("a2", nil, nil, 0)
	tr.EndSpan("a3", nil, nil, 0)
	tr.EndSpan("a1", nil, nil, 0)

	spans := tr.RunSpans("run-A")
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	if spans[0].SpanID != "a1" {
		t.Errorf("first span = %q, want a1", spans[0].SpanID)
	}
}

// TestTracer_DifferentRuns keeps traces separate.
func TestTracer_DifferentRuns(t *testing.T) {
	tr := tracer.New()
	tr.StartSpan("run-1", "x1", "", tracer.SpanSystem, "run", 0)
	tr.StartSpan("run-2", "y1", "", tracer.SpanSystem, "run", 0)
	tr.EndSpan("x1", nil, nil, 0)
	tr.EndSpan("y1", nil, nil, 0)

	if got := len(tr.RunSpans("run-1")); got != 1 {
		t.Errorf("run-1 spans = %d, want 1", got)
	}
	if got := len(tr.RunSpans("run-2")); got != 1 {
		t.Errorf("run-2 spans = %d, want 1", got)
	}
}

// TestTracer_ThreadSafety runs concurrent span starts/ends.
func TestTracer_ThreadSafety(t *testing.T) {
	tr := tracer.New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spanID := fmt.Sprintf("s-%d", i)
			s := tr.StartSpan("run-multi", spanID, "", tracer.SpanAct, "test", i)
			tr.EndSpan(s.SpanID, map[string]any{"i": i}, nil, float64(i)*0.001)
		}(i)
	}
	wg.Wait()
	// If it doesn't panic and all spans are present, thread safety holds.
	spans := tr.RunSpans("run-multi")
	if len(spans) != 50 {
		t.Errorf("got %d spans, want 50", len(spans))
	}
}
