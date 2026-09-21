package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/onegw"
	"github.com/FreePeak/agentloop/internal/tools"
)

// fakeModel records what it was asked and returns a canned reply.
type fakeModel struct {
	gotMsgs []onegw.Message
	reply   onegw.Reply
	err     error
	calls   int
}

func (f *fakeModel) Chat(_ context.Context, msgs ...onegw.Message) (onegw.Reply, error) {
	f.calls++
	f.gotMsgs = msgs
	return f.reply, f.err
}

func modelRunner(t *testing.T, m ModelClient, maxSteps int) *LoopRunner {
	t.Helper()
	guard := budget.New(100.0, 200.0)
	cfg := RunnerConfig{
		RunID:     "m8-test",
		MaxSteps:  maxSteps,
		WallClock: 10 * time.Second,
		Goal:      "summarise the repo",
		Model:     m,
	}
	return NewRunnerWithToolFn(cfg, guard, tools.NewRegistry(),
		func(step int, _ RunnerConfig) (string, map[string]any) {
			return "query", map[string]any{"step": step}
		})
}

// With a model configured, a bound exit's synthesis comes from the model,
// carries the answering leg, and accumulates the reported token usage.
func TestM8_SynthesisUsesModelAndRecordsUsage(t *testing.T) {
	fm := &fakeModel{reply: onegw.Reply{
		Content: "  the repo is a Go policy plane  ",
		Model:   "deepseek-v4.1-flash",
		Usage:   onegw.Usage{Prompt: 40, Completion: 9, Total: 49},
	}}
	result, err := modelRunner(t, fm, 2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if fm.calls != 1 {
		t.Fatalf("model calls = %d, want exactly 1 per run exit", fm.calls)
	}
	if result.PartialSynthesis != "the repo is a Go policy plane" {
		t.Errorf("PartialSynthesis = %q, want the model's trimmed answer", result.PartialSynthesis)
	}
	if result.AnsweredBy != "deepseek-v4.1-flash" {
		t.Errorf("AnsweredBy = %q, want the answering leg", result.AnsweredBy)
	}
	if result.Usage.Total != 49 || result.Usage.ModelCalls != 1 {
		t.Errorf("Usage = %+v, want total 49 across 1 call", result.Usage)
	}
	// The goal must reach the model — not the run id.
	last := fm.gotMsgs[len(fm.gotMsgs)-1]
	if !strings.Contains(last.Content, "summarise the repo") {
		t.Errorf("prompt missing the goal:\n%s", last.Content)
	}
	if strings.Contains(last.Content, "m8-test") {
		t.Errorf("prompt leaked the run id as the goal:\n%s", last.Content)
	}
}

// A failing model must not lose the run: the deterministic partial is
// returned, the reason is recorded, and Run() still succeeds.
func TestM8_ModelFailureFallsBackToDeterministicPartial(t *testing.T) {
	fm := &fakeModel{err: fmt.Errorf("onegw: 502: upstream unavailable")}
	result, err := modelRunner(t, fm, 2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want the run to survive a model failure", err)
	}

	if result.PartialSynthesis == "" {
		t.Fatal("PartialSynthesis empty after model failure — the run lost its steps")
	}
	if !strings.Contains(result.PartialSynthesis, "Synthesis after") {
		t.Errorf("PartialSynthesis = %q, want the deterministic fallback", result.PartialSynthesis)
	}
	if !strings.Contains(result.SynthesisError, "upstream unavailable") {
		t.Errorf("SynthesisError = %q, want the failure recorded", result.SynthesisError)
	}
}

// With no model configured (every unit test, and any deploy without onegw
// wired) behaviour is exactly as before: deterministic, no network.
func TestM8_NoModelKeepsDeterministicBehaviour(t *testing.T) {
	result, err := modelRunner(t, nil, 2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !strings.Contains(result.PartialSynthesis, "Synthesis after") {
		t.Errorf("PartialSynthesis = %q, want the deterministic string", result.PartialSynthesis)
	}
	if result.Usage.ModelCalls != 0 || result.AnsweredBy != "" {
		t.Errorf("Usage/AnsweredBy set with no model: %+v / %q", result.Usage, result.AnsweredBy)
	}
}
