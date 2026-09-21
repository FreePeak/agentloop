package memory_test

import (
	"testing"

	"github.com/FreePeak/agentloop/internal/memory"
)

// Test70PercentRule is the M4 acceptance case from PRD §13:
// a 20-iteration run holds the 70% rule. Every Add is a
// pre-action write, so after 20 iterations Usage() is still
// under the reasoning-budget ceiling.
func Test70PercentRule(t *testing.T) {
	s := memory.New("m4-70p-run")
	for i := 0; i < 20; i++ {
		// Each step adds a sizable observation; total raw
		// would be 20 * 12 = 240 tokens (240% of window)
		// without compression and eviction.
		s.Add(memory.TierWorking, "observation from step", 12)
	}
	if u := s.Usage(); u > memory.ContextCeiling {
		t.Fatalf("after 20 iterations usage %.2f exceeds 70%% ceiling", u)
	}
	if got := s.UsedTokens(); got > int(memory.WindowTokens*memory.ContextCeiling) {
		t.Fatalf("used tokens %d > ceiling %d", got, int(memory.WindowTokens*memory.ContextCeiling))
	}
}

// TestLandmarkNeverEvicted is P32: a landmark entry that
// arrives under the ceiling survives compaction/eviction.
func TestLandmarkNeverEvicted(t *testing.T) {
	s := memory.New("m4-landmark-run")
	s.Add(memory.TierLandmark, "user feedback decision", 12)
	// Fill working up to the ceiling so the landmark should
	// be forced to stay while working items compress.
	for i := 0; i < 20; i++ {
		s.Add(memory.TierWorking, "noise", 6)
	}
	if len(s.Landmarks()) == 0 {
		t.Fatal("landmark (P32) was evicted — must be never-evicted")
	}
}

// TestCompressCadence is P34: compaction happens every 5
// iterations and keeps the working tier inside the ceiling.
// The working tier is unexported, so the effect is observed
// through Usage() and UsedTokens().
func TestCompressCadence(t *testing.T) {
	s := memory.New("m4-compress-run")
	for i := 0; i < 5; i++ {
		s.Add(memory.TierWorking, "raw", 8)
	}
	// Iteration 5 compresses 5x8=40 tokens into one bullet.
	s.Add(memory.TierWorking, "after", 8)
	if u := s.Usage(); u > memory.ContextCeiling {
		t.Fatalf("usage %.2f exceeds ceiling", u)
	}
	// Compression must not erase history entirely.
	if s.UsedTokens() == 0 {
		t.Fatal("working tier fully evicted — compression erased history")
	}
}

// TestDelete clears every tier (P40 / PII removal).
func TestDelete(t *testing.T) {
	s := memory.New("m4-delete-run")
	for i := 0; i < 5; i++ {
		s.Add(memory.TierWorking, "x", 8)
	}
	s.Delete()
	if s.Usage() != 0 {
		t.Fatalf("usage after Delete = %.2f, want 0", s.Usage())
	}
	if got := len(s.Items()); got != 0 {
		t.Fatalf("items after Delete = %d, want 0", got)
	}
}

// TestRoundTrip ensures serialization (P42) preserves the
// 70% invariant across a marshal/unmarshal cycle.
func TestRoundTrip(t *testing.T) {
	s := memory.New("m4-rt-run")
	for i := 0; i < 10; i++ {
		s.Add(memory.TierWorking, "step", 6)
	}
	blob, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := memory.Unmarshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != s.RunID {
		t.Fatalf("run id changed: %q -> %q", s.RunID, got.RunID)
	}
	if u := got.Usage(); u > memory.ContextCeiling {
		t.Fatalf("restored usage %.2f exceeds 70%% ceiling", u)
	}
}

// TestUnmarshalRejectsUnknownTier is the P43 validation
// path: a corrupt checkpoint must not load.
func TestUnmarshalRejectsUnknownTier(t *testing.T) {
	blob := []byte(`{"run_id":"bad","iterations":1,` +
		`"working":[{"tier":"time-travel","data":"x","tokens":4,"is_landmark":false}],` +
		`"landmarks":[],"retrieved":[],"system_tokens":20}`)
	if _, err := memory.Unmarshal(blob); err == nil {
		t.Fatal("Unmarshal accepted unknown tier — P43 validation missing")
	}
}
