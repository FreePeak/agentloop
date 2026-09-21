package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/FreePeak/agentloop/internal/onegw"
)

// ModelClient is the outbound model surface the loop needs — one call,
// given conversation messages and the tier that should serve it.
// internal/onegw.Client satisfies it; tests use fakes. Kept deliberately
// this small: the loop must not grow a gateway's job (retries, key pools,
// fallback), only consume one.
//
// The tier parameter is not decoration. Until it existed, the loop
// computed a tier per step (M3) and never sent it, so "tiered routing"
// was a field on the run record and nothing else.
type ModelClient interface {
	ChatTier(ctx context.Context, tier string, msgs ...onegw.Message) (onegw.Reply, error)
}

// Tierless adapts a client that only knows Chat, so a fake or an older
// implementation still satisfies ModelClient. Tests use it; production
// passes the real client, whose ChatTier routes.
type Tierless struct {
	C interface {
		Chat(ctx context.Context, msgs ...onegw.Message) (onegw.Reply, error)
	}
}

// ChatTier ignores the tier: this adapter exists precisely for clients
// that cannot route.
func (t Tierless) ChatTier(ctx context.Context, _ string, msgs ...onegw.Message) (onegw.Reply, error) {
	return t.C.Chat(ctx, msgs...)
}

// synthesize replaces a bound-exit's deterministic partial with a real
// model answer when a model client is configured.
//
// The deterministic string stays the fallback: with no client (every unit
// test, and any deploy without onegw wired) behaviour is unchanged, so the
// containment suite keeps passing without a live gateway.
//
// Cost note: when the budget ceiling fires, this call is exactly what the
// 10% pre-synthesis reserve exists to fund (PRD §17, budget.PreSynthReserve).
func (r *LoopRunner) synthesize(ctx context.Context, result *RunResult) error {
	if r.model == nil {
		result.PartialSynthesis = synthesizePartial(result.Steps)
		return nil
	}

	prompt := buildSynthesisPrompt(r.cfg.Goal, *result)
	// Synthesis is its own step type: a run that hit a bound wants a
	// different model than the one driving the loop (PRD move 3).
	reply, err := r.model.ChatTier(ctx, TierSynthesis,
		onegw.Message{Role: "system", Content: synthesisSystemPrompt},
		onegw.Message{Role: "user", Content: prompt},
	)
	if err != nil {
		// A failed synthesis must not lose the run: fall back to the
		// deterministic partial and record why, so the exit is still
		// reported with its steps rather than as an error.
		result.PartialSynthesis = synthesizePartial(result.Steps)
		result.SynthesisError = err.Error()
		return err
	}

	result.PartialSynthesis = strings.TrimSpace(reply.Content)
	result.AnsweredBy = reply.Model
	result.Usage.Add(reply.Usage)
	return nil
}

const synthesisSystemPrompt = "You are the synthesis step of a bounded agent loop. " +
	"You are given the goal and the steps the loop executed. Answer the goal " +
	"from what the steps actually established, using only that. Be concise. " +
	"If the steps do not establish the answer, say what is missing rather than " +
	"inventing a result."

// buildSynthesisPrompt renders the run's own record as the user message.
// It carries what the loop actually knows — goal, state, exit reason, and
// the step/tool line for each step — and nothing speculative.
func buildSynthesisPrompt(goal string, result RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n", goal)
	if result.ExitReason != "" {
		fmt.Fprintf(&b, "Ended: state=%s exit_reason=%s\n", result.State, result.ExitReason)
	}
	fmt.Fprintf(&b, "Steps executed: %d\n", len(result.Steps))
	for _, s := range result.Steps {
		fmt.Fprintf(&b, "- step %d: tool=%s", s.StepID, s.Tool)
		if s.Result != "" {
			fmt.Fprintf(&b, " result=%s", s.Result)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TokenUsage is the run's cumulative token accounting, summed from the
// usage onegw reports per call. Enforcement stays in internal/budget;
// this is the observation half.
type TokenUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
	ModelCalls int `json:"model_calls"`
}

// Add accumulates one reply's usage.
func (u *TokenUsage) Add(o onegw.Usage) {
	u.Prompt += o.Prompt
	u.Completion += o.Completion
	u.Total += o.Total
	u.ModelCalls++
}
