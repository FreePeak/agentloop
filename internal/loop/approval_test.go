package loop

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/tools"
)

func TestCategorize(t *testing.T) {
	cases := []struct {
		tool     string
		expected Category
	}{
		{"read", CatAuto},
		{"search", CatAuto},
		{"list", CatAuto},
		{"get", CatAuto},
		{"update", CatConfirm},
		{"edit", CatConfirm},
		{"patch", CatConfirm},
		{"write", CatConfirm},
		{"delete", CatApprove},
		{"send", CatApprove},
		{"deploy", CatApprove},
		{"pay", CatApprove},
		{"unknown_tool", CatApprove}, // fail closed
	}
	for _, c := range cases {
		if got := Categorize(c.tool); got != c.expected {
			t.Errorf("Categorize(%q) = %q, want %q", c.tool, got, c.expected)
		}
	}
}

func TestApprovalGate_AutoReadsAlwaysApprove(t *testing.T) {
	g := NewApprovalGate()
	req := ApprovalRequest{
		RunID: "r1", StepID: 0, Tool: "read",
		Category: CatAuto, Confidence: 0.0,
		Requested: time.Now(),
	}
	d := g.Check(req)
	if d.Action != "approve" {
		t.Errorf("auto read: got %q, want approve", d.Action)
	}
	if g.AutoApproveCount() != 1 {
		t.Errorf("AutoApproveCount = %d, want 1", g.AutoApproveCount())
	}
	if len(g.Pending()) != 0 {
		t.Errorf("Pending() = %d, want 0", len(g.Pending()))
	}
}

func TestApprovalGate_ConfirmHighConfidenceApproves(t *testing.T) {
	g := NewApprovalGate()
	req := ApprovalRequest{
		RunID: "r1", StepID: 1, Tool: "update",
		Category: CatConfirm, Confidence: 0.9,
		Requested: time.Now(),
	}
	d := g.Check(req)
	if d.Action != "approve" {
		t.Errorf("high-confirm: got %q, want approve", d.Action)
	}
}

func TestApprovalGate_ConfirmLowConfidenceDenied(t *testing.T) {
	g := NewApprovalGate()
	req := ApprovalRequest{
		RunID: "r1", StepID: 1, Tool: "update",
		Category: CatConfirm, Confidence: 0.5,
		Requested: time.Now(),
	}
	d := g.Check(req)
	if d.Action != "deny" {
		t.Errorf("low-confirm: got %q, want deny", d.Action)
	}
	if len(g.Pending()) != 1 {
		t.Errorf("Pending() = %d, want 1", len(g.Pending()))
	}
}

func TestApprovalGate_ApproveCategoryAlwaysPending(t *testing.T) {
	g := NewApprovalGate()
	req := ApprovalRequest{
		RunID: "r1", StepID: 2, Tool: "delete",
		Category: CatApprove, Confidence: 0.99,
		Requested: time.Now(),
	}
	d := g.Check(req)
	if d.Action != "deny" {
		t.Errorf("approve-cat: got %q, want deny (pending)", d.Action)
	}
	if len(g.Pending()) != 1 {
		t.Errorf("Pending() = %d, want 1", len(g.Pending()))
	}
}

// TestApprovalGate_ApproveEnablesResume proves the M5
// approval → resume gap is closed: a held CatApprove step
// re-checks the gate after the operator approves and runs,
// instead of denying on every pass (the pre-fix behaviour,
// where Approve() only flipped a ledger entry and the run
// stayed paused forever).
func TestApprovalGate_ApproveEnablesResume(t *testing.T) {
	gate := NewApprovalGate()
	guard := budget.New(100.0, 200.0)
	reg := tools.NewRegistry()
	// Step 0 is a CatApprove tool (held); step 1+ are reads.
	pick := func(step int, cfg RunnerConfig) (string, map[string]any) {
		if step == 0 {
			return "delete", map[string]any{"path": "/tmp/x"}
		}
		return "read", map[string]any{"path": "/tmp/y"}
	}
	cfg := RunnerConfig{
		RunID:    "resume-gap",
		MaxSteps: 3,
		Goal:     "resume test",
		Gate:     gate,
	}
	runner := NewRunnerWithApprovalGate(cfg, guard, reg, pick, gate)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != StatePausedApproval {
		t.Fatalf("initial state = %q, want paused_approval", result.State)
	}

	// Approve the held step. Before the fix this was a no-op:
	// the gate denied CatApprove on every pass, so Resume()
	// re-entered the loop and paused again on the same step.
	if !gate.Approve("resume-gap", 0) {
		t.Fatal("Approve(step 0) = false, want true (was pending)")
	}

	resumed, err := runner.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State == StatePausedApproval {
		t.Fatalf("resumed state = %q, want anything but paused_approval (approval did not resume the run)", resumed.State)
	}
	// The held step must have executed on resume — the
	// ledger now carries an approve for runID:step 0.
	found := false
	for _, rec := range gate.Ledger() {
		if rec.RunID == "resume-gap" && rec.StepID == 0 && rec.Decision.Action == "approve" {
			found = true
		}
	}
	if !found {
		t.Error("ledger has no approved decision for runID:step 0 after resume")
	}
}

// TestApprovalGate_TimeoutDenies is the M5 acceptance:
// a request that sits unapproved longer than the timeout
// is denied — not escalated, not re-sent. Timeout denies.
func TestApprovalGate_TimeoutDenies(t *testing.T) {
	g := NewApprovalGate()
	g.TestClock(func() time.Time { return time.Now() })

	req := ApprovalRequest{
		RunID: "r1", StepID: 3, Tool: "delete",
		Category: CatApprove, Confidence: 0.99,
		Requested: g.timeNow().Add(-2 * time.Minute),
	}
	d := g.Check(req)
	if d.Action != "deny" {
		t.Errorf("timeout: got %q, want deny", d.Action)
	}
	if d.Reason == "" {
		t.Error("timeout: empty reason")
	}
	// The timeout itself recorded a deny; the pending
	// decision (CatApprove) is replaced by the timeout
	// denial, so Pending() shows 1 (the timeout denial).
	if len(g.Pending()) != 1 {
		t.Errorf("Pending() = %d, want 1", len(g.Pending()))
	}
	ledger := g.Ledger()
	last := ledger[len(ledger)-1].Decision
	if last.Reason != "approval timeout after 30s" {
		t.Errorf("timeout reason = %q, want approval timeout after 30s", last.Reason)
	}
}

func TestApprovalGate_EmptyRunIDorToolDenied(t *testing.T) {
	g := NewApprovalGate()
	d := g.Check(ApprovalRequest{RunID: "", StepID: 0, Tool: "read"})
	if d.Action != "deny" {
		t.Errorf("empty runID: got %q, want deny", d.Action)
	}
	d = g.Check(ApprovalRequest{RunID: "r1", StepID: 0, Tool: ""})
	if d.Action != "deny" {
		t.Errorf("empty tool: got %q, want deny", d.Action)
	}
}

func TestApprovalGate_AndThenApprove(t *testing.T) {
	g := NewApprovalGate()
	req := ApprovalRequest{
		RunID: "r1", StepID: 4, Tool: "delete",
		Category: CatApprove, Confidence: 0.99,
		Requested: time.Now(),
	}
	if d := g.Check(req); d.Action != "deny" {
		t.Fatalf("initial check: got %q, want deny", d.Action)
	}
	if !g.Approve("r1", 4) {
		t.Fatal("Approve returned false for a pending request")
	}
	if g.Approve("r1", 4) {
		t.Fatal("Approve returned true for an already-approved request")
	}
	// After approve, the pending entry should be gone.
	if len(g.Pending()) != 0 {
		t.Errorf("Pending() = %d, want 0 after Approve", len(g.Pending()))
	}
	// And the ledger must reflect the approve decision.
	ledger := g.Ledger()
	if ledger[len(ledger)-1].Decision.Action != "approve" {
		t.Errorf("ledger last decision = %q, want approve", ledger[len(ledger)-1].Decision.Action)
	}
}

func TestApprovalGate_LedgerIsASnapshot(t *testing.T) {
	g := NewApprovalGate()
	for i := 0; i < 3; i++ {
		g.Check(ApprovalRequest{
			RunID: fmt.Sprintf("r%d", i), StepID: i,
			Tool: "read", Category: CatAuto,
			Requested: time.Now(),
		})
	}
	snap := g.Ledger()
	if len(snap) != 3 {
		t.Fatalf("Ledger() = %d, want 3", len(snap))
	}
	// Appending via Check should NOT change the snapshot.
	g.Check(ApprovalRequest{
		RunID: "r3", StepID: 3, Tool: "read",
		Category: CatAuto, Requested: time.Now(),
	})
	if len(snap) != 3 {
		t.Errorf("snapshot changed after append: %d", len(snap))
	}
}

func TestApprovalGate_LabelsEveryDenial(t *testing.T) {
	// M5: <10% interruptions; each interruption must be
	// labelled so the operator knows why it was held/denied.
	g := NewApprovalGate()
	reqs := []ApprovalRequest{
		{RunID: "r1", StepID: 0, Tool: "delete", Category: CatApprove, Confidence: 0.9, Requested: time.Now()},
		{RunID: "r2", StepID: 0, Tool: "update", Category: CatConfirm, Confidence: 0.5, Requested: time.Now()},
		{RunID: "r3", StepID: 0, Tool: "unknown", Category: CatApprove, Confidence: 0.9, Requested: time.Now()},
	}
	denied := 0
	for _, req := range reqs {
		d := g.Check(req)
		if d.Action == "deny" {
			if d.Reason == "" {
				t.Errorf("denial with empty reason: %+v", d)
			}
			denied++
		}
	}
	if denied != 3 {
		t.Errorf("denied = %d, want 3", denied)
	}
}
