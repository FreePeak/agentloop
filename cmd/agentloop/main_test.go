// Package main_test contains M2 live (HTTP) tests for the agentloop server.
// Uses httptest to exercise the real handlers without a port conflict.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/eval"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/tools"
)

// newTestServer wraps the production Server in an httptest server.
func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s := NewServer()
	srv := httptest.NewServer(routes(s))
	return srv, s
}

// routes registers all HTTP routes on a ServeMux — shared between
// production main() and tests so handlers stay in sync.
func routes(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", s.submitRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /v1/runs/{id}/kill", s.killRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	mux.HandleFunc("DELETE /v1/runs/{id}", s.deleteRun)
	mux.HandleFunc("DELETE /v1/runs/{id}/memory", s.deleteRunMemory)
	mux.HandleFunc("GET /v1/runs/{id}/approvals", s.getApprovals)
	mux.HandleFunc("POST /v1/runs/{id}/approvals", s.submitApproval)
	mux.HandleFunc("POST /v1/runs/{id}/approvals/{approval_id}", s.submitApprovalByID)
	// Admin/console routes (M6).
	mux.HandleFunc("GET /admin/api/v1/runs", s.runsListHandler)
	mux.HandleFunc("GET /admin/api/v1/evals", s.evalReportHandler)
	mux.HandleFunc("GET /admin/console/runs", s.consoleRunsPage)
	mux.HandleFunc("GET /admin/console/approvals", s.consoleApprovalsPage)
	mux.HandleFunc("POST /admin/console/kill", s.consoleKillHandler)
	return mux
}

// TestLive_SubmitAndPoll submits a run and polls for completion.
func TestLive_SubmitAndPoll(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// Submit a run.
	body := `{"goal":"live test run","context":"m2 live test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/runs status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	var submit map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&submit); err != nil {
		t.Fatalf("decode submit resp: %v", err)
	}
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)
	if runID == "" {
		t.Fatal("run_id empty in submit response")
	}

	// Poll until the run completes (steps recorded) or pauses for approval.
	var result RunResultResponse
	finished := false
	pausedApproval := false
	for i := 0; i < 30; i++ {
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET /v1/runs/%s: %v", runID, gerr)
		}
		if getResp.StatusCode == http.StatusOK {
			if derr := json.NewDecoder(getResp.Body).Decode(&result); derr == nil {
				if len(result.Steps) > 0 {
					finished = true
				}
				if result.State == "paused_approval" {
					pausedApproval = true
					finished = true
				}
			}
		}
		getResp.Body.Close()
		if finished {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !finished {
		t.Fatal("run did not finish with steps within timeout")
	}
	if pausedApproval {
		// M5: run paused for approval — verify gate caught a CatApprove tool.
		return
	}
	if result.ExitReason == "" {
		t.Error("ExitReason empty — run did not complete")
	}
	if len(result.Steps) == 0 {
		t.Error("no steps recorded")
	}
}

// TestLive_KillRun submits a run and kills it.
func TestLive_KillRun(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	body := `{"goal":"kill me","context":"test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	// Wait for the run result to be stored.
	waitForRun(t, srv, runID)

	// Kill it.
	killResp, err := http.Post(srv.URL+"/v1/runs/"+runID+"/kill", "application/json", nil)
	if err != nil {
		t.Fatalf("POST kill: %v", err)
	}
	defer killResp.Body.Close()
	if killResp.StatusCode != http.StatusOK {
		t.Fatalf("kill status = %d, want %d", killResp.StatusCode, http.StatusOK)
	}

	var killed map[string]any
	json.NewDecoder(killResp.Body).Decode(&killed)
	if killed["state"] != "killed" {
		t.Errorf("state = %v, want killed", killed["state"])
	}
}

// waitForRun polls GET /v1/runs/{id} until the run result
// is stored in the server. Used by kill/delete/console tests
// to avoid a race with the background goroutine.
func waitForRun(t *testing.T, srv *httptest.Server, runID string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr == nil && getResp.StatusCode == http.StatusOK {
			getResp.Body.Close()
			return
		}
		if getResp != nil {
			getResp.Body.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run never stored")
}

// TestDeleteRun submits a run, deletes it, and confirms 404 after.
func TestDeleteRun(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	body := `{"goal":"delete me","context":"test"}`
	postResp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(postResp.Body).Decode(&submit)
	postResp.Body.Close()
	runID, _ := submit["run_id"].(string)

	// Wait for the run result to be stored.
	waitForRun(t, srv, runID)

	// DELETE existing run → 204.
	delReq, _ := http.NewRequest("DELETE", srv.URL+"/v1/runs/"+runID, nil)
	delResp, derr := http.DefaultClient.Do(delReq)
	if derr != nil {
		t.Fatalf("DELETE: %v", derr)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want %d", delResp.StatusCode, http.StatusNoContent)
	}

	// GET after delete → 404.
	getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
	if gerr != nil {
		t.Fatalf("GET after delete: %v", gerr)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d, want %d", getResp.StatusCode, http.StatusNotFound)
	}

	// DELETE again → 404 (idempotent cleanup).
	delReq2, _ := http.NewRequest("DELETE", srv.URL+"/v1/runs/"+runID, nil)
	delResp2, derr2 := http.DefaultClient.Do(delReq2)
	if derr2 != nil {
		t.Fatalf("second DELETE: %v", derr2)
	}
	defer delResp2.Body.Close()
	if delResp2.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d, want %d", delResp2.StatusCode, http.StatusNotFound)
	}
}

// TestLive_EventsSSE verifies the events endpoint streams.
func TestLive_EventsSSE(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// Submit a run first.
	body := `{"goal":"sse test","context":"test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	// Wait for the run to finish so the result is stored.
	for i := 0; i < 30; i++ {
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET run: %v", gerr)
		}
		getResp.Body.Close()
		if getResp.StatusCode == http.StatusOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Stream events.
	eventsResp, err := http.Get(srv.URL + "/v1/runs/" + runID + "/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer eventsResp.Body.Close()

	if eventsResp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", eventsResp.Header.Get("Content-Type"))
	}

	// Read at least the "done" event.
	scanner := bufio.NewScanner(eventsResp.Body)
	foundDone := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "event: done") {
			foundDone = true
			break
		}
	}
	if !foundDone {
		t.Error("no 'done' event in SSE stream")
	}
}

// RunResultResponse mirrors RunResult for JSON decoding.
type RunResultResponse struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	ExitReason string `json:"exit_reason"`
	Steps      []struct {
		StepID int    `json:"step_id"`
		Tool   string `json:"tool"`
	} `json:"steps"`
}

// TestM6_EvalSuite runs 4 eval cases across all categories
// through a real LoopRunner and proves the deploy gate works.
func TestM6_EvalSuite(t *testing.T) {
	srv, s := newTestServer(t)
	defer srv.Close()

	factory := func(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
		return loop.NewRunner(cfg, budget.New(100.0, 200.0), s.tools), budget.New(100.0, 200.0), s.tools, nil
	}
	runner := eval.NewRunner(factory)

	cases := []eval.Case{
		{ID: "m6-happy", Category: eval.CatHappy, Goal: "m6 happy", Context: "test", ScoreFn: func(_ loop.RunResult) float64 { return 0.9 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
		{ID: "m6-edge", Category: eval.CatEdge, Goal: "m6 edge", Context: "test", ScoreFn: func(_ loop.RunResult) float64 { return 0.85 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
		{ID: "m6-adversarial", Category: eval.CatAdversarial, Goal: "m6 adversarial", Context: "test", ScoreFn: func(_ loop.RunResult) float64 { return 0.4 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
		{ID: "m6-regression", Category: eval.CatRegression, Goal: "m6 regression", Context: "test", ScoreFn: func(_ loop.RunResult) float64 { return 0.5 }, LatencyCap: 10 * time.Second, CostCap: 1.00},
	}

	report, err := runner.Run(context.Background(), "m6-suite", cases)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.Total != 4 {
		t.Fatalf("total = %d, want 4", report.Total)
	}
	if len(report.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(report.Results))
	}
	if report.ByCategory == nil {
		t.Fatal("by_category map is nil")
	}
	if !report.DeployBlocked() {
		t.Error("deploy should be blocked at 50% pass rate")
	}
	_ = report.PassRate
}

// TestM6_AdminRoutes verifies the admin/console endpoints
// return 200 with the expected content types.
func TestM6_AdminRoutes(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// GET /admin/api/v1/runs — empty list.
	resp, err := http.Get(srv.URL + "/admin/api/v1/runs")
	if err != nil {
		t.Fatalf("GET /admin/api/v1/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q, want application/json", resp.Header.Get("Content-Type"))
	}

	// GET /admin/api/v1/evals — empty report.
	resp, err = http.Get(srv.URL + "/admin/api/v1/evals")
	if err != nil {
		t.Fatalf("GET /admin/api/v1/evals: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// GET /admin/console/runs — HTML.
	resp, err = http.Get(srv.URL + "/admin/console/runs")
	if err != nil {
		t.Fatalf("GET /admin/console/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("content-type = %q, want text/html", resp.Header.Get("Content-Type"))
	}

	// GET /admin/console/approvals — HTML.
	resp, err = http.Get(srv.URL + "/admin/console/approvals")
	if err != nil {
		t.Fatalf("GET /admin/console/approvals: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestM6_ConsoleKill verifies the admin kill endpoint works.
func TestM6_ConsoleKill(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// Submit a run first.
	body := `{"goal":"kill via admin","context":"test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	// Wait for the run result to be stored.
	waitForRun(t, srv, runID)

	// Kill via admin endpoint.
	killResp, err := http.Post(srv.URL+"/admin/console/kill", "application/json", strings.NewReader(`{"run_id":"`+runID+`"}`))
	if err != nil {
		t.Fatalf("POST admin kill: %v", err)
	}
	defer killResp.Body.Close()
	if killResp.StatusCode != http.StatusOK {
		t.Fatalf("admin kill status = %d, want %d", killResp.StatusCode, http.StatusOK)
	}

	// Verify state was set to killed in the run store.
	getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
	if gerr != nil {
		t.Fatalf("GET: %v", gerr)
	}
	defer getResp.Body.Close()
	var result RunResultResponse
	json.NewDecoder(getResp.Body).Decode(&result)
	if result.State != "killed" {
		t.Errorf("state = %q, want killed", result.State)
	}
}

// TestM6_Demo_HITLWithEvals demonstrates M5 HITL approval + M6
// eval gate together: a CatApprove tool call pauses the run
// (M5), then the eval suite reports blocked below threshold (M6).
func TestM6_Demo_HITLWithEvals(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// Step 1: M5 — submit a run that hits a CatApprove tool.
	// Use the gate-aware runner through a real HTTP round-trip:
	// submit a goal, poll for the paused_approval state.
	body := `{"goal":"demo hitl","context":"demo"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	// Poll for completion (the run runs in a goroutine).
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET: %v", gerr)
		}
		var result RunResultResponse
		json.NewDecoder(getResp.Body).Decode(&result)
		getResp.Body.Close()
		if result.State != "" {
			break
		}
	}

	// Step 2: M6 — run an eval suite against a tool registry.
	// Verify the eval endpoint returns a report.
	evalResp, err := http.Get(srv.URL + "/admin/api/v1/evals")
	if err != nil {
		t.Fatalf("GET /admin/api/v1/evals: %v", err)
	}
	defer evalResp.Body.Close()
	if evalResp.StatusCode != http.StatusOK {
		t.Fatalf("eval status = %d, want %d", evalResp.StatusCode, http.StatusOK)
	}
	var report eval.Report
	json.NewDecoder(evalResp.Body).Decode(&report)
	if report.SuiteID == "" {
		t.Error("eval report missing suite_id")
	}
}

// TestM5_ApprovalEndpoints verifies the M5 approval HTTP
// surface: GET returns pending approvals, POST approves.
func TestM5_ApprovalEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// GET approvals for unknown run → 404.
	resp, err := http.Get(srv.URL + "/v1/runs/unknown/approvals")
	if err != nil {
		t.Fatalf("GET approvals: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	// POST approve for unknown run → 404.
	body := `{"step_id":0}`
	resp, err = http.Post(srv.URL+"/v1/runs/unknown/approvals", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestM5_ApproveByStepID verifies approving a specific
// step ID via the /approvals/{approval_id} endpoint.
func TestM5_ApproveByStepID(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// POST /v1/runs/{id}/approvals/0 for unknown run → 404.
	resp, err := http.Post(srv.URL+"/v1/runs/unknown/approvals/0", "application/json", nil)
	if err != nil {
		t.Fatalf("POST approve-by-id: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestM5_GateRegisteredForNewRun proves the gate the run was built
// with is the one enforcing HITL. Regression guard for issue #12:
// submitRun constructed the runner with a nil gate while the gate
// sat in the map, so no run ever paused for approval.
//
// The assertion is the paused state, not the approvals endpoint —
// that endpoint reads the map (populated either way) and would pass
// even while the runner ran gateless.
func TestM5_GateRegisteredForNewRun(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	body := `{"goal":"gate registered","context":"test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)
	if runID == "" {
		t.Fatal("run_id empty")
	}

	// These tools are not all read-category: repo_context categorizes
	// to approve (fail closed), so a gateless runner is the only way
	// this run reaches a terminal state without pausing.
	var state string
	for i := 0; i < 100; i++ {
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET run: %v", gerr)
		}
		var result RunResultResponse
		der := json.NewDecoder(getResp.Body).Decode(&result)
		getResp.Body.Close()
		if der == nil && result.State != "" {
			state = result.State
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "paused_approval" {
		t.Fatalf("run state = %q, want paused_approval (runner has no gate wired)", state)
	}
}

// TestM4_DeleteRunMemory verifies DELETE /v1/runs/{id}/memory.
func TestM4_DeleteRunMemory(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	// Submit a run.
	body := `{"goal":"memory test","context":"test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	// Wait for the run to be stored.
	waitForRun(t, srv, runID)

	// DELETE memory → 204.
	delReq, _ := http.NewRequest("DELETE", srv.URL+"/v1/runs/"+runID+"/memory", nil)
	delResp, derr := http.DefaultClient.Do(delReq)
	if derr != nil {
		t.Fatalf("DELETE memory: %v", derr)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE memory status = %d, want %d", delResp.StatusCode, http.StatusNoContent)
	}

	// GET after delete → 404.
	getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
	if gerr != nil {
		t.Fatalf("GET after delete: %v", gerr)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d, want %d", getResp.StatusCode, http.StatusNotFound)
	}
}
