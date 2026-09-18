// Package replay reconstructs a run from its trace (Tracer output).
// Used for M2 validation: given a trace, replay it and detect
// anomalies (duplicate writes, budget overruns, cycle patterns).
package replay

import (
	"fmt"
	"strings"

	"github.com/FreePeak/agentloop/internal/tracer"
)

// ReplayResult is the outcome of replaying a trace.
type ReplayResult struct {
	RunID      string
	SpanCount  int
	Kinds      map[string]int // kind → count
	Errors     []string
	Warnings   []string
	CycleDetected bool
	DedupHits  int
}

// Replay reconstructs a run summary from traced spans.
// It does NOT re-execute — it analyzes the trace structure.
func Replay(t *tracer.Tracer, runID string) ReplayResult {
	spans := t.RunSpans(runID)
	r := ReplayResult{
		RunID:   runID,
		SpanCount: len(spans),
		Kinds:   make(map[string]int),
	}

	seen := make(map[string]int) // tool name → count (dedup check)

	for _, s := range spans {
		kind := string(s.Kind)
		r.Kinds[kind]++

		if s.Error != "" {
			r.Errors = append(r.Errors, fmt.Sprintf("span %s: %s", s.SpanID, s.Error))
		}

		// Detect duplicate writes: same tool appears more than once in executor spans.
		if s.Kind == tracer.SpanAct || s.Kind == tracer.SpanExecutor {
			tool := s.Label
			seen[tool]++
			if seen[tool] > 1 {
				r.DedupHits += 1
			}
		}
	}

	// Detect cycle: more than 2 of any single executor span kind.
	for tool, count := range seen {
		if count > 2 {
			r.CycleDetected = true
			r.Warnings = append(r.Warnings, fmt.Sprintf("cycle detected: tool %q appears %d times", tool, count))
		}
	}

	return r
}

// Diff compares two replay results and returns a human-readable summary.
// Used for M2 injected-fault detection: compare traces before/after a fault.
func (r ReplayResult) Diff(other ReplayResult) string {
	var diff []string
	if r.SpanCount != other.SpanCount {
		diff = append(diff, fmt.Sprintf("span count: %d vs %d", r.SpanCount, other.SpanCount))
	}
	for k, v := range r.Kinds {
		if other.Kinds[k] != v {
			diff = append(diff, fmt.Sprintf("kind %s: %d vs %d", k, v, other.Kinds[k]))
		}
	}
	if len(r.Errors) != len(other.Errors) {
		diff = append(diff, fmt.Sprintf("error count: %d vs %d", len(r.Errors), len(other.Errors)))
	}
	if r.CycleDetected != other.CycleDetected {
		diff = append(diff, fmt.Sprintf("cycle detected: %v vs %v", r.CycleDetected, other.CycleDetected))
	}
	if r.DedupHits != other.DedupHits {
		diff = append(diff, fmt.Sprintf("dedup hits: %d vs %d", r.DedupHits, other.DedupHits))
	}
	if len(diff) == 0 {
		return "no diff"
	}
	return strings.Join(diff, "; ")
}

// Validate checks a replay result against M2 rules.
// Returns error if any rule is violated.
func (r ReplayResult) Validate() error {
	if r.CycleDetected {
		return fmt.Errorf("cycle detected in trace: %v", r.Warnings)
	}
	if r.DedupHits > 0 {
		return fmt.Errorf("duplicate writes detected: %d dedup hits", r.DedupHits)
	}
	return nil
}
