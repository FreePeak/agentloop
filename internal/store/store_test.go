package store_test

import (
	"path/filepath"
	"testing"

	"github.com/FreePeak/agentloop/internal/store"
)

func TestSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const runID = "run-abc"
	blob := []byte(`{"run_id":"run-abc","iterations":5}`)

	// No checkpoint yet.
	if ok, err := s.HasCheckpoint(runID); err != nil || ok {
		t.Fatalf("fresh store should have no checkpoint (ok=%v err=%v)", ok, err)
	}
	if _, _, ok, err := s.LoadCheckpoint(runID); err != nil || ok {
		t.Fatalf("load on empty store should miss (ok=%v err=%v)", ok, err)
	}

	if err := s.SaveCheckpoint(runID, 5, blob); err != nil {
		t.Fatal(err)
	}
	has, err := s.HasCheckpoint(runID)
	if err != nil || !has {
		t.Fatalf("checkpoint should exist after save (has=%v err=%v)", has, err)
	}

	step, state, ok, err := s.LoadCheckpoint(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || step != 5 || string(state) != string(blob) {
		t.Fatalf("loaded mismatch: ok=%v step=%d state=%q want step=5 state=%q", ok, step, state, blob)
	}

	// Re-save (same run) replaces, not appends: one row per run.
	if err := s.SaveCheckpoint(runID, 6, []byte(`{"run_id":"run-abc","iterations":6}`)); err != nil {
		t.Fatal(err)
	}
	step, _, ok, err = s.LoadCheckpoint(runID)
	if err != nil || !ok || step != 6 {
		t.Fatalf("re-save should replace: ok=%v step=%d err=%v", ok, step, err)
	}

	// Deletion API: run state is gone.
	if err := s.DeleteRun(runID); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasCheckpoint(runID); err != nil || has {
		t.Fatalf("checkpoint should be deleted (has=%v err=%v)", has, err)
	}
}

// TestCheckpointSurvivesReopen is the P8 durability case: the
// checkpoint must be readable from a fresh connection (a
// process restart after a fault) — in-memory would not survive.
func TestCheckpointSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "durable.db")

	s1, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveCheckpoint("run-dur", 5, []byte(`dur`)); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	step, state, ok, err := s2.LoadCheckpoint("run-dur")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || step != 5 || string(state) != "dur" {
		t.Fatalf("durable checkpoint lost across reopen: ok=%v step=%d state=%q", ok, step, state)
	}
}
