package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FreePeak/agentloop/internal/onegw"
	"github.com/FreePeak/agentloop/internal/tools"
)

// Reasoner is the piece that makes this a loop rather than a rotation: one
// model call per step, given what the run knows *now*, returning the next
// tool and its arguments.
//
// It is the only mid-loop model call. Bound exits still synthesise
// separately (model.go); this one decides the action.
//
// Cost: one call per step, which is exactly what BudgetGuard's pre-action
// check governs — a reasoner call is metered like any other step.
func (r *LoopRunner) decide(ctx context.Context, step int, result *RunResult) (name string, args map[string]any, why string, err error) {
	choice, err := r.choose(ctx, step, result)
	if err != nil {
		return "", nil, "", err
	}
	if choice.Done {
		// An empty name is the caller's signal that the goal predicate
		// fired; the rationale rides back so the record can carry it.
		return "", nil, choice.Why, nil
	}
	return choice.Tool, choice.Args, choice.Why, nil
}

// choose runs the reasoner and returns its decision.
func (r *LoopRunner) choose(ctx context.Context, step int, result *RunResult) (StepChoice, error) {
	if r.model == nil {
		return StepChoice{}, fmt.Errorf("no model client")
	}
	reg := r.toolRegistry
	if reg == nil {
		return StepChoice{}, fmt.Errorf("no tool registry")
	}

	reply, err := r.model.Chat(ctx,
		onegw.Message{Role: "system", Content: reasonSystemPrompt(reg.List())},
		onegw.Message{Role: "user", Content: buildReasonPrompt(r, step, result)},
	)
	if err != nil {
		return StepChoice{}, err
	}
	usageAdd(result, reply.Usage)

	choice, err := parseDecision(reply.Content)
	if err != nil {
		return StepChoice{}, err
	}
	if err := r.checkDecision(reg, choice); err != nil {
		return StepChoice{}, err
	}
	return choice, nil
}

// usageAdd accumulates the reasoner's tokens on the run's result, so the
// step's cost reflects the call that chose it and `usage` in the run
// record stays the whole story (synthesis adds to the same total).
func usageAdd(result *RunResult, u onegw.Usage) {
	result.Usage.Add(u)
}

// StepChoice is the reasoner's answer: the next action.
//
// `Why` is not decoration. It is written into the step record, and it is
// what an operator reads when a run does something surprising — without
// it a trajectory shows tools and no reasons.
//
// Named StepChoice, not Decision: `Decision` is already the approval
// gate's type in this package, and a reasoner decision is a different
// thing from a gate decision.
type StepChoice struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
	Why  string         `json:"why,omitempty"`
	Done bool           `json:"done,omitempty"`
}

// parseDecision reads the model's reply as a decision, tolerating the
// ways a model actually returns JSON: bare, fenced, or wrapped in a
// sentence. A reply that cannot be read is an error the caller treats as
// an observation — the loop falls back to its rotation rather than dying.
func parseDecision(content string) (StepChoice, error) {
	raw := extractJSONObject(content)
	if raw == "" {
		return StepChoice{}, fmt.Errorf("reasoner: no JSON object in reply")
	}
	var d StepChoice
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return StepChoice{}, fmt.Errorf("reasoner: decode decision: %w", err)
	}
	return d, nil
}

// extractJSONObject returns the first balanced {...} in s, ignoring
// braces inside strings. A model that answers "Here is the plan: {...}"
// is answering correctly; a naive first-to-last slice would break on the
// braces in the prose around it.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// nothing: braces inside a string are text
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// checkDecision refuses a decision the loop cannot act on, before the
// gate or the tool sees it: an unknown tool would otherwise be a
// fail-closed hold on a step nobody can approve.
func (r *LoopRunner) checkDecision(reg tools.ToolRegistry, d StepChoice) error {
	if d.Done {
		return nil
	}
	if d.Tool == "" {
		return fmt.Errorf("reasoner: no tool named")
	}
	for _, t := range reg.List() {
		if t.Name == d.Tool {
			return nil
		}
	}
	names := make([]string, 0, len(reg.List()))
	for _, t := range reg.List() {
		names = append(names, t.Name)
	}
	return fmt.Errorf("reasoner: unknown tool %q (available: %s)", d.Tool, strings.Join(names, ", "))
}

// reasonSystemPrompt is the contract the model is held to. It carries the
// registry's own descriptions — including each tool's DO NOT USE WHEN,
// which the PRD calls the highest-ROI prompt hour (P24) — because a model
// choosing from invented tool names is the failure this prevents.
func reasonSystemPrompt(ts []tools.Tool) string {
	var b strings.Builder
	b.WriteString("You are the decision step of a bounded agent loop. Each turn you choose ONE tool call.\n\n")
	b.WriteString("Answer with a single JSON object and nothing else:\n")
	b.WriteString(`{"tool":"<name>","args":{...},"why":"<one sentence>","done":false}` + "\n\n")
	b.WriteString("Set \"done\": true when the goal is established and no further tool call is needed.\n\n")
	b.WriteString("Available tools:\n")
	for _, t := range ts {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		if t.UseWhen != "" {
			fmt.Fprintf(&b, "    USE WHEN: %s\n", t.UseWhen)
		}
		if t.DoNotUseWhen != "" {
			fmt.Fprintf(&b, "    DO NOT USE WHEN: %s\n", t.DoNotUseWhen)
		}
		if len(t.Schema) > 0 {
			fmt.Fprintf(&b, "    ARGS SCHEMA: %s\n", compactSchema(t.Schema))
		}
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- A tool name that is not in the list is an error. Never invent one.\n")
	b.WriteString("- Every argument a schema marks required must be present.\n")
	b.WriteString("- Use only what the previous results established. If they establish nothing, " +
		"choose the call that would establish it.\n")
	return b.String()
}

func compactSchema(raw json.RawMessage) string {
	s := string(raw)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// buildReasonPrompt renders what the run knows *now*: the goal, where it
// is, the plan step if there is one, and the last result verbatim.
//
// The last result is the whole point. A rotation cannot use it; a
// reasoner that is not shown it is a rotation with extra steps.
func buildReasonPrompt(r *LoopRunner, step int, result *RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n", r.cfg.Goal)
	if r.cfg.Context != "" {
		fmt.Fprintf(&b, "Context: %s\n", r.cfg.Context)
	}
	fmt.Fprintf(&b, "Step: %d of %d\n", step, r.cfg.MaxSteps)
	fmt.Fprintf(&b, "Spend so far: $%.4f of $%.2f\n", result.SpendUSD, r.cfg.CostBudget)

	if r.plan != nil && step < len(r.plan.Steps) {
		ps := r.plan.Steps[step]
		fmt.Fprintf(&b, "Planned phase: %s\n", ps.Phase)
		fmt.Fprintf(&b, "Planned instruction: %s\n", ps.Instruction)
		fmt.Fprintf(&b, "Planned success criteria: %s\n", ps.Success)
	}

	if len(result.Steps) == 0 {
		b.WriteString("\nNo steps have run yet. Choose the first call: establish something, " +
			"do not guess.\n")
		return b.String()
	}

	b.WriteString("\nSteps so far (most recent last):\n")
	// The last few, not all of them: a bounded loop's context is the recent
	// trajectory, and a run that has taken 40 steps does not need all 40
	// re-read to choose the 41st.
	from := 0
	if len(result.Steps) > 5 {
		from = len(result.Steps) - 5
	}
	if from > 0 {
		fmt.Fprintf(&b, "(showing the last %d of %d)\n", len(result.Steps)-from, len(result.Steps))
	}
	for _, s := range result.Steps[from:] {
		fmt.Fprintf(&b, "- step %d: %s(%s) -> %s\n", s.StepID, s.Tool, compactArgs(s), truncate(renderResult(s.Result), 600))
	}
	b.WriteString("\nChoose the next call that makes progress on the goal using what these results " +
		"established. If the goal is already established, set done.\n")
	return b.String()
}

// compactArgs renders a step's arguments as JSON, so the model sees the
// same shape it is being asked to produce.
func compactArgs(s StepRecord) string {
	if len(s.Args) == 0 {
		return ""
	}
	b, err := json.Marshal(s.Args)
	if err != nil {
		return fmt.Sprintf("%v", s.Args)
	}
	return truncate(string(b), 200)
}

// renderResult turns a step's stored result into text a model can read.
// A map renders as JSON rather than Go's map printer: a model given
// `map[output:... ran:true]` has to guess, and guesses wrong about types.
func renderResult(v any) string {
	if v == nil {
		return "(no result)"
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
