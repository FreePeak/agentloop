// Package supervisor holds the M7 multi-agent control plane: role
// cards, a typed message bus, and a supervisor that only runs when
// §10's gate is met (design.md §7, PRD §13.1 move 11).
//
// M7 is conditional by design. The gate is a token split, not a
// scale target: coordination spend above 30% of total tokens means
// there are too many agents talking (Ch.7). The N² channel law —
// N(N-1)/2 — is enforced here as arithmetic, not as advice.
package supervisor

import (
	"fmt"
	"sort"
	"strings"
)

// Tier names the model tier a role runs on (M3: onegw combos).
// Mixed tiers across roles are one of the four gate conditions.
type Tier string

const (
	TierPlanning  Tier = "planning"  // decompose/arbitrate — strongest
	TierExecution Tier = "execution" // specialists — mid
	TierTiny      Tier = "tiny"      // classify/route — cheap
)

// FailureBehavior says what a role does when its own run fails.
// Explicit, because a role with no failure behavior is a zombie
// (design.md §7: P57 lifecycle — no zombies).
type FailureBehavior string

const (
	FailEscalate FailureBehavior = "escalate" // hand to supervisor for arbitration
	FailRetry    FailureBehavior = "retry"    // one retry, then escalate
	FailAbort    FailureBehavior = "abort"    // stop the whole run
)

// RoleCard is an agent's API contract, kept in version control
// (design.md §7): name, model tier, tools, prompt, I/O format,
// failure behavior. Two roles may not share a name — the card is
// the identity, the name is how the bus routes to it.
type RoleCard struct {
	Name     string          `json:"name"`
	Model    Tier            `json:"model"`     // model tier (M3 combo)
	Tools    []string        `json:"tools"`     // visible tools for this role
	Prompt   string          `json:"prompt"`    // system prompt / role instruction
	Input    string          `json:"input"`     // I/O format: what it accepts
	Output   string          `json:"output"`    // I/O format: what it returns
	OnFail   FailureBehavior `json:"on_fail"`   // failure behavior (P57)
	MaxSteps int             `json:"max_steps"` // per-role step ceiling
}

// Validate rejects an incomplete card. A role card missing a tier,
// tools, or failure behavior is not an agent, it is a wish.
func (c RoleCard) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("role card: name required")
	}
	if c.Model == "" {
		return fmt.Errorf("role card %q: model tier required", c.Name)
	}
	if len(c.Tools) == 0 {
		return fmt.Errorf("role card %q: at least one tool required", c.Name)
	}
	if c.OnFail == "" {
		return fmt.Errorf("role card %q: failure behavior required (no zombies)", c.Name)
	}
	switch c.OnFail {
	case FailEscalate, FailRetry, FailAbort:
	default:
		return fmt.Errorf("role card %q: unknown failure behavior %q", c.Name, c.OnFail)
	}
	return nil
}

// RoleRoster is the set of roles in one supervisor run. It is an
// API surface: a specialist can only be addressed if it is on the
// roster, and the roster is what the channel math counts.
type RoleRoster struct {
	roles map[string]RoleCard
}

// NewRoster validates every card and rejects duplicate names.
// An empty roster is valid — it is a single-agent run (the v1
// default), not an error.
func NewRoster(cards ...RoleCard) (*RoleRoster, error) {
	r := &RoleRoster{roles: make(map[string]RoleCard, len(cards))}
	for _, c := range cards {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if _, dup := r.roles[c.Name]; dup {
			return nil, fmt.Errorf("role card %q: duplicate name", c.Name)
		}
		r.roles[c.Name] = c
	}
	return r, nil
}

// Get returns the card for a role name.
func (r *RoleRoster) Get(name string) (RoleCard, bool) {
	c, ok := r.roles[name]
	return c, ok
}

// Names returns role names in deterministic (sorted) order.
// Deterministic because a supervisor that assigns work in map
// order is a supervisor that cannot be replayed.
func (r *RoleRoster) Names() []string {
	out := make([]string, 0, len(r.roles))
	for name := range r.roles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Len returns the agent count N.
func (r *RoleRoster) Len() int { return len(r.roles) }

// Tiers returns the distinct model tiers in the roster. More than
// one tier is one of the four §10 gate conditions.
func (r *RoleRoster) Tiers() []Tier {
	seen := make(map[Tier]bool, len(r.roles))
	for _, c := range r.roles {
		seen[c.Model] = true
	}
	out := make([]Tier, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ChannelCount is the N² law: N(N-1)/2 coordination channels.
// 5 agents already mean 10 channels; 10 mean 45 (design.md §7).
func (r *RoleRoster) ChannelCount() int {
	n := len(r.roles)
	return n * (n - 1) / 2
}
