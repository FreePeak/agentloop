// Package main implements the agentloop HTTP service.
// M1 ships the runs API: submit, poll, kill, and SSE events.
// M5 wires ApprovalGate into the runner and adds approval HTTP endpoints.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/agentloop/internal/budget"
	"github.com/FreePeak/agentloop/internal/eval"
	"github.com/FreePeak/agentloop/internal/leankg"
	"github.com/FreePeak/agentloop/internal/loop"
	"github.com/FreePeak/agentloop/internal/onegw"
	"github.com/FreePeak/agentloop/internal/planner"
	"github.com/FreePeak/agentloop/internal/tools"
	"github.com/FreePeak/agentloop/internal/xdev"
)

// Server holds the in-memory run store, the runner/gate registry,
// the tool registry, the M6 eval runner that gates deploys, and the
// M8 model transport shared by every run.
type Server struct {
	mu         sync.Mutex
	runs       map[string]loop.RunResult
	runners    map[string]*loop.LoopRunner
	gates      map[string]*loop.ApprovalGate
	tools      tools.ToolRegistry
	evalRunner *eval.Runner
	model      *onegw.Client
	// sandboxDir is the workspace sandboxed turns run in; empty when no
	// executor is configured.
	sandboxDir string
}

// NewServer creates a Server with the v1 tool set and empty stores.
//
// Both outbound dependencies are wired from the environment, because
// endpoints and credentials are deployment facts, not code:
//
//	AGENTLOOP_ONEGW_URL    default http://127.0.0.1:8080
//	AGENTLOOP_ONEGW_KEY    bearer key; empty sends no Authorization header
//	AGENTLOOP_ONEGW_COMBO  routing combo used as the wire model, default "dev"
//	AGENTLOOP_LEANKG_URL   code-graph root, default http://127.0.0.1:8090
//	AGENTLOOP_LEANKG_OFF   any value disables the knowledge client
//
// The tool registry is built once and shared: every run's steps go through
// the same `query` client, which is the point of a registry.
//
// The eval runner deliberately gets a registry with **no** knowledge client
// and no model: the M6 deploy gate must stay deterministic and offline, so it
// must not depend on LeanKG or onegw being up (PRD §11.4).
func NewServer() *Server {
	sandboxDir := os.Getenv("AGENTLOOP_XDEV_DIR")
	reg := tools.NewRegistryWithKnowledge(leankgFromEnv()).WithSandbox(xdevFromEnv(context.Background(), sandboxDir), sandboxDir)
	return &Server{
		runs:       make(map[string]loop.RunResult),
		runners:    make(map[string]*loop.LoopRunner),
		gates:      make(map[string]*loop.ApprovalGate),
		tools:      reg,
		sandboxDir: sandboxDir,
		model: onegw.New(
			envOr("AGENTLOOP_ONEGW_URL", "http://127.0.0.1:8080"),
			os.Getenv("AGENTLOOP_ONEGW_KEY"),
			envOr("AGENTLOOP_ONEGW_COMBO", "dev"),
		).WithTiers(tiersFromEnv()),
		evalRunner: eval.NewRunner(func(cfg loop.RunnerConfig) (*loop.LoopRunner, *budget.Guard, tools.ToolRegistry, error) {
			// Same runner the service builds, minus the model: the gate and
			// the planner are part of what the cases exercise, so building a
			// bare runner here made the adversarial case unscoreable — no
			// gate means no pause, and a pause is its whole premise.
			g := budget.New(cfg.CostBudget, float64(loop.DailyCeilingMult)*cfg.CostBudget)
			// No sandbox and no knowledge client: the M6 deploy gate must
			// stay deterministic and offline (PRD §11.4), so it must not
			// depend on xdev or LeanKG being up.
			reg := tools.NewRegistry()
			gate := loop.NewApprovalGate()
			cfg.Gate = gate
			return loop.NewRunnerWithPlannerAndGate(cfg, g, reg, planner.NewPlanner(), gate), g, reg, nil
		}),
	}
}

// envOr returns the environment value for key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// leankgFromEnv wires the code-graph client from the environment.
//
//	AGENTLOOP_LEANKG_URL   default http://127.0.0.1:8090
//	AGENTLOOP_LEANKG_OFF   any non-empty value disables retrieval
//
// No LeanKG API key: the service is local and its REST surface is
// unauthenticated by design for a single-tenant deployment (PRD §7.4).
// tiersFromEnv maps this loop's three routing tiers onto onegw combos.
//
// The gateway ships whatever combos an operator configured — in this
// portfolio, exactly one (`dev`). Naming a combo here that does not exist
// upstream is how "tiered routing" becomes an error instead of a saving,
// so an unset tier simply falls through to AGENTLOOP_ONEGW_COMBO and the
// loop still runs:
//
//	AGENTLOOP_ONEGW_COMBO_PLANNING   combo for plan/replan steps
//	AGENTLOOP_ONEGW_COMBO_EXECUTION  combo for action steps (the hot path)
//	AGENTLOOP_ONEGW_COMBO_SYNTHESIS  combo for the bound-exit answer
func tiersFromEnv() map[string]string {
	out := map[string]string{}
	for tier, key := range map[string]string{
		loop.TierPlanning:  "AGENTLOOP_ONEGW_COMBO_PLANNING",
		loop.TierExecution: "AGENTLOOP_ONEGW_COMBO_EXECUTION",
		loop.TierSynthesis: "AGENTLOOP_ONEGW_COMBO_SYNTHESIS",
	} {
		if v := os.Getenv(key); v != "" {
			out[tier] = v
		}
	}
	return out
}

func leankgFromEnv() *leankg.Client {
	if os.Getenv("AGENTLOOP_LEANKG_OFF") != "" {
		return nil
	}
	return leankg.New(envOr("AGENTLOOP_LEANKG_URL", "http://127.0.0.1:8090"))
}

// xdevFromEnv starts ONE sandbox child and returns it, or nil when no
// executor is wanted.
//
//	AGENTLOOP_XDEV_BIN   default "xdev"
//	AGENTLOOP_XDEV_DIR   the workspace turns run in; default is a fresh
//	                     temp dir, because a sandbox pointed at the
//	                     server's own cwd can edit agentloop itself
//	AGENTLOOP_XDEV_OFF   any value disables it
//
// One child is shared by every run on purpose: `xdev rpc` keeps a session
// and a model conversation, so a per-run child would pay startup on every
// step and lose the thread between them. It is still one turn at a time —
// the client serialises prompts.
//
// A child that will not start is not fatal: the registry is built without
// a sandbox and `run_tests`/`write_file` report that no executor is
// configured. That is the same shape as LeanKG being down, and it keeps a
// deploy without xdev able to run the read-only paths.
func xdevFromEnv(ctx context.Context, dir string) *xdev.Client {
	if os.Getenv("AGENTLOOP_XDEV_OFF") != "" {
		return nil
	}
	if dir == "" {
		d, err := os.MkdirTemp("", "agentloop-sandbox-")
		if err != nil {
			log.Printf("sandbox: no workspace: %v", err)
			return nil
		}
		dir = d
	}
	c, err := xdev.Start(ctx, xdev.Config{
		Command: envOr("AGENTLOOP_XDEV_BIN", "xdev"),
		Dir:     dir,
	})
	if err != nil {
		log.Printf("sandbox: xdev unavailable, run_tests and write_file will report no executor: %v", err)
		return nil
	}
	log.Printf("sandbox: xdev rpc started in %s", dir)
	return c
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
		SandboxDir: s.sandboxDir,
		// M8: the loop can call the portfolio's gateway for synthesis.
		// The eval runner deliberately gets no model — the deploy gate
		// must stay deterministic and offline.
		Model: s.model,
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
	writeJSON(w, map[string]any{"run_id": runID, "state": loop.StateThinking})
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
	writeJSON(w, result)
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
	// Signal the live runner's kill channel so the running
	// goroutine exits within one step (P75). Without this the
	// stored state is stamped but the loop keeps going.
	s.mu.Lock()
	runner := s.runners[runID]
	s.mu.Unlock()
	if runner != nil {
		runner.Kill()
	}
	s.mu.Lock()
	s.runs[runID] = result
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, result)
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
	writeJSON(w, map[string]any{"run_id": runID, "approvals": gate.Ledger(), "state": result.State})
}

func (s *Server) submitApproval(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)
	s.mu.Lock()
	gate, ok := s.gates[runID]
	runner := s.runners[runID]
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
	// M5 resume path: approving the held step re-enters the
	// runner, which re-checks the gate and continues the loop.
	// Without this, approval is a ledger write that never
	// resumes the run (issue: approval → resume gap).
	if approved && runner != nil {
		result, err := runner.Resume(context.Background())
		if err != nil {
			result.State = loop.StateFailed
		}
		s.mu.Lock()
		s.runs[runID] = result
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"approved": approved})
}

func (s *Server) submitApprovalByID(w http.ResponseWriter, r *http.Request) {
	runID, stepID := parseApprovalPath(r.URL.Path)
	s.mu.Lock()
	gate, ok := s.gates[runID]
	runner := s.runners[runID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"no gate found"}`, http.StatusNotFound)
		return
	}
	approved := gate.Approve(runID, stepID)
	// M5 resume path (see submitApproval).
	if approved && runner != nil {
		result, err := runner.Resume(context.Background())
		if err != nil {
			result.State = loop.StateFailed
		}
		s.mu.Lock()
		s.runs[runID] = result
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"approved": approved})
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
	// M6: run the full eval suite (PRD §11.4, §13.1).
	// Deploy is blocked when pass rate < 85%.
	report, err := s.evalRunner.Run(context.Background(), "default", m6SuiteCases())
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, eval.Report{SuiteID: "default", Total: 0, Passed: 0, PassRate: 0, ByCategory: map[string]float64{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, report)
}

func (s *Server) runsListHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	runs := make([]loop.RunResult, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"runs": runs})
}

func (s *Server) consoleRunsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	runCount := len(s.runs)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	writeConsole(w, "<html><body><h1>Runs</h1><p>Total: %d</p></body></html>", runCount)
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
	writeConsole(w, "<html><body><h1>Approvals</h1><p>Pending: %d</p></body></html>", pending)
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
	// Signal the live runner's kill channel (P75); the stored
	// state alone does not stop the running goroutine.
	runner := s.runners[runID]
	if runner != nil {
		runner.Kill()
	}
	s.runs[runID] = result
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, result)
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
		writeConsole(w, "event: step\ndata: %s\n\n", b)
		if flush != nil {
			flush.Flush()
		}
	}
	done := map[string]any{"state": result.State, "exit_reason": result.ExitReason}
	b, _ := json.Marshal(done)
	writeConsole(w, "event: done\ndata: %s\n\n", b)
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

// writeJSON writes one JSON body. A failure here cannot be reported to the
// client — the status line and headers are already on the wire — so the error
// is explicitly discarded rather than left unchecked, and the request is
// logged so a broken client is still visible.
func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// m6SuiteCases returns the M6 acceptance suite (PRD §11.4) used as
// the deploy gate. It delegates to eval.DefaultSuite so the suite
// definition lives in one place (internal/eval) and the HTTP handler
// stays a thin wire.
func m6SuiteCases() []eval.Case {
	return eval.DefaultSuite()
}

// writeConsole is writeJSON's counterpart for the HTML and SSE surfaces,
// where a failed write is equally unreportable and equally worth logging.
func writeConsole(w http.ResponseWriter, format string, args ...any) {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		log.Printf("write console: %v", err)
	}
}

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
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
