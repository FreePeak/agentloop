// M4: memory wiring and checkpoint persistence for the loop
// runner (PRD M4, P8 Checkpoint Loop, P42 Memory Versioning,
// P43 Structured State). Checkpoints are taken at the
// compression cadence (every 5 steps) and carry the memory
// tiers, the steps so far, spend, and the dedup map, so a
// resumed run neither re-fires restored writes nor loses its
// trajectory. Resume rewinds to the last completed checkpoint
// — never past it, never before it.
package loop

import (
	"encoding/json"
	"fmt"

	"github.com/FreePeak/agentloop/internal/memory"
)

// checkpoint is the durable blob for one run. It is only
// written by the LoopRunner for that run ("one writer per
// (run, resource)"; read-only fan-out everywhere else). The
// Memory payload is the four-tier store; SeenArgs is the
// idempotency map so restored steps do not re-fire (P26 +
// NFR-4).
type checkpoint struct {
	StepIdx  int            `json:"step_idx"`
	SpendUSD float64        `json:"spend_usd"`
	Memory   []byte         `json:"memory"`
	SeenArgs map[string]int `json:"seen_args"`
	Steps    []StepRecord   `json:"steps"`
}

// prepareResume loads the latest durable checkpoint for the
// run into the runner's state: memory tiers, steps, spend,
// and the dedup map. It never rewinds past the most recent
// checkpoint and leaves startStep at 0 when none exists.
func (r *LoopRunner) prepareResume() {
	r.memory = memory.New(r.cfg.RunID)
	if r.checkpointStore == nil {
		return
	}
	has, err := r.checkpointStore.HasCheckpoint(r.cfg.RunID)
	if err != nil || !has {
		return
	}
	stepIdx, raw, ok, err := r.checkpointStore.LoadCheckpoint(r.cfg.RunID)
	if err != nil || !ok {
		return
	}
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return // corrupt blob → fresh start (rollback on corruption)
	}
	if m, err := memory.Unmarshal(cp.Memory); err == nil && m != nil {
		r.memory = m
	}
	if cp.SeenArgs != nil {
		r.seenArgs = cp.SeenArgs
	}
	r.spendSoFar = cp.SpendUSD
	r.restoredSteps = cp.Steps
	r.startStep = stepIdx
	r.resumed = true
}

// saveCheckpoint persists the run's durable state. Called at
// the compression cadence (every 5 completed iterations) —
// the P8 checkpoint interval, which bounds the loss window to
// the steps after the last checkpoint.
func (r *LoopRunner) saveCheckpoint(stepIdx int, steps []StepRecord) error {
	if r.checkpointStore == nil || r.memory == nil {
		return nil
	}
	mem, err := r.memory.Marshal()
	if err != nil {
		return fmt.Errorf("marshal memory: %w", err)
	}
	cp := checkpoint{
		StepIdx:  stepIdx,
		SpendUSD: r.spendSoFar,
		Memory:   mem,
		SeenArgs: r.seenArgs,
		Steps:    steps,
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	return r.checkpointStore.SaveCheckpoint(r.cfg.RunID, stepIdx, raw)
}

// rememberStep records one iteration of working memory after
// a step executes (P31 sliding window + P34 compression are
// handled inside memory.Store). Token weight is clamped so a
// single tool result cannot monopolize the context ceiling.
func (r *LoopRunner) rememberStep(step int, tool string, data any, failure bool) {
	if r.memory == nil {
		return
	}
	tok := estimateTokens(data)
	if tok < 4 {
		tok = 4
	}
	if tok > 40 {
		tok = 40
	}
	label := "observation"
	if failure {
		label = "error"
	}
	r.memory.Add(memory.TierWorking, fmt.Sprintf("step %d %s (%s): %s", step+1, tool, label, partialString(data, 200)), tok)
}

// maybeCheckpoint saves the run at the cadence boundary.
// stepIdx is the number of completed iterations — for a
// resumed run it counts restored iterations too, so cadence
// is absolute, not per-execution-session.
func (r *LoopRunner) maybeCheckpoint(steps []StepRecord) {
	if r.memory == nil || r.checkpointStore == nil {
		return
	}
	if r.memory.Iterations > 0 && r.memory.Iterations%memory.CompressEvery == 0 {
		_ = r.saveCheckpoint(r.memory.Iterations, steps)
	}
}

// ponytail: checkpoint cadence every 5 iterations (P8) means
// steps between the last checkpoint and a fault (≤4) re-execute
// on resume unless the tool is idempotent. Upgrade path: a
// per-step executions table fulfilling P26 persist-before-execute
// would make even the loss window resume-safe.
