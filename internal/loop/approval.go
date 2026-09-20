// Package loop holds the ApprovalGate (M5: HITL, P30/P68).
//
// Fail-closed policy table + audit ledger. The gate holds
// a step (state=paused_approval) until the operator approves
// or the timeout fires — a fired timeout DENIES (M5:
// "timeout denies"). Policy and transport are separate:
// the runner holds the gate, the HTTP surface only reads/writes
// the ledger (P33).
package loop

import (
	"fmt"
	"sync"
	"time"
)

// Category is the HITL bucket a tool call falls into.
type Category string

const (
	CatAuto    Category = "auto"    // read/search/list/get — never interrupt (P30)
	CatConfirm Category = "confirm" // update/edit/patch — auto if confident
	CatApprove Category = "approve" // delete/send/deploy/pay — always approve
)

// Categorize maps a tool name to its HITL category.
func Categorize(tool string) Category {
	switch {
	case tool == "read", tool == "search", tool == "list", tool == "get":
		return CatAuto
	case tool == "update", tool == "edit", tool == "patch", tool == "write":
		return CatConfirm
	case tool == "delete", tool == "send", tool == "deploy", tool == "pay":
		return CatApprove
	default:
		return CatApprove // fail closed
	}
}

// Decision is the outcome of one gate check.
type Decision struct {
	Action    string    `json:"action"`    // approve | deny
	Category  Category  `json:"category"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
}

func (d Decision) ShouldRun() bool { return d.Action == "approve" }

// ApprovalRequest is one gate submission from the runner.
type ApprovalRequest struct {
	RunID      string    `json:"run_id"`
	StepID     int       `json:"step_id"`
	Tool       string    `json:"tool"`
	ArgsHash   string    `json:"args_hash"`
	Category   Category  `json:"category"`
	Confidence float64   `json:"confidence"`
	Requested  time.Time `json:"requested"`
}

// ApprovalRecord is what the audit ledger stores.
type ApprovalRecord struct {
	ApprovalRequest `json:",inline"`
	Decision        Decision `json:",inline"`
}

// ApprovalGate is the fail-closed policy table (P30/P68).
// Timeout: how long a request may sit unapproved before DENY.
// MinConfidence: below this, confirm-category is held.
type ApprovalGate struct {
	mu              sync.Mutex
	Timeout         time.Duration
	MinConfidence   float64
	ledger          []ApprovalRecord
	timeNow         func() time.Time // overridable for tests
	pendingDecisions map[string]Decision // runID:step → pending approve
}

// NewApprovalGate returns a gate with M5 defaults:
// 30s approval window (timeout denies), 0.7 confidence floor.
func NewApprovalGate() *ApprovalGate {
	return &ApprovalGate{
		Timeout:          30 * time.Second,
		MinConfidence:    0.7,
		timeNow:          time.Now,
		pendingDecisions: make(map[string]Decision),
	}
}

// Key returns the ledger key for a request (runID + step).
func (req ApprovalRequest) Key() string {
	return fmt.Sprintf("%s:%d", req.RunID, req.StepID)
}

// Check evaluates one request against the policy + timeout.
// Fail-closed: unhandled combinations return deny with a
// labelled reason. The timeout DENIES (M5 acceptance).
//
// Policy (P30):
//   auto → approve immediately (read → auto)
//   confirm → approve if confidence >= MinConfidence, else deny
//   approve → hold pending (never auto); recorded as pending
//     so the operator sees it in the queue
func (g *ApprovalGate) Check(req ApprovalRequest) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()

	if req.RunID == "" || req.Tool == "" {
		d := Decision{Action: "deny", Category: req.Category,
			Reason: "empty run_id or tool", Timestamp: g.timeNow()}
		g.ledger = append(g.ledger, ApprovalRecord{req, d})
		return d
	}

	if g.timeNow().Sub(req.Requested) > g.Timeout {
		d := Decision{Action: "deny", Category: req.Category,
			Reason: fmt.Sprintf("approval timeout after %s", g.Timeout),
			Timestamp: g.timeNow()}
		g.ledger = append(g.ledger, ApprovalRecord{req, d})
		delete(g.pendingDecisions, req.Key())
		return d
	}

	switch req.Category {
	case CatAuto:
		d := Decision{Action: "approve", Category: CatAuto,
			Reason: "read-category: auto-approved (P30 read→auto)",
			Timestamp: g.timeNow()}
		g.ledger = append(g.ledger, ApprovalRecord{req, d})
		return d
	case CatConfirm:
		if req.Confidence >= g.MinConfidence {
			d := Decision{Action: "approve", Category: CatConfirm,
				Reason: fmt.Sprintf("confirm: confidence %.2f >= floor %.2f", req.Confidence, g.MinConfidence),
				Timestamp: g.timeNow()}
			g.ledger = append(g.ledger, ApprovalRecord{req, d})
			return d
		}
		d := Decision{Action: "deny", Category: CatConfirm,
			Reason: fmt.Sprintf("confirm: confidence %.2f < floor %.2f", req.Confidence, g.MinConfidence),
			Timestamp: g.timeNow()}
		g.ledger = append(g.ledger, ApprovalRecord{req, d})
		return d
	case CatApprove:
		// Hold pending; operator decides via Server.approve.
		d := Decision{Action: "deny", Category: CatApprove,
			Reason: "high-impact: approval required (P30 always_approve)",
			Timestamp: g.timeNow()}
		g.ledger = append(g.ledger, ApprovalRecord{req, d})
		g.pendingDecisions[req.Key()] = d
		return d
	default:
		d := Decision{Action: "deny", Category: req.Category,
			Reason: "unknown category — fail closed",
			Timestamp: g.timeNow()}
		g.ledger = append(g.ledger, ApprovalRecord{req, d})
		return d
	}
}

// Approve marks a pending request as approved by the operator.
// Returns false if the request was not pending (already decided).
func (g *ApprovalGate) Approve(runID string, stepID int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := fmt.Sprintf("%s:%d", runID, stepID)
	if _, ok := g.pendingDecisions[key]; !ok {
		return false
	}
	for i := len(g.ledger) - 1; i >= 0; i-- {
		r := g.ledger[i]
		if r.RunID == runID && r.StepID == stepID && r.Decision.Action == "deny" {
			g.ledger[i].Decision = Decision{Action: "approve", Category: Categorize(r.Tool),
				Reason: "operator approved", Timestamp: g.timeNow()}
			delete(g.pendingDecisions, key)
			return true
		}
	}
	return false
}

// Ledger returns a snapshot of the audit ledger.
func (g *ApprovalGate) Ledger() []ApprovalRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ApprovalRecord, len(g.ledger))
	copy(out, g.ledger)
	return out
}

// Pending returns every request awaiting operator decision.
func (g *ApprovalGate) Pending() []ApprovalRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ApprovalRecord, 0)
	for _, r := range g.ledger {
		if r.Decision.Action == "deny" {
			out = append(out, r)
		}
	}
	return out
}

// AutoApproveCount returns how many decisions were auto-approved.
func (g *ApprovalGate) AutoApproveCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.ledger {
		if r.Decision.Action == "approve" {
			n++
		}
	}
	return n
}

// TestClock sets the gate's clock for tests; defer TestClock(time.Now).
func (g *ApprovalGate) TestClock(clock func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.timeNow = clock
}
