// Package store persists checkpoints to SQLite (WAL), the
// durability layer that makes resume possible (P8 Checkpoint
// Loop, PRD NFR-4). One checkpoint row per run: a single
// writer per (run, resource) — the LoopRunner for a run is
// the only writer; reads are fan-out safe.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// checkpointDDL creates the checkpoints table. A single
	// PRIMARY KEY on run_id is the "one writer per run"
	// contract expressed in the schema: UPSERT replaces
	// rather than appends.
	checkpointDDL = `CREATE TABLE IF NOT EXISTS checkpoints (
		run_id TEXT NOT NULL,
		step_idx INTEGER NOT NULL,
		memory_state BLOB NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (run_id)
	);`
	schemaVersion = 1
)

// Store is the checkpoint DB. Its methods are safe for one
// writer (the run loop); concurrent writers on the same run
// must serialize externally — the schema enforces one row per run.
type Store struct {
	db *sql.DB
}

// Open creates the checkpoint store at path and initializes
// the schema. WAL mode is set so a crash mid-write leaves a
// readable checkpoint (the durability that P8 requires).
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping checkpoint db: %w", err)
	}
	// WAL + busy timeout: readers see a consistent snapshot
	// even while the writer is mid-commit. Synchronous NORMAL
	// keeps durably-faster-than-FULL without losing a crash's
	// last committed transaction.
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if _, err := db.Exec(checkpointDDL); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

// SaveCheckpoint writes (or replaces) the checkpoint for a
// run. Called by the LoopRunner at the compression cadence
// (every 5 steps) so a fault mid-step can resume from the
// last completed checkpoint rather than restarting.
func (s *Store) SaveCheckpoint(runID string, stepIdx int, memoryState []byte) error {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO checkpoints (run_id, step_idx, memory_state, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE
		 SET step_idx = excluded.step_idx,
		     memory_state = excluded.memory_state,
		     created_at = excluded.created_at`,
		runID, stepIdx, memoryState, createdAt,
	)
	return err
}

// LoadCheckpoint returns the most recent checkpoint for a
// run and whether one exists. The returned step_idx is the
// step the run was at when the checkpoint was taken — resume
// continues from here, not from 0.
func (s *Store) LoadCheckpoint(runID string) (stepIdx int, memoryState []byte, ok bool, err error) {
	row := s.db.QueryRow(`SELECT step_idx, memory_state FROM checkpoints WHERE run_id = ?`, runID)
	var state []byte
	err = row.Scan(&stepIdx, &state)
	if err == sql.ErrNoRows {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("load checkpoint %q: %w", runID, err)
	}
	return stepIdx, state, true, nil
}

// DeleteRun removes every checkpoint (and implicitly its
// state) for a run — the deletion API (tenant / PII removal).
// Checkpoint blobs may contain memory; wiping them here is
// required alongside memory.Store.Delete() so nothing lingers.
func (s *Store) DeleteRun(runID string) error {
	res, err := s.db.Exec(`DELETE FROM checkpoints WHERE run_id = ?`, runID)
	if err != nil {
		return fmt.Errorf("delete run %q: %w", runID, err)
	}
	deleted, _ := res.RowsAffected()
	if deleted == 0 {
		// Not an error — a run may have no checkpoint yet.
		return nil
	}
	return nil
}

// HasCheckpoint reports whether a checkpoint exists for the
// run (used by resume to decide between fresh start and load).
func (s *Store) HasCheckpoint(runID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoints WHERE run_id = ?`, runID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("has checkpoint %q: %w", runID, err)
	}
	return n > 0, nil
}

// Close drains the WAL and releases the file handle.
func (s *Store) Close() error {
	return s.db.Close()
}
