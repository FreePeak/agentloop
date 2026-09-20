// Package main implements the agentloop HTTP service.
// M1 ships the runs API: submit, poll, kill, and SSE events.
// M5 wires ApprovalGate into the runner and adds approval HTTP endpoints.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/eval"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/planner"
	"github.com/FreePeak/agentloop/internal/tools"
)

// Server holds the in-memory run store, the runner/gate registry,
// and the tool registry.
type Server struct {
	mu      sync.Mutex
	runs    map[string]loop.RunResult
	runners map[string]*loop.LoopRunner
	gates   map[string]*loop.ApprovalGate
	tools   tools.ToolRegistry
}

// NewServer creates a Server with the 5 v1 tools and empty stores.
func NewServer() *Server {
	return &Server{
		runs:    make(map[string]loop.RunResult),
		runners: make(map[string]*loop.LoopRunner),
		gates:   make(map[string]*loop.ApprovalGate),
		tools:   tools.NewRegistry(),
	}
}

type runRequest struct {
	Goal       string   `json:"goal"`
	Context    string   `json:"context"`
	MaxSteps   *int     `json:"max_steps,omitempty"`
	CostBudget *float64 `json:"cost_budget,omitempty"`
}

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
	gate := loop.NewApprovalGate()
	cfg := loop.RunnerConfig{
		RunID:      runID,
		MaxSteps:   maxSteps,
		WallClock:  time.Duration(loop.WallClockS) * time.Second,
		CostBudget: costBudget,
		Goal:       body.Goal,
		Context:    body.Context,
		Gate:       gate,
	}
	runner := loop.NewRunnerWithPlannerAndGate(cfg, guard, s.tools, planner.NewPlanner(), gate)
	s.mu.Lock()
	s.runners[runID] = runner
	s.gates[runID] = gate
	s.mu.Unlock()
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
	json.NewEncoder(w).Encode(map[string]any{"run_id": runID, "state": loop.StateThinking})
}

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

func (s *Server) killRun(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	result, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}
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

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	_, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}
	delete(s.runs, runID)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRunMemory(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	_, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}
	delete(s.gates, runID)
	delete(s.runners, runID)
	delete(s.runs, runID)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getApprovals(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	gate := s.gates[runID]
	result := s.runs[runID]
	s.mu.Unlock()
	if gate == nil {
		http.Error(w, `{"error":"no gate found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"run_id": runID, "approvals": gate.Ledger(), "state": result.State})
}

func (s *Server) submitApproval(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	gate, ok := s.gates[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"no gate found"}`, http.StatusNotFound)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	stepID := 0
	if v, ok := body["step_id"].(float64); ok {
		stepID = int(v)
	}
	approved := gate.Approve(runID, stepID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"approved": approved})
}

func (s *Server) submitApprovalByID(w http.ResponseWriter, r *http.Request) {
	runID, stepID := parseApprovalPath(r.URL.Path)
	s.mu.Lock()
	gate, ok := s.gates[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"no gate found"}`, http.StatusNotFound)
		return
	}
	approved := gate.Approve(runID, stepID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"approved": approved})
}

func parseApprovalPath(path string) (string, int) {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "runs" && i+1 < len(parts) {
			runID := parts[i+1]
			for j, q := range parts {
				if q == "approvals" && j+1 < len(parts) {
					if sid, err := strconv.Atoi(parts[j+1]); err == nil {
						return runID, sid
					}
				}
			}
			return runID, 0
		}
	}
	return "", 0
}

func (s *Server) evalReportHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(eval.Report{SuiteID: "default", Total: 0, Passed: 0, PassRate: 0, ByCategory: map[string]float64{}})
}

func (s *Server) runsListHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	runs := make([]loop.RunResult, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"runs": runs})
}

func (s *Server) consoleRunsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	runCount := len(s.runs)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, "<html><body><h1>Runs</h1><p>Total: %d</p></body></html>", runCount)
}

func (s *Server) consoleApprovalsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pending := 0
	for _, r := range s.runs {
		if r.State == loop.StatePausedApproval {
			pending++
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, "<html><body><h1>Approvals</h1><p>Pending: %d</p></body></html>", pending)
}

func (s *Server) consoleKillHandler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	runID, _ := body["run_id"].(string)
	s.mu.Lock()
	result, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
		return
	}
	result.State = loop.StateKilled
	result.Success = boolPtr(false)
	s.runs[runID] = result
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

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

func generateRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func extractRunID(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "runs" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func boolPtr(b bool) *bool { return &b }

func main() {
	s := NewServer()
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
	mux.HandleFunc("GET /admin/api/v1/runs", s.runsListHandler)
	mux.HandleFunc("GET /admin/api/v1/evals", s.evalReportHandler)
	mux.HandleFunc("GET /admin/console/runs", s.consoleRunsPage)
	mux.HandleFunc("GET /admin/console/approvals", s.consoleApprovalsPage)
	mux.HandleFunc("POST /admin/console/kill", s.consoleKillHandler)
	port := os.Getenv("AGENTLOOP_PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Printf("agentloop listening on %s\n", addr)
	http.ListenAndServe(addr, mux)
}
