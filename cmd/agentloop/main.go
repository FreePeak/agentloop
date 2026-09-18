// Package main implements the agentloop HTTP service.
// M1 ships the runs API: submit, poll, kill, and SSE events.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/tools"
)

// Server holds the in-memory run store and the tool registry.
type Server struct {
	mu    sync.Mutex
	runs  map[string]loop.RunResult
	tools tools.ToolRegistry
}

// NewServer creates a Server with the 5 v1 tools and an empty run store.
func NewServer() *Server {
	return &Server{
		runs:  make(map[string]loop.RunResult),
		tools: tools.NewRegistry(),
	}
}

// runRequest is the POST /v1/runs body.
type runRequest struct {
	Goal     string   `json:"goal"`
	Context  string   `json:"context"`
	MaxSteps *int     `json:"max_steps,omitempty"`
	CostBudget *float64 `json:"cost_budget,omitempty"`
}

// submitRun handles POST /v1/runs.
func (s *Server) submitRun(w http.ResponseWriter, r *http.Request) {
	var body runRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Goal == "" {
		http.Error(w, `{"error":"goal required"}`, http.StatusBadRequest)
		return
	}

	runID := generateRunID()
	maxSteps := loop.MaxSteps
	if body.MaxSteps != nil {
		maxSteps = *body.MaxSteps
	}
	costBudget := loop.CostBudgetUSD
	if body.CostBudget != nil {
		costBudget = *body.CostBudget
	}

	guard := budget.New(costBudget, float64(loop.DailyCeilingMult)*costBudget)
	cfg := loop.RunnerConfig{
		RunID:      runID,
		MaxSteps:   maxSteps,
		WallClock:  time.Duration(loop.WallClockS) * time.Second,
		CostBudget: costBudget,
		Goal:       body.Goal,
		Context:    body.Context,
	}
	runner := loop.NewRunner(cfg, guard, s.tools)

	// Run the loop in a goroutine so the API returns immediately.
	// Use context.Background() (not r.Context()) so the background
	// run isn't killed when the HTTP handler returns. The runner
	// enforces its own WallClock timeout via RunnerConfig.
	go func() {
		result, err := runner.Run(context.Background())
		if err != nil {
			result.State = loop.StateFailed
		}
		s.mu.Lock()
		s.runs[runID] = result
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"run_id": runID,
		"state":  loop.StateThinking,
	})
}

// getRun handles GET /v1/runs/{id}.
func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	result, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// killRun handles POST /v1/runs/{id}/kill.
func (s *Server) killRun(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	result, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}
	// In M1, kill sets the result state directly (no live runner to signal).
	result.State = loop.StateKilled
	result.Success = boolPtr(false)
	if result.PartialSynthesis == "" {
		result.PartialSynthesis = "killed by operator"
	}
	s.mu.Lock()
	s.runs[runID] = result
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// events handles GET /v1/runs/{id}/events — simplified SSE for M1.
// Returns all recorded state transitions as event-stream lines.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	result, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flush, _ := w.(http.Flusher)

	for _, step := range result.Steps {
		evt := map[string]any{"step_id": step.StepID, "tool": step.Tool, "phase": step.Phase}
		b, _ := json.Marshal(evt)
		fmt.Fprintf(w, "event: step\ndata: %s\n\n", b)
		if flush != nil {
			flush.Flush()
		}
	}

	done := map[string]any{"state": result.State, "exit_reason": result.ExitReason}
	b, _ := json.Marshal(done)
	fmt.Fprintf(w, "event: done\ndata: %s\n\n", b)
	if flush != nil {
		flush.Flush()
	}
}

// generateRunID returns a 16-byte random hex string (ULID-like).
func generateRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// extractRunID strips the /v1/runs/ prefix and any suffix.
func extractRunID(path string) string {
	rest := strings.TrimPrefix(path, "/v1/runs/")
	rest = strings.TrimSuffix(rest, "/kill")
	rest = strings.TrimSuffix(rest, "/events")
	return rest
}

func boolPtr(b bool) *bool { return &b }

func main() {
	s := NewServer()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", s.submitRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /v1/runs/{id}/kill", s.killRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	http.ListenAndServe(":8080", mux)
}
