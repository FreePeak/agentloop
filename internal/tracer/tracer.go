// Package tracer provides nested span tracing for agentloop runs.
// Every step is a span with parent-child relationships, enabling
// diff-based fault detection (M2) and replay (M2).
package tracer

import (
	"fmt"
	"sync"
	"time"
)

// SpanKind classifies what produced a span.
type SpanKind string

const (
	SpanRouter   SpanKind = "router"    // tier/model selection
	SpanPlanner  SpanKind = "planner"   // plan generation
	SpanExecutor SpanKind = "executor"  // tool execution
	SpanEvaluate SpanKind = "evaluate"  // eval scoring
	SpanThink    SpanKind = "think"     // reasoning phase
	SpanAct      SpanKind = "act"       // action phase
	SpanSystem   SpanKind = "system"    // system event (budget, kill, exit)
)

// Span is one unit of trace. Spans nest via ParentID to form a tree.
type Span struct {
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_id,omitempty"`
	RunID      string         `json:"run_id"`
	Kind       SpanKind       `json:"kind"`
	Label      string         `json:"label"`
	Step       int            `json:"step,omitempty"`
	Input      interface{}    `json:"input,omitempty"`
	Output     interface{}    `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	LatencyMs  int64          `json:"latency_ms,omitempty"`
	CostUSD    float64        `json:"cost_usd,omitempty"`
	StartMs    int64          `json:"start_ms,omitempty"`
	EndMs      int64          `json:"end_ms,omitempty"`
	Children   []string       `json:"children,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// Tracer accumulates spans for a run, keyed by RunID. Thread-safe.
type Tracer struct {
	mu    sync.Mutex
	spans map[string]*Span
	roots []string
	runs  map[string][]string
}

// New returns a ready Tracer.
func New() *Tracer {
	return &Tracer{
		spans: make(map[string]*Span),
		runs:  make(map[string][]string),
	}
}

// StartSpan begins a span. Returns the span for the caller to fill and End.
func (t *Tracer) StartSpan(runID, spanID, parentID string, kind SpanKind, label string, step int) *Span {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := &Span{
		SpanID:   spanID,
		ParentID: parentID,
		RunID:    runID,
		Kind:     kind,
		Label:    label,
		Step:     step,
		Metadata: make(map[string]any),
		StartMs:  time.Now().UnixMilli(),
	}

	t.spans[spanID] = s

	if parentID != "" {
		if parent, ok := t.spans[parentID]; ok {
			parent.Children = append(parent.Children, spanID)
		}
	} else {
		t.roots = append(t.roots, spanID)
	}

	t.runs[runID] = append(t.runs[runID], spanID)
	return s
}

// EndSpan closes a span (sets end time, output/error).
func (t *Tracer) EndSpan(spanID string, output interface{}, err error, costUSD float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.spans[spanID]
	if !ok {
		return
	}
	s.EndMs = time.Now().UnixMilli()
	s.LatencyMs = s.EndMs - s.StartMs
	s.Output = output
	if err != nil {
		s.Error = err.Error()
	}
	s.CostUSD = costUSD
}

// RunSpans returns all spans for a run in ingestion order.
func (t *Tracer) RunSpans(runID string) []Span {
	t.mu.Lock()
	defer t.mu.Unlock()

	ids := t.runs[runID]
	out := make([]Span, 0, len(ids))
	for _, id := range ids {
		if s, ok := t.spans[id]; ok {
			cp := *s
			out = append(out, cp)
		}
	}
	return out
}

// Spans returns all spans (for testing/diff).
func (t *Tracer) Spans() map[string]Span {
	t.mu.Lock()
	defer t.mu.Unlock()

	cp := make(map[string]Span, len(t.spans))
	for k, v := range t.spans {
		cp[k] = *v
	}
	return cp
}

// String returns a human-readable summary of a span.
func (s Span) String() string {
	return fmt.Sprintf("span %s [%s] step=%d kind=%s label=%q dur=%dms",
		s.SpanID, s.RunID, s.Step, s.Kind, s.Label, s.LatencyMs)
}
