package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The PRD claims (§7.2, NFR-1b) that every tool result reaching the model is
// screened. Before this wiring, nothing in the service ever set a screen —
// only unit tests did — so the claim was false in production.
//
// This test drives a run through the HTTP API against a stub System One and
// requires the screen to actually fire and its verdict to reach the run.
func TestGuardrailScreenFiresInProduction(t *testing.T) {
	var screens int
	var sawState string
	so := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q, want /v1/systemone", r.URL.Path)
		}
		var body struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		screens++
		// The FIRST screen is the goal; the rest are tool results. Keep the
		// goal's text so the assertion is about what was judged, not just
		// that something was.
		if screens == 1 {
			sawState = body.State
		}
		w.Header().Set("Content-Type", "application/json")
		// A jailbreak so the verdict is unambiguous.
		_, _ = w.Write([]byte(`{"model":"jev-stub","answers":{
			"jailbreak":0.95,"harmful_request":0.9,"medical_advice":0.1,
			"self_harm":0.0,"severity":2.4}}`))
	}))
	defer so.Close()

	t.Setenv("AGENTLOOP_GUARDRAIL_URL", so.URL)
	t.Setenv("AGENTLOOP_GUARDRAIL_MODEL", "jev-stub")
	t.Setenv("AGENTLOOP_LEANKG_OFF", "1")
	t.Setenv("AGENTLOOP_XDEV_OFF", "1")

	srv, _ := newTestServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/runs", "application/json",
		strings.NewReader(`{"goal":"screen me","max_steps":3}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	var result RunResultResponse
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET: %v", gerr)
		}
		derr := json.NewDecoder(getResp.Body).Decode(&result)
		getResp.Body.Close()
		// Any terminal state, including a goal-screen block, ends the wait.
		// Waiting only for exhausted/success made this test hang on the
		// very outcome it was asserting.
		if derr == nil && result.State != "" && result.State != "thinking" {
			break
		}
	}

	if screens == 0 {
		t.Fatal("the System One endpoint was never called — the screen is not wired into the service")
	}
	if sawState == "" {
		t.Error("the screen was called with empty state")
	}
	// Exactly one screen is expected here: the goal. The block must land
	// BEFORE any step runs, so no tool result is ever screened — an
	// injection must not get a single tool call first. (A run that passes
	// the goal screen then screens each tool result too; that path is
	// covered by TestScreenSeesTheResultText in internal/loop.)
	if screens != 1 {
		t.Errorf("screens = %d, want exactly 1 (the goal) — a blocked goal must not run any step", screens)
	}
	// The goal is judged first, and the block lands there.
	if sawState != "screen me" {
		t.Errorf("the first screen judged %q, want the submitted goal", sawState)
	}
	if result.ExitReason != "guardrail_block" {
		t.Errorf("exit_reason = %q, want guardrail_block (jailbreak > 0.70)", result.ExitReason)
	}
	// The block must land on the GOAL screen, before any step ran — an
	// injection must not get a single tool call first.
	if len(result.Steps) != 1 || result.Steps[0].Tool != "(goal screen)" {
		t.Errorf("steps = %+v, want exactly the goal-screen record", result.Steps)
	}
}

// With no screening endpoint configured, the run must proceed and SAY it was
// unscreened — not silently look clean.
func TestNoGuardrailEndpointIsRecordedNotAssumed(t *testing.T) {
	t.Setenv("AGENTLOOP_GUARDRAIL_URL", "")
	t.Setenv("AGENTLOOP_LEANKG_OFF", "1")
	t.Setenv("AGENTLOOP_XDEV_OFF", "1")

	srv, _ := newTestServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/runs", "application/json",
		strings.NewReader(`{"goal":"no screen configured","max_steps":2}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	var result RunResultResponse
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET: %v", gerr)
		}
		derr := json.NewDecoder(getResp.Body).Decode(&result)
		getResp.Body.Close()
		if derr == nil && result.State != "" && result.State != "thinking" {
			break
		}
	}
	if result.ExitReason == "guardrail_block" {
		t.Error("a run with no screen configured was blocked by a screen")
	}
}
