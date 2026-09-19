// Package main_test contains M2 live (HTTP) tests for the agentloop server.
// Uses httptest to exercise the real handlers without a port conflict.
package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	mux.HandleFunc("DELETE /v1/runs/{id}", s.deleteRun)
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

	// Poll until the run completes (steps recorded).
	var result RunResultResponse
	finished := false
	for i := 0; i < 30; i++ {
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET /v1/runs/%s: %v", runID, gerr)
		}
		if getResp.StatusCode == http.StatusOK {
			if derr := json.NewDecoder(getResp.Body).Decode(&result); derr == nil && len(result.Steps) > 0 {
				finished = true
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
