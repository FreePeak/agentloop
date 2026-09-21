package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/onegw"
	"github.com/FreePeak/agentloop/internal/planner"
	"github.com/FreePeak/agentloop/internal/tools"
)

// scriptedModel answers with a fixed sequence of replies, so a test can
// assert what the loop does with each — including the replies a model
// should never send.
type scriptedModel struct {
	replies []string
	errs    []error
	calls   int
	prompts []string
	tiers   []string
}

func (m *scriptedModel) ChatTier(_ context.Context, tier string, msgs ...onegw.Message) (onegw.Reply, error) {
	m.tiers = append(m.tiers, tier)
	i := m.calls
	m.calls++
	for _, msg := range msgs {
		if msg.Role == "user" {
			m.prompts = append(m.prompts, msg.Content)
		}
	}
	if i < len(m.errs) && m.errs[i] != nil {
		return onegw.Reply{}, m.errs[i]
	}
	if i >= len(m.replies) {
		// Past the script: say the goal is met, so a test that under-scripts
		// still terminates instead of spinning to max_steps.
		return onegw.Reply{Content: `{"done":true,"why":"script exhausted"}`, Model: "scripted"}, nil
	}
	return onegw.Reply{
		Content: m.replies[i],
		Model:   "scripted",
		Usage:   onegw.Usage{Prompt: 10, Completion: 5, Total: 15},
	}, nil
}

func reasonRunner(t *testing.T, m ModelClient, maxSteps int) *LoopRunner {
	t.Helper()
	cfg := RunnerConfig{
		RunID:      "reason-test",
		MaxSteps:   maxSteps,
		WallClock:  10 * time.Second,
		CostBudget: 100,
		Goal:       "fix the parser bug",
		Model:      m,
	}
	r := newRunner(cfg, budget.New(100, 200), tools.NewRegistry(), nil, nil, nil, planner.NewPlanner(), nil, nil)
	return r
}

// The whole point: the model's chosen tool and its arguments are what runs.
// A rotation cannot do this, and the previous loop could not either.
func TestReasonerChoosesToolAndArgs(t *testing.T) {
	// First call: read. Second: the goal is met.
	m := &scriptedModel{replies: []string{
		`{"tool":"query","args":{"query":"parseConfig"},"why":"find the symbol"}`,
		`{"done":true,"why":"nothing further to check"}`,
	}}
	r := reasonRunner(t, m, 5)

	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Steps) == 0 {
		t.Fatal("no steps recorded")
	}
	first := result.Steps[0]
	if first.Tool != "query" {
		t.Errorf("tool = %q, want the model's choice (query)", first.Tool)
	}
	if first.Args["query"] != "parseConfig" {
		t.Errorf("args = %v, want the model's arguments", first.Args)
	}
	if !strings.Contains(first.Why, "find the symbol") {
		t.Errorf("why = %q, want the model's rationale recorded", first.Why)
	}
	// And the loop stopped because the model said so, not because it ran out.
	if result.State != StateSuccess {
		t.Errorf("state = %q, want success (the model said done)", result.State)
	}
	if result.ExitReason != ExitGoalMet {
		t.Errorf("exit_reason = %q, want %q", result.ExitReason, ExitGoalMet)
	}
}

// The reason prompt must carry what the previous step established. A
// reasoner not shown its own results is a rotation with extra steps.
func TestReasonPromptCarriesTheLastResult(t *testing.T) {
	m := &scriptedModel{replies: []string{
		`{"tool":"query","args":{"query":"x"},"why":"first"}`,
		`{"tool":"run_tests","args":{},"why":"verify"}`,
		`{"done":true,"why":"done"}`,
	}}
	r := reasonRunner(t, m, 5)

	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(m.prompts) < 2 {
		t.Fatalf("prompts = %d, want one per decision", len(m.prompts))
	}
	// The second decision's prompt must show the first step's tool and result.
	second := m.prompts[1]
	if !strings.Contains(second, "step 0: query") {
		t.Errorf("second prompt does not carry the first step:\n%s", second)
	}
	// And the first must say nothing has run yet, rather than inventing history.
	if !strings.Contains(m.prompts[0], "No steps have run yet") {
		t.Errorf("first prompt should say the run is empty:\n%s", m.prompts[0])
	}
}

// A model that answers with prose around the JSON is answering correctly —
// models do this constantly, and a parser that requires a bare object turns
// a usable reply into a failed step.
func TestParseDecisionToleratesFencedAndProloguedJSON(t *testing.T) {
	cases := map[string]string{
		"bare":                       `{"tool":"query","args":{"query":"a"}}`,
		"fenced":                     "```json\n{\"tool\":\"query\",\"args\":{\"query\":\"a\"}}\n```",
		"prologued":                  "Here is my decision:\n{\"tool\":\"query\",\"args\":{\"query\":\"a\"}}\nThat should work.",
		"with brace inside a string": `{"tool":"write_file","args":{"content":"f(x) { return 1; }"}}`,
	}
	for name, in := range cases {
		got, err := parseDecision(in)
		if err != nil {
			t.Errorf("%s: parseDecision() error: %v", name, err)
			continue
		}
		if got.Tool == "" {
			t.Errorf("%s: tool empty", name)
		}
	}
}

// An unknown tool must be refused before the gate sees it: the gate would
// fail closed and hold a step nobody can approve.
func TestReasonerRefusesAnInventedTool(t *testing.T) {
	m := &scriptedModel{replies: []string{
		`{"tool":"repo_context","args":{},"why":"invented"}`,
		`{"done":true,"why":"done"}`,
	}}
	r := reasonRunner(t, m, 3)

	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// It fell back to the rotation for that step, and SAID SO.
	if len(result.ReasonErrors) == 0 {
		t.Fatal("an unactionable decision was not recorded — it fell back silently")
	}
	if !strings.Contains(result.ReasonErrors[0], "unknown tool") {
		t.Errorf("ReasonErrors = %v, want the unknown-tool reason", result.ReasonErrors)
	}
	if !strings.Contains(result.Steps[0].Why, "reasoner unavailable") {
		t.Errorf("step Why = %q, want the fallback recorded on the step", result.Steps[0].Why)
	}
}

// A model that is unreachable must not kill the run: the loop falls back
// to the deterministic rotation, and the trajectory says why.
func TestReasonerTransportFailureFallsBackAndRecords(t *testing.T) {
	m := &scriptedModel{
		errs:    []error{errors.New("gateway unreachable"), errors.New("gateway unreachable")},
		replies: []string{`{"done":true,"why":"done"}`},
	}
	r := reasonRunner(t, m, 3)

	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Steps) == 0 {
		t.Fatal("the run produced no steps — a dead model must not stop the loop")
	}
	if len(result.ReasonErrors) == 0 {
		t.Error("no ReasonErrors recorded for an unreachable model")
	}
	for _, want := range []string{"rotation", "no model client"} {
		_ = want
	}
	if !strings.Contains(result.Steps[0].Why, "reasoner unavailable") {
		t.Errorf("step Why = %q, want the fallback recorded", result.Steps[0].Why)
	}
}

// With no model client at all, behaviour must be byte-identical to before:
// the rotation, and the step says so. This is what keeps every existing
// test and every gateway-less deploy working.
func TestNoModelMeansRotationAndSaysSo(t *testing.T) {
	r := reasonRunner(t, nil, 3)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Steps) == 0 {
		t.Fatal("no steps")
	}
	if result.Steps[0].Tool != "query" {
		t.Errorf("first tool = %q, want the rotation's first (query)", result.Steps[0].Tool)
	}
	if !strings.Contains(result.Steps[0].Why, "rotation") {
		t.Errorf("step Why = %q, want the rotation recorded", result.Steps[0].Why)
	}
	if len(result.ReasonErrors) != 0 {
		t.Errorf("ReasonErrors = %v, want none when no model was configured", result.ReasonErrors)
	}
}

// The system prompt must carry the registry's own descriptions, including
// each tool's DO NOT USE WHEN — a model choosing from invented names is the
// failure this prevents (PRD move 4).
func TestReasonSystemPromptCarriesTheToolContract(t *testing.T) {
	reg := tools.NewRegistry()
	p := reasonSystemPrompt(reg.List())
	for _, want := range []string{"query", "run_tests", "write_file", "DO NOT USE WHEN", "ARGS SCHEMA", "Never invent one"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
}

// The reasoner's tokens are counted, like every other call the run makes.
func TestReasonerUsageIsCounted(t *testing.T) {
	m := &scriptedModel{replies: []string{
		`{"tool":"query","args":{"query":"x"},"why":"a"}`,
		`{"tool":"run_tests","args":{},"why":"b"}`,
		`{"done":true,"why":"c"}`,
	}}
	r := reasonRunner(t, m, 5)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Usage.ModelCalls < 3 {
		t.Errorf("model_calls = %d, want one per decision", result.Usage.ModelCalls)
	}
	if result.Usage.Total == 0 {
		t.Error("usage total is zero — reasoner tokens are not counted")
	}
}

// The run's steps must carry their arguments: a trace showing only an
// args *hash* cannot answer "what did it actually try?".
func TestStepRecordsCarryArgs(t *testing.T) {
	m := &scriptedModel{replies: []string{
		`{"tool":"query","args":{"query":"needle"},"why":"a"}`,
		`{"done":true,"why":"b"}`,
	}}
	r := reasonRunner(t, m, 3)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	b, err := json.Marshal(result.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "needle") {
		t.Errorf("step JSON does not carry its args: %s", b)
	}
}

// Resuming from a hold must not re-ask the model: the decision was already
// made and approved, and a second call can choose a *different* tool —
// which would run something the operator never saw, under an approval for
// something else. Cost is the smaller reason; this is the reason.
func TestResumeReusesTheApprovedDecisionWithoutRecall(t *testing.T) {
	m := &scriptedModel{replies: []string{
		// step 0: a read, auto-approved.
		`{"tool":"query","args":{"query":"parseConfig"},"why":"locate"}`,
		// step 1: a write — this holds for approval.
		`{"tool":"write_file","args":{"path":"a.go","content":"fixed"},"why":"apply the fix"}`,
		// Anything after this would be a re-ask.
		`{"tool":"run_tests","args":{},"why":"verify"}`,
		`{"done":true,"why":"done"}`,
	}}
	gate := NewApprovalGate()
	cfg := RunnerConfig{
		RunID:      "resume-recall",
		MaxSteps:   4,
		WallClock:  10 * time.Second,
		CostBudget: 100,
		Goal:       "fix the parser",
		Model:      m,
		Gate:       gate,
	}
	reg := tools.NewRegistry()
	r := newRunner(cfg, budget.New(100, 200), reg, nil, nil, nil, planner.NewPlanner(), nil, gate)

	// Run to the hold.
	first, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if first.State != StatePausedApproval {
		t.Fatalf("state = %q, want a hold on the write", first.State)
	}
	callsAtHold := m.calls

	// Approve and resume.
	if !gate.Approve("resume-recall", 1) {
		t.Fatal("Approve() returned false for the held step")
	}
	second, err := r.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	// The decision at the held step must be the SAME one that was approved.
	var held *StepRecord
	for i := range second.Steps {
		if second.Steps[i].StepID == 1 {
			held = &second.Steps[i]
		}
	}
	if held == nil {
		t.Fatal("the held step is missing from the resumed run")
	}
	if held.Tool != "write_file" {
		t.Errorf("resumed step tool = %q, want the approved one (write_file)", held.Tool)
	}
	if held.Args["content"] != "fixed" {
		t.Errorf("resumed step args = %v, want the approved arguments", held.Args)
	}
	// And it was REPLAYED, not re-asked: the rationale says so. Without
	// this the tool could coincidentally match while the model was asked
	// again (and could have answered differently with a live gateway).
	if !strings.Contains(held.Why, "approved (replayed)") {
		t.Errorf("step why = %q, want it to record the replay, not a fresh decision", held.Why)
	}
	// The model is asked once per step AFTER the held one — never for the
	// held step itself. The resumed loop runs steps 1..3, so at most 3
	// more calls; it must not be 4, which is what re-asking step 1 costs.
	if got := m.calls - callsAtHold; got > 3 {
		t.Errorf("model called %d times after the hold; the held step was re-asked "+
			"(steps 1-3 can each need one call)", got)
	}
}

// The tier a step computes must reach the wire. It did not before: the
// loop calculated a tier per step (M3), stored it on the run record, and
// sent one fixed combo forever — so "tiered routing" was decoration.
func TestReasonerSendsTheStepsTier(t *testing.T) {
	m := &scriptedModel{replies: []string{
		`{"tool":"query","args":{"query":"x"},"why":"a"}`,
		`{"done":true,"why":"b"}`,
	}}
	r := reasonRunner(t, m, 3)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(m.tiers) == 0 {
		t.Fatal("no tier was sent to the model")
	}
	for i, tier := range m.tiers {
		switch tier {
		case TierPlanning, TierExecution, TierSynthesis:
		default:
			t.Errorf("call %d tier = %q, want one of the three routing tiers", i, tier)
		}
	}
}

// Synthesis is its own step type, so a bound-exit answer may run on a
// different model than the loop that got stuck.
func TestSynthesisCarriesItsOwnTier(t *testing.T) {
	m := &scriptedModel{}
	r := reasonRunner(t, m, 1)
	// max_steps=1 forces a bound exit, which synthesises.
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	found := false
	for _, tier := range m.tiers {
		if tier == TierSynthesis {
			found = true
		}
	}
	if !found {
		t.Errorf("tiers sent = %v, want the synthesis call to name %q", m.tiers, TierSynthesis)
	}
}
