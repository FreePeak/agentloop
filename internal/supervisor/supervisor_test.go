package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// stubSpecialist is a deterministic Specialist for tests.
type stubSpecialist struct {
	outcome string
	tokens  int
	err     error
	calls   int
	revise  string // outcome after a revise round
}

func (s *stubSpecialist) Execute(t Task) (string, int, error) {
	s.calls++
	if s.revise != "" && t.Round > 0 {
		return s.revise, s.tokens, nil
	}
	return s.outcome, s.tokens, s.err
}

// rejectingOnceReviewer rejects the first round only (P51 convergence).
type rejectingOnceReviewer struct{ seen int }

func (r *rejectingOnceReviewer) Accept(t Task, outcome string) (bool, string) {
	r.seen++
	return r.seen > 1, "tighten the summary"
}

func testRoster(t *testing.T) *RoleRoster {
	t.Helper()
	r, err := NewRoster(
		RoleCard{Name: "researcher", Model: TierExecution, Tools: []string{"repo_search", "web_search"},
			Prompt: "find facts", Input: "question", Output: "findings", OnFail: FailEscalate, MaxSteps: 4},
		RoleCard{Name: "writer", Model: TierPlanning, Tools: []string{"write_file"},
			Prompt: "compose", Input: "findings", Output: "draft", OnFail: FailRetry, MaxSteps: 3},
	)
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	return r
}

func openGate() GateResult { return EvaluateGate(GateInput{DistinctTools: 12}) }

func TestGate_ClosedByDefault(t *testing.T) {
	got := EvaluateGate(GateInput{DistinctTools: 5})
	if got.Open {
		t.Fatalf("gate open with 5 tools, want closed: %+v", got)
	}
	if got.Triggered != nil {
		t.Errorf("triggered = %v, want none", got.Triggered)
	}
	if !strings.Contains(got.Reason, "single agent") {
		t.Errorf("reason %q should name the single-agent outcome", got.Reason)
	}
}

func TestGate_EachConditionOpens(t *testing.T) {
	cases := []struct {
		name string
		in   GateInput
		want GateCondition
	}{
		{"tools", GateInput{DistinctTools: 10}, GateToolCount},
		{"tiers", GateInput{MixedTiers: true}, GateMixedTiers},
		{"parallel", GateInput{GenuineParallel: true}, GateParallelism},
		{"context", GateInput{ContextOverflow: true}, GateContextWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateGate(tc.in)
			if !got.Open {
				t.Fatalf("gate closed, want open: %+v", got)
			}
			if len(got.Triggered) != 1 || got.Triggered[0] != tc.want {
				t.Errorf("triggered = %v, want [%s]", got.Triggered, tc.want)
			}
		})
	}
}

func TestRoster_RejectsIncompleteCard(t *testing.T) {
	if _, err := NewRoster(RoleCard{Name: "ghost"}); err == nil {
		t.Fatal("incomplete card accepted, want error")
	}
	if _, err := NewRoster(
		RoleCard{Name: "dup", Model: TierTiny, Tools: []string{"get"}, OnFail: FailAbort},
		RoleCard{Name: "dup", Model: TierTiny, Tools: []string{"get"}, OnFail: FailAbort},
	); err == nil {
		t.Fatal("duplicate role name accepted, want error")
	}
}

func TestRoster_ChannelCountIsN2Law(t *testing.T) {
	r := testRoster(t)
	if got := r.ChannelCount(); got != 1 {
		t.Errorf("2 agents: channels = %d, want 1 (N(N-1)/2)", got)
	}
	cards := []RoleCard{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		cards = append(cards, RoleCard{Name: n, Model: TierTiny, Tools: []string{"get"}, OnFail: FailAbort})
	}
	big, err := NewRoster(cards...)
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	if got := big.ChannelCount(); got != 10 {
		t.Errorf("5 agents: channels = %d, want 10", got)
	}
}

func TestBus_TypedKindsAndFIFO(t *testing.T) {
	b := NewBus()
	if err := b.Send(Message{Kind: "gossip", To: "x", Tokens: 1}); err == nil {
		t.Fatal("unknown kind accepted, want error")
	}
	if err := b.Send(Message{Kind: MsgTask, To: "x", Tokens: 0}); err == nil {
		t.Fatal("zero-token message accepted, want error")
	}
	if err := b.Send(Message{Kind: MsgTask, To: "", Tokens: 1}); err == nil {
		t.Fatal("empty receiver accepted, want error")
	}

	for i, p := range []string{"first", "second"} {
		if err := b.Send(Message{Kind: MsgTask, From: "supervisor", To: "writer", Payload: p, Tokens: i + 1}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	m, ok := b.Receive("writer")
	if !ok || m.Payload != "first" {
		t.Fatalf("FIFO broken: got %+v ok=%v", m, ok)
	}
	m, ok = b.Receive("writer")
	if !ok || m.Payload != "second" {
		t.Fatalf("FIFO broken on second: got %+v ok=%v", m, ok)
	}
	if _, ok := b.Receive("writer"); ok {
		t.Error("queue not empty after draining")
	}
	if got := b.CoordinationTokens(); got != 3 {
		t.Errorf("coordination tokens = %d, want 3", got)
	}
	if got := len(b.Log()); got != 2 {
		t.Errorf("log length = %d, want 2 (every send logged)", got)
	}
}

func TestBus_KindsExhaustive(t *testing.T) {
	for _, k := range AllMessageKinds() {
		if !k.Valid() {
			t.Errorf("kind %q listed but not valid", k)
		}
	}
	if len(AllMessageKinds()) != 4 {
		t.Errorf("kinds = %d, want 4", len(AllMessageKinds()))
	}
}

func TestSupervisor_RefusesClosedGate(t *testing.T) {
	if _, err := NewSupervisor(SupervisorConfig{}, testRoster(t), NewBus(), EvaluateGate(GateInput{DistinctTools: 5})); err == nil {
		t.Fatal("supervisor built with closed gate, want error")
	}
}

func TestSupervisor_RunAssignsAndReviews(t *testing.T) {
	roster := testRoster(t)
	bus := NewBus()
	s, err := NewSupervisor(SupervisorConfig{RunID: "r1", DecomposeTokens: 10, ReviewTokens: 5, MaxRounds: 2}, roster, bus, openGate())
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	stub := &stubSpecialist{outcome: "v1 draft", tokens: 20, revise: "v2 draft"}
	if err := s.Register("writer", stub); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetReviewer(&rejectingOnceReviewer{})

	done, err := s.Run([]Task{{ID: "t1", Role: "writer", Input: "write it", Tokens: 4}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(done) != 1 || done[0].Outcome != "v2 draft" {
		t.Fatalf("task outcome = %+v, want revised v2 draft", done)
	}
	if stub.calls != 2 {
		t.Errorf("execute calls = %d, want 2 (one assign + one revise)", stub.calls)
	}
	if got := bus.CountByKind(MsgFeedback); got != 1 {
		t.Errorf("feedback messages = %d, want 1", got)
	}
	if got := bus.CountByKind(MsgTask); got != 1 {
		t.Errorf("task messages = %d, want 1", got)
	}

	steps := s.Steps()
	for _, want := range []Step{StepDecompose, StepAssign, StepExecute, StepReview, StepRevise, StepSynthesize} {
		found := false
		for _, got := range steps {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("lifecycle step %q missing from %v", want, steps)
		}
	}
}

func TestSupervisor_RefusesUnregisteredRole(t *testing.T) {
	s, err := NewSupervisor(SupervisorConfig{RunID: "r2"}, testRoster(t), NewBus(), openGate())
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if _, err := s.Run([]Task{{ID: "t1", Role: "writer", Tokens: 1}}); err == nil {
		t.Fatal("unregistered role accepted, want error")
	}
	if err := s.Register("not-on-roster", &stubSpecialist{}); err == nil {
		t.Fatal("off-roster register accepted, want error")
	}
}

func TestSupervisor_FailureBehaviors(t *testing.T) {
	// abort: the run stops with an error.
	roster := testRoster(t)
	s, err := NewSupervisor(SupervisorConfig{RunID: "r3"}, roster, NewBus(), openGate())
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := s.Register("writer", &stubSpecialist{err: errors.New("boom")}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// writer is FailRetry on the test roster, so it retries then fails.
	if _, err := s.Run([]Task{{ID: "t1", Role: "writer", Tokens: 1}}); err == nil {
		t.Fatal("retry-then-fail did not surface an error")
	}

	// escalate: the failure becomes a question on the bus, run continues.
	roster2, err := NewRoster(RoleCard{Name: "researcher", Model: TierExecution,
		Tools: []string{"repo_search"}, OnFail: FailEscalate})
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	bus2 := NewBus()
	s2, err := NewSupervisor(SupervisorConfig{RunID: "r4"}, roster2, bus2, openGate())
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := s2.Register("researcher", &stubSpecialist{err: errors.New("blocked")}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := s2.Run([]Task{{ID: "t1", Role: "researcher", Tokens: 1}}); err != nil {
		t.Fatalf("escalate policy surfaced an error: %v", err)
	}
	if got := bus2.CountByKind(MsgQuestion); got != 1 {
		t.Errorf("escalation questions = %d, want 1", got)
	}
}

func TestSupervisor_StopsWhenCoordinationExceedsBudget(t *testing.T) {
	// Heavy coordination chatter against tiny work tokens: the run
	// must stop before assigning the remaining tasks.
	roster := testRoster(t)
	bus := NewBus()
	s, err := NewSupervisor(SupervisorConfig{RunID: "r5", DecomposeTokens: 5}, roster, bus, openGate())
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	// 1 work token per execute, but every result payload is long.
	if err := s.Register("writer", &stubSpecialist{outcome: strings.Repeat("x", 400), tokens: 1}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tasks := []Task{
		{ID: "t1", Role: "writer", Tokens: 1},
		{ID: "t2", Role: "writer", Tokens: 1},
		{ID: "t3", Role: "writer", Tokens: 1},
	}
	done, err := s.Run(tasks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !s.OverBudget() {
		t.Fatalf("coordination %% = %.1f, want over %.0f", s.CoordinationPct(), CoordinationBudgetPct)
	}
	if len(done) >= len(tasks) {
		t.Errorf("ran all %d tasks (%.1f%% coordination) — should stop early", len(done), s.CoordinationPct())
	}
}

func TestSupervisor_UnderBudgetForTightChatter(t *testing.T) {
	roster := testRoster(t)
	s, err := NewSupervisor(SupervisorConfig{RunID: "r6", DecomposeTokens: 5}, roster, NewBus(), openGate())
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := s.Register("writer", &stubSpecialist{outcome: "ok", tokens: 500}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := s.Run([]Task{{ID: "t1", Role: "writer", Tokens: 2}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.OverBudget() {
		t.Errorf("coordination %% = %.1f, want under %.0f", s.CoordinationPct(), CoordinationBudgetPct)
	}
}

func TestSupervisor_BusClockOverridable(t *testing.T) {
	bus := NewBus()
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	bus.TestClock(func() time.Time { return fixed })
	if err := bus.Send(Message{Kind: MsgTask, To: "writer", Tokens: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m, _ := bus.Receive("writer")
	if !m.SentAt.Equal(fixed) {
		t.Errorf("SentAt = %v, want %v", m.SentAt, fixed)
	}
}
