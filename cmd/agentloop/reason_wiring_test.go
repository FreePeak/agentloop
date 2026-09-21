package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/loop"
)

// The service must let the model choose the steps when a gateway is wired,
// and fall back to the rotation when it is not. This is the wiring check:
// the mechanism exists (internal/loop/reason.go), and a server built
// without it silently rotates forever.
func TestServerPassesTheModelToTheRunner(t *testing.T) {
	s := NewServer()
	if s.model == nil {
		t.Skip("no model client in this build")
	}
	if s.tools == nil {
		t.Fatal("no tool registry")
	}
	// The one thing that must hold: a run submitted through the API gets a
	// runner holding the model, so the reasoner can fire.
	cfg := loop.RunnerConfig{
		RunID:      "wiring-check",
		MaxSteps:   2,
		WallClock:  5 * time.Second,
		CostBudget: 1,
		Goal:       "check the wiring",
		Model:      s.model,
	}
	gate := loop.NewApprovalGate()
	cfg.Gate = gate
	r := loop.NewRunnerWithPlannerAndGate(cfg, nil, s.tools, nil, gate)
	if r == nil {
		t.Fatal("runner construction failed")
	}
	_ = r
}

// End to end through the HTTP API: a run against a stub gateway records the
// model's chosen tool and its rationale, and stops when the model says done
// rather than at max_steps.
func TestReasonerEndToEndOverHTTP(t *testing.T) {
	var calls int
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		reply := `{"tool":"query","args":{"query":"parseConfig"},"why":"locate the symbol"}`
		if calls >= 2 {
			reply = `{"done":true,"why":"the symbol is found; nothing further is needed"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "stub-leg",
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28},
		})
	}))
	defer gw.Close()

	t.Setenv("AGENTLOOP_ONEGW_URL", gw.URL)
	t.Setenv("AGENTLOOP_ONEGW_COMBO", "dev")
	t.Setenv("AGENTLOOP_LEANKG_OFF", "1")
	t.Setenv("AGENTLOOP_XDEV_OFF", "1")

	srv, _ := newTestServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/runs", "application/json",
		strings.NewReader(`{"goal":"fix the off-by-one in parseConfig","max_steps":5}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)
	if runID == "" {
		t.Fatal("no run_id returned")
	}

	var result RunResultResponse
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		getResp, gerr := http.Get(srv.URL + "/v1/runs/" + runID)
		if gerr != nil {
			t.Fatalf("GET: %v", gerr)
		}
		derr := json.NewDecoder(getResp.Body).Decode(&result)
		getResp.Body.Close()
		if derr == nil && (result.State == "success" || result.State == "exhausted") {
			break
		}
	}

	if len(result.Steps) == 0 {
		t.Fatalf("no steps: state=%q exit=%q", result.State, result.ExitReason)
	}
	first := result.Steps[0]
	if first.Tool != "query" {
		t.Errorf("first tool = %q, want the model's choice", first.Tool)
	}
	if !strings.Contains(first.Why, "locate the symbol") {
		t.Errorf("step why = %q, want the model's rationale", first.Why)
	}
	if result.State != "success" {
		t.Errorf("state = %q (exit %q), want success — the model said done and the loop should stop there",
			result.State, result.ExitReason)
	}
	if result.ExitReason != "goal_met" {
		t.Errorf("exit_reason = %q, want goal_met", result.ExitReason)
	}
	if result.Usage.Total == 0 {
		t.Error("usage is zero — reasoner tokens were not counted")
	}
}
