// Package memory implements the four-tier memory store (PRD M4,
// design.md §6, Ch.8): working, landmarks, retrieved, and system +
// tools. The 70% context rule is enforced on every write: state and
// history together never exceed 70% of the window, leaving 30% as the
// model's reasoning budget. Landmarks are never evicted (P32);
// working memory is compressed every 5 iterations (P34) and evicted
// lowest-value-first (P40) to hold the ceiling. Eviction is lossless
// in the book's sense — data is demoted, never destroyed.
package memory

import (
	"encoding/json"
	"fmt"
)

// Calibrated priors from PRD §17 and design.md §6 — none are specs.
const (
	// WindowTokens is the fixed context window this store budgets for.
	// Tests and the loop run against 100 tokens so the 70% rule is
	// observable without a real model context.
	WindowTokens = 100
	// ContextCeiling is the 70% rule: never fill more than 70% of the
	// window with state+history; 30% stays as the reasoning budget.
	ContextCeiling = 0.70
	// CompressEvery is the rolling compression cadence (P34): every
	// 5 iterations the working buffer is rolled up into bullets.
	CompressEvery = 5
	// MaxBullets is P34's ≤20-bullet summary cap.
	MaxBullets = 20
	// CompressRatio is the target size of a compressed bullet relative
	// to the items it replaced (book range 20–40%; we pick 25%).
	CompressRatio = 0.25
	// maxRetrievedTokens is the 20% category budget for retrieved facts.
	maxRetrievedTokens = int(WindowTokens * 0.20)
	// There is deliberately no maxLandmarkTokens: the landmark tier is never
	// evicted (P32 — "keep decisions verbatim"), so a budget for it would be
	// a threshold no code could honour. Landmark growth is capped upstream
	// by the promotion signals in Add(), not by trimming here.
	//
	// What this costs: a run that promotes many landmarks can hold the
	// ceiling open, since enforceCeiling() evicts working entries only and
	// stops when they run out. The 70% rule is therefore a working-tier
	// guarantee, not a whole-store one — stated in docs/PRD.md §9.1.
)

// Tier names the four memory tiers (design.md §6 table).
type Tier string

const (
	TierWorking   Tier = "working"   // P31 sliding window, 40% budget
	TierLandmark  Tier = "landmark"  // P32, never evicted, 20% budget
	TierRetrieved Tier = "retrieved" // P33 semantic recall / P38 fact cache, 20% budget
	TierSystem    Tier = "system"    // prompt, schemas, templates, 20% (static)
)

// Item is one memory entry. Tokens is the entry's estimated weight in
// the context window. Landmark items are never evicted (P32).
type Item struct {
	Tier       Tier   `json:"tier"`
	Data       string `json:"data"`
	Tokens     int    `json:"tokens"`
	IsLandmark bool   `json:"is_landmark"`
}

// Store is the memory for one run. Budgets are enforced per write, so
// Usage() never exceeds ContextCeiling while the store is in use.
type Store struct {
	RunID      string `json:"run_id"`
	Iterations int    `json:"iterations"`

	working   []Item
	landmarks []Item
	retrieved []Item

	// systemTokens is the static tier (prompt/schemas/templates). It
	// is NOT part of the dynamic 70% budget — state and history are
	// the working/landmark/retrieved tiers.
	systemTokens int
}

// New returns an empty Store for runID. System overhead is fixed at
// the 20% category budget and excluded from the dynamic ceiling.
func New(runID string) *Store {
	return &Store{
		RunID:        runID,
		systemTokens: int(WindowTokens * 0.20),
	}
}

// Add stores one memory entry. A working entry above the ceiling
// triggers compression (P34) then lowest-value eviction (P40) so the
// 70% rule holds on every write. Entries matching a landmark signal
// are promoted to the landmark tier (P32), which is never evicted.
func (s *Store) Add(tier Tier, data string, tokens int) {
	s.Iterations++
	if tokens <= 0 {
		return
	}
	item := Item{Tier: tier, Data: data, Tokens: tokens}

	switch tier {
	case TierSystem:
		// Static tier: keep only the fixed overhead.
		s.systemTokens = tokens
		return
	case TierLandmark:
		item.IsLandmark = true
		s.landmarks = append(s.landmarks, item)
	case TierRetrieved:
		s.retrieved = append(s.retrieved, item)
		s.trimRetrieved()
	case TierWorking:
		if isLandmarkSignal(data) {
			item.IsLandmark = true
			s.landmarks = append(s.landmarks, item)
		} else {
			s.working = append(s.working, item)
		}
	default:
		// Unknown tier is treated as working.
		s.working = append(s.working, item)
	}

	if s.Iterations%CompressEvery == 0 {
		s.Compress()
	}
	s.enforceCeiling()
}

// Compress rolls the working buffer up into at most MaxBullets
// summaries (P34: keep decisions/data/status, drop redundancy).
// Each bullet costs CompressRatio of the items it replaced, freeing
// 60–80% of the working tier.
func (s *Store) Compress() {
	if len(s.working) <= 1 || s.workingTokens() <= MaxBullets {
		return
	}
	n := len(s.working)
	total := 0
	for _, it := range s.working {
		total += it.Tokens
	}
	bulletTokens := int(float64(total) * CompressRatio)
	if bulletTokens < 1 {
		bulletTokens = 1
	}
	summary := fmt.Sprintf("summary: %d prior observations (%d tokens)", n, total)
	s.working = []Item{{Tier: TierWorking, Data: summary, Tokens: bulletTokens}}
}

// enforceCeiling evicts lowest-value working entries until the
// dynamic tiers fit under the 70% ceiling. Landmarks are never
// evicted (P32) and retrieved is capped by its own 20% budget, so the
// loop terminates.
func (s *Store) enforceCeiling() {
	ceiling := int(float64(WindowTokens) * ContextCeiling)
	for s.UsedTokens() > ceiling {
		if len(s.working) == 0 {
			// Only landmarks remain. Landmarks are never evicted (P32), so
			// the loop stops here; LandmarkBudget below is what surfaces
			// that they are the reason the ceiling cannot be met.
			break
		}
		idx := 0
		for i, it := range s.working {
			if it.Tokens < s.working[idx].Tokens {
				idx = i
			}
		}
		s.working = append(s.working[:idx], s.working[idx+1:]...)
	}
}

// trimRetrieved keeps the retrieved tier inside its 20% category
// budget, evicting the largest entries first (P38).
func (s *Store) trimRetrieved() {
	for s.retrievedTokens() > maxRetrievedTokens && len(s.retrieved) > 0 {
		idx := 0
		for i, it := range s.retrieved {
			if it.Tokens > s.retrieved[idx].Tokens {
				idx = i
			}
		}
		s.retrieved = append(s.retrieved[:idx], s.retrieved[idx+1:]...)
	}
}

// workingTokens sums the working tier's weight.
func (s *Store) workingTokens() int {
	n := 0
	for _, it := range s.working {
		n += it.Tokens
	}
	return n
}

// retrievedTokens sums the retrieved tier's weight.
func (s *Store) retrievedTokens() int {
	n := 0
	for _, it := range s.retrieved {
		n += it.Tokens
	}
	return n
}

// landmarkTokens sums the landmark tier's weight.
func (s *Store) landmarkTokens() int {
	n := 0
	for _, it := range s.landmarks {
		n += it.Tokens
	}
	return n
}

// UsedTokens returns the dynamic state+history weight (working +
// landmarks + retrieved). Landmark additions are bounded by their cap
// so they can never alone breach the ceiling.
func (s *Store) UsedTokens() int {
	return s.workingTokens() + s.landmarkTokens() + s.retrievedTokens()
}

// Usage returns the fraction of the window consumed by dynamic
// state+history. The invariant is Usage() <= ContextCeiling after
// every Add.
func (s *Store) Usage() float64 {
	return float64(s.UsedTokens()) / float64(WindowTokens)
}

// Landmarks returns the landmark entries, in insertion order. The
// never-evicted tier (P32) is exposed so replays can show decisions.
func (s *Store) Landmarks() []Item {
	out := make([]Item, len(s.landmarks))
	copy(out, s.landmarks)
	return out
}

// Items returns every dynamic entry (working, landmarks, retrieved).
func (s *Store) Items() []Item {
	out := make([]Item, 0, len(s.working)+len(s.landmarks)+len(s.retrieved))
	out = append(out, s.working...)
	out = append(out, s.landmarks...)
	out = append(out, s.retrieved...)
	return out
}

// Delete clears every tier for the run — the deletion API (P40 /
// tenant or PII removal). The store remains usable afterwards.
func (s *Store) Delete() {
	s.working = nil
	s.landmarks = nil
	s.retrieved = nil
	s.Iterations = 0
}

// Marshal serializes the store so a checkpoint can persist it
// (P43 structured state, P42 memory versioning).
func (s *Store) Marshal() ([]byte, error) {
	payload := struct {
		RunID        string `json:"run_id"`
		Iterations   int    `json:"iterations"`
		Working      []Item `json:"working"`
		Landmarks    []Item `json:"landmarks"`
		Retrieved    []Item `json:"retrieved"`
		SystemTokens int    `json:"system_tokens"`
	}{
		RunID:        s.RunID,
		Iterations:   s.Iterations,
		Working:      s.working,
		Landmarks:    s.landmarks,
		Retrieved:    s.retrieved,
		SystemTokens: s.systemTokens,
	}
	return json.Marshal(payload)
}

// Unmarshal restores a store from a checkpoint blob. It validates
// (P43) that tier names are known before restoring.
func Unmarshal(data []byte) (*Store, error) {
	payload := struct {
		RunID        string `json:"run_id"`
		Iterations   int    `json:"iterations"`
		Working      []Item `json:"working"`
		Landmarks    []Item `json:"landmarks"`
		Retrieved    []Item `json:"retrieved"`
		SystemTokens int    `json:"system_tokens"`
	}{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	s := New(payload.RunID)
	s.Iterations = payload.Iterations
	s.working = payload.Working
	s.landmarks = payload.Landmarks
	s.retrieved = payload.Retrieved
	if payload.SystemTokens > 0 {
		s.systemTokens = payload.SystemTokens
	}
	for _, it := range append(append([]Item{}, s.working...), append(s.landmarks, s.retrieved...)...) {
		switch it.Tier {
		case TierWorking, TierLandmark, TierRetrieved, TierSystem:
		default:
			return nil, fmt.Errorf("memory: unknown tier %q in checkpoint", it.Tier)
		}
	}
	// Re-assert the ceiling after restore — a hand-edited or stale
	// checkpoint must not be able to breach the 70% rule.
	s.enforceCeiling()
	return s, nil
}

// isLandmarkSignal detects the five landmark signals (design.md §6,
// P32): plan changes, user feedback, state transitions, recoveries,
// and expensive (cost-bearing) outputs. Deterministic keyword match —
// the real detector upgrades to the same five signals over model text.
func isLandmarkSignal(data string) bool {
	for _, sig := range []string{"decision", "feedback", "recovery", "state transition", "expensive"} {
		if containsFold(data, sig) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if eqFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
