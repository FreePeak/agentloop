package supervisor

import (
	"fmt"
	"sync"
	"time"
)

// Topology is how the agents are wired (design.md §7).
// Hierarchical is the default; pipeline is for ordered stages;
// peer-to-peer is deliberately absent — flat all-to-all is a
// chaos engine, and the N² law is why.
type Topology string

const (
	TopoHierarchical Topology = "hierarchical" // P52: supervisor delegates
	TopoPipeline     Topology = "pipeline"     // P47: ordered stages
)

// Valid reports whether t is a supported topology.
func (t Topology) Valid() bool {
	return t == TopoHierarchical || t == TopoPipeline
}

// CoordinationBudgetPct is the Ch.7 stop rule: coordination spend
// above 30% of total tokens means too many agents are talking.
const CoordinationBudgetPct = 30.0

// GateCondition names one of the four §10 triggers. M7 is only
// honest when the project actually meets one — the gate is a
// measurement, not a milestone checkbox.
type GateCondition string

const (
	GateToolCount     GateCondition = "10+ distinct tools"
	GateMixedTiers    GateCondition = "mixed model tiers"
	GateParallelism   GateCondition = "genuine parallelism"
	GateContextWindow GateCondition = "context beyond one window"
)

// AllGateConditions lists the four triggers in declaration order.
func AllGateConditions() []GateCondition {
	return []GateCondition{GateToolCount, GateMixedTiers, GateParallelism, GateContextWindow}
}

// GateInput is the measured state the §10 gate is evaluated
// against. Every field is a count, none a judgment call.
type GateInput struct {
	DistinctTools   int  // visible tool count a single agent would carry
	MixedTiers      bool // does the work need more than one model tier?
	GenuineParallel bool // are there truly independent subtasks?
	ContextOverflow bool // does the working set exceed one context window?
}

// GateResult is the gate's verdict plus the reason, so a refusal
// is legible in the console and in the run trace.
type GateResult struct {
	Open      bool
	Triggered []GateCondition
	Reason    string
}

// EvaluateGate decides whether M7 may run. Any one of the four
// conditions opens the gate (design.md §7); none open means the
// single agent stays, and that is a pass, not a failure.
func EvaluateGate(in GateInput) GateResult {
	var hit []GateCondition
	if in.DistinctTools >= 10 {
		hit = append(hit, GateToolCount)
	}
	if in.MixedTiers {
		hit = append(hit, GateMixedTiers)
	}
	if in.GenuineParallel {
		hit = append(hit, GateParallelism)
	}
	if in.ContextOverflow {
		hit = append(hit, GateContextWindow)
	}
	if len(hit) == 0 {
		return GateResult{
			Open:   false,
			Reason: "no §10 condition met — single agent stays (prefer fewer agents)",
		}
	}
	return GateResult{
		Open:      true,
		Triggered: hit,
		Reason:    fmt.Sprintf("%d of %d §10 conditions met", len(hit), len(AllGateConditions())),
	}
}

// Task is one unit of work the supervisor assigns to a specialist.
type Task struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Input   string `json:"input"`
	Round   int    `json:"round"` // review/revise loop counter (P51)
	Tokens  int    `json:"tokens"`
	Outcome string `json:"outcome,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

// Step is supervisor lifecycle, matching design.md §7's shape:
// decompose → assign → execute → review → revise → synthesize.
type Step string

const (
	StepDecompose  Step = "decompose"
	StepAssign     Step = "assign"
	StepExecute    Step = "execute"
	StepReview     Step = "review"
	StepRevise     Step = "revise"
	StepSynthesize Step = "synthesize"
)

// AllSteps lists the lifecycle in declaration order.
func AllSteps() []Step {
	return []Step{StepDecompose, StepAssign, StepExecute, StepReview, StepRevise, StepSynthesize}
}

// Specialist is the execution seam. The supervisor does not run
// loops itself — it assigns work, and the concrete agent (a
// LoopRunner, a stub in tests) returns an outcome.
type Specialist interface {
	Execute(task Task) (outcome string, tokens int, err error)
}

// Reviewer is the review/revise seam (P51 reviewer-writer): it
// decides whether a subtask's outcome is acceptable. Nil skips
// review — a supervisor with no reviewer has no revise loop, and
// that is a configuration, not an error.
type Reviewer interface {
	Accept(task Task, outcome string) (ok bool, feedback string)
}

// SupervisorConfig holds the tunables for one supervised run.
type SupervisorConfig struct {
	RunID                 string
	Topology              Topology
	CoordinationBudgetPct float64 // default CoordinationBudgetPct
	MaxRounds             int     // review/revise cap per subtask (P51: 2–3)
	DecomposeTokens       int     // tokens the supervisor's own planning costs
	ReviewTokens          int     // tokens each review round costs
}

// Supervisor is the run-level control plane (design.md §7). One
// run, one supervisor, N specialists — teams of 3–4 under a lead,
// never a flat mesh.
type Supervisor struct {
	cfg    SupervisorConfig
	roster *RoleRoster
	bus    *Bus
	agents map[string]Specialist
	review Reviewer
	gate   GateResult

	mu          sync.Mutex
	steps       []Step
	tasks       []Task
	spendTokens int
}

// NewSupervisor builds a supervised run. It refuses to construct
// when the §10 gate is shut: an M7 topology without a gate is
// exactly the N² tax the design is written to avoid.
func NewSupervisor(cfg SupervisorConfig, roster *RoleRoster, bus *Bus, gate GateResult) (*Supervisor, error) {
	if roster == nil || bus == nil {
		return nil, fmt.Errorf("supervisor: roster and bus required")
	}
	if !gate.Open {
		return nil, fmt.Errorf("supervisor: §10 gate closed (%s) — run a single agent", gate.Reason)
	}
	if cfg.Topology == "" {
		cfg.Topology = TopoHierarchical
	}
	if !cfg.Topology.Valid() {
		return nil, fmt.Errorf("supervisor: unknown topology %q", cfg.Topology)
	}
	if cfg.CoordinationBudgetPct <= 0 {
		cfg.CoordinationBudgetPct = CoordinationBudgetPct
	}
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = 3 // P51: reviewer-writer converges in 2–3 rounds
	}
	return &Supervisor{
		cfg:    cfg,
		roster: roster,
		bus:    bus,
		agents: make(map[string]Specialist, roster.Len()),
		gate:   gate,
	}, nil
}

// Register binds a specialist implementation to a roster role.
// A role with no agent cannot be assigned work, and registering
// an unroled name is rejected — the roster is the API surface.
func (s *Supervisor) Register(role string, agent Specialist) error {
	if _, ok := s.roster.Get(role); !ok {
		return fmt.Errorf("supervisor: role %q not on roster", role)
	}
	if agent == nil {
		return fmt.Errorf("supervisor: role %q: nil specialist", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[role] = agent
	return nil
}

// SetReviewer attaches the review seam (P51). Optional.
func (s *Supervisor) SetReviewer(r Reviewer) { s.review = r }

// Channels returns the N² coordination channel count for the roster.
func (s *Supervisor) Channels() int { return s.roster.ChannelCount() }

// Steps returns the lifecycle steps recorded so far.
func (s *Supervisor) Steps() []Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Step, len(s.steps))
	copy(out, s.steps)
	return out
}

// Tasks returns the assigned tasks in order.
func (s *Supervisor) Tasks() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, len(s.tasks))
	copy(out, s.tasks)
	return out
}

func (s *Supervisor) record(step Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, step)
}

// Run drives decompose → assign → execute → (review → revise)* →
// synthesize. It stops early when coordination spend crosses the
// 30% budget: past that line, adding agents costs more than it
// returns, and the honest move is to synthesize what exists.
func (s *Supervisor) Run(tasks []Task) ([]Task, error) {
	s.spendTokens = s.cfg.DecomposeTokens
	s.record(StepDecompose)

	s.record(StepAssign)
	done := make([]Task, 0, len(tasks))

	for _, t := range tasks {
		if t.Role == "" || t.ID == "" {
			return nil, fmt.Errorf("supervisor: task needs id and role")
		}
		if _, ok := s.roster.Get(t.Role); !ok {
			return nil, fmt.Errorf("supervisor: task %q: role %q not on roster", t.ID, t.Role)
		}
		agent, ok := s.agents[t.Role]
		if !ok {
			return nil, fmt.Errorf("supervisor: task %q: no specialist registered for role %q", t.ID, t.Role)
		}

		// supervisor → specialist
		if err := s.bus.Send(Message{
			Kind: MsgTask, From: "supervisor", To: t.Role,
			Topic: t.ID, Payload: t.Input, Tokens: t.Tokens,
		}); err != nil {
			return nil, err
		}

		s.record(StepExecute)
		outcome, tokens, err := agent.Execute(t)
		t.Tokens += tokens
		s.spendTokens += tokens

		if err != nil {
			t.Failed = true
			card, _ := s.roster.Get(t.Role)
			switch card.OnFail {
			case FailAbort:
				return done, fmt.Errorf("supervisor: task %q aborted by role %q policy: %w", t.ID, t.Role, err)
			case FailEscalate:
				// supervisor → itself: the question is the escalation.
				// Floor at 1 token so an empty error string still reports.
				qt := len(err.Error())
				if qt < 1 {
					qt = 1
				}
				if serr := s.bus.Send(Message{
					Kind: MsgQuestion, From: t.Role, To: "supervisor",
					Topic: t.ID, Payload: err.Error(), Tokens: qt,
				}); serr != nil {
					return done, serr
				}
			case FailRetry:
				outcome, tokens, err = agent.Execute(t)
				t.Tokens += tokens
				s.spendTokens += tokens
				if err != nil {
					return done, fmt.Errorf("supervisor: task %q failed after retry: %w", t.ID, err)
				}
			}
		}
		t.Outcome = outcome

		// specialist → supervisor. A failed task already reported
		// itself (escalation question or returned error), so only a
		// successful outcome goes on the bus as a result.
		if !t.Failed {
			if err := s.bus.Send(Message{
				Kind: MsgResult, From: t.Role, To: "supervisor",
				Topic: t.ID, Payload: outcome, Tokens: len(outcome),
			}); err != nil {
				return done, err
			}
			s.spendTokens += len(outcome)
		}

		// review → revise loop (P51), capped by MaxRounds.
		if s.review != nil && !t.Failed {
			for round := 0; round < s.cfg.MaxRounds; round++ {
				s.record(StepReview)
				s.spendTokens += s.cfg.ReviewTokens
				ok, feedback := s.review.Accept(t, t.Outcome)
				if ok {
					break
				}
				s.record(StepRevise)
				if err := s.bus.Send(Message{
					Kind: MsgFeedback, From: "supervisor", To: t.Role,
					Topic: t.ID, Payload: feedback, Tokens: len(feedback),
				}); err != nil {
					return done, err
				}
				t.Round = round + 1
				revised, rtokens, rerr := agent.Execute(t)
				t.Tokens += rtokens
				s.spendTokens += rtokens
				if rerr != nil {
					t.Failed = true
					break
				}
				t.Outcome = revised
			}
		}

		s.mu.Lock()
		s.tasks = append(s.tasks, t)
		s.mu.Unlock()
		done = append(done, t)

		if s.OverBudget() {
			break
		}
	}

	s.record(StepSynthesize)
	return done, nil
}

// CoordinationPct returns coordination tokens as a percentage of
// total tokens spent by the run.
func (s *Supervisor) CoordinationPct() float64 {
	total := s.TotalTokens()
	if total == 0 {
		return 0
	}
	return float64(s.bus.CoordinationTokens()) / float64(total) * 100
}

// TotalTokens returns coordination plus specialist work tokens.
func (s *Supervisor) TotalTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spendTokens + s.bus.CoordinationTokens()
}

// OverBudget reports whether coordination spend has crossed the
// 30% stop rule (Ch.7).
func (s *Supervisor) OverBudget() bool {
	return s.CoordinationPct() > s.cfg.CoordinationBudgetPct
}

// Gate returns the gate verdict this supervisor was built under.
func (s *Supervisor) Gate() GateResult { return s.gate }

// Roster returns the roster (read-only use).
func (s *Supervisor) Roster() *RoleRoster { return s.roster }

// Bus returns the coordination bus (read-only use).
func (s *Supervisor) Bus() *Bus { return s.bus }

// StartedAt-style helper kept minimal: the supervisor records step
// order, and the tracer (M2) carries wall-clock per span.
var _ = time.Now
