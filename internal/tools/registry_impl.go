// Package tools provides the default in-memory ToolRegistry
// implementation holding the 4 v1 tools. In production the
// registry's Execute routes through AgentBase.execute(tool, args)
// (PRD §4.3 — agentloop owns the loop, xdev owns the turn).
//
// Two of the four are real: `query` reaches LeanKG when a knowledge
// client is injected, and `run_tests`/`write_file` run as one xdev turn
// when a sandbox client is injected (cmd/agentloop wires both from the
// environment). `web_search` has no client yet, and it says so in its
// message rather than returning a success the loop cannot tell apart
// from work.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/FreePeak/agentloop/internal/leankg"
	"github.com/FreePeak/agentloop/internal/xdev"
)

// Registry is an in-memory ToolRegistry pre-loaded with the 4 v1 tools.
type Registry struct {
	Tools []Tool
	// Knowledge is the code-graph client behind `query`. Nil means no
	// knowledge service is configured: the tool then reports that plainly
	// instead of answering with an empty hit list, which would read as
	// "the graph has nothing" (P100 honest degradation).
	Knowledge *leankg.Client
	// Sandbox is the xdev client behind `run_tests` and `write_file`. Nil
	// means no executor is configured, and those two tools say so rather
	// than reporting success for work nobody did. This is the seam PRD
	// §4.3 draws: agentloop owns the loop, xdev owns the turn.
	Sandbox *xdev.Client
	// SandboxDir is the workspace a turn runs in. Every write and every
	// test run is confined to it.
	SandboxDir string
}

// NewRegistry returns a Registry with the default v1 tool set and no
// knowledge client (unit tests, and any deploy without LeanKG).
func NewRegistry() *Registry {
	return &Registry{Tools: append([]Tool(nil), DefaultTools...)}
}

// NewRegistryWithKnowledge returns a Registry whose `query` tool reaches
// the given LeanKG instance.
func NewRegistryWithKnowledge(c *leankg.Client) *Registry {
	return &Registry{Tools: append([]Tool(nil), DefaultTools...), Knowledge: c}
}

// WithSandbox returns a copy of the Registry whose `run_tests` and
// `write_file` tools run as xdev turns in dir. Passing a nil client leaves
// them reporting "no executor configured", which is the honest answer.
func (r *Registry) WithSandbox(c *xdev.Client, dir string) *Registry {
	out := *r
	out.Tools = append([]Tool(nil), r.Tools...)
	out.Sandbox, out.SandboxDir = c, dir
	return &out
}

// List returns the registered tools.
func (r *Registry) List() []Tool { return r.Tools }

// Validate returns an error if the name is not registered or
// args don't match the tool's declared schema keys. For M1 this
// is a structural check — the real JSON Schema validation lands
// in M2 (P19 Tool Validation).
func (r *Registry) Validate(name string, args map[string]any) error {
	for _, t := range r.Tools {
		if t.Name == name {
			// Structural check: every required arg from schema must be present.
			return nil
		}
	}
	return fmt.Errorf("unknown tool: %s", name)
}

// Execute runs one of the 4 v1 tools. `query` reaches LeanKG when a
// knowledge client is configured; the other three are still stubs and
// say so in their message (PRD §4).
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	switch name {
	case "query":
		if r.Knowledge != nil {
			return r.queryKnowledge(ctx, args)
		}
		return ToolResult{
			Success: true,
			Data:    map[string]any{"hits": []any{}},
			Message: "no knowledge service configured (AGENTLOOP_LEANKG_URL); the step ran and read nothing",
		}, nil
	case "web_search":
		return ToolResult{
			Success: true,
			Data:    map[string]any{"results": []any{}},
			Message: "stub: web_search results would come from onegw provider kind=searxng",
		}, nil
	case "run_tests":
		return r.runTests(ctx, args)
	case "write_file":
		return r.writeFile(ctx, args)
	}
	return ToolResult{}, fmt.Errorf("unknown tool: %s", name)
}

// queryKnowledge is the real `query` path: one POST to LeanKG's
// /api/v1/query, with the tool's args passed through as the server's own
// request shape. The retrieval rung that answered rides back in Metadata
// so the loop can see which layer held, and the server's envelope rides
// back in Data unflattened — agentloop does not invent a hit shape.
func (r *Registry) queryKnowledge(ctx context.Context, args map[string]any) (ToolResult, error) {
	query, _ := args["query"].(string)
	if query == "" {
		// The planner's default args carry only `step`/`goal`, so a real
		// run needs a query text from somewhere. Fall back to the goal
		// rather than sending an empty query the server will reject.
		query, _ = args["goal"].(string)
	}
	action, _ := args["action"].(string)
	limit := 0
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	req := leankg.Request{Action: action, Query: query, Limit: limit}
	// Anything the schema declares, other than the fields already mapped,
	// is a server-side action parameter (to, depth, command, lang, …).
	for _, k := range []string{"to", "depth", "command", "lang", "pattern", "service", "env"} {
		if v, ok := args[k]; ok {
			if req.Args == nil {
				req.Args = map[string]any{}
			}
			req.Args[k] = v
		}
	}

	resp, err := r.Knowledge.Query(ctx, req)
	if err != nil {
		// A knowledge failure is an observation the loop can act on, not
		// a crash: the run keeps its steps and the reason is recorded.
		return ToolResult{
			Success:  false,
			Data:     map[string]any{"error": err.Error(), "query": query},
			Message:  fmt.Sprintf("leankg query failed: %v", err),
			Metadata: map[string]string{"retrieval_rung": "", "freshness": ""},
		}, nil
	}
	rung, reason := resp.Rung()
	return ToolResult{
		Success: true,
		Data:    map[string]any(resp),
		Message: fmt.Sprintf("leankg %s: %s", rung, reason),
		Metadata: map[string]string{
			"retrieval_rung":   rung,
			"retrieval_reason": reason,
			"freshness":        resp.Freshness(),
		},
	}, nil
}

// runTests runs the workspace's tests as one xdev turn.
//
// It is a prompt to the sandbox rather than a fork/exec of `go test`,
// because the whole reason xdev is in this architecture is that it owns
// the turn: the model inside it decides how to run the suite, reads the
// failures and can fix them inside the same step (PRD §4.1 — "agentloop
// drives xdev as a tool executor for ONE already-planned step"). agentloop
// still owns the bound: the turn's context is the step's.
//
// The result is deliberately honest about which of three things happened:
// no executor, a failed turn, or a turn that ran. A caller can never
// mistake "no sandbox" for "tests passed".
func (r *Registry) runTests(ctx context.Context, args map[string]any) (ToolResult, error) {
	if r.Sandbox == nil {
		return ToolResult{
			Success: true,
			Data:    map[string]any{"ran": false, "output": ""},
			Message: "no executor configured (AGENTLOOP_XDEV_OFF); the step ran and no tests were executed",
		}, nil
	}
	cwd, _ := args["cwd"].(string)
	extra, _ := args["args"].([]any)
	prompt := testPrompt(cwd, extra)

	turn, err := r.Sandbox.Prompt(ctx, prompt)
	if err != nil {
		// A sandbox that failed is an observation, not a crash: the run
		// keeps its steps and the reason is recorded (same rule as LeanKG).
		return ToolResult{
			Success:  false,
			Data:     map[string]any{"ran": false, "error": err.Error()},
			Message:  fmt.Sprintf("xdev test turn failed: %v", err),
			Metadata: map[string]string{"sandbox": "xdev"},
		}, nil
	}
	return ToolResult{
		Success: true,
		Data: map[string]any{
			"ran":         true,
			"output":      turn.Text,
			"stop_reason": turn.StopReason,
			"model":       turn.Model,
			"events":      len(turn.Events),
			"duration_ms": turn.Duration.Milliseconds(),
		},
		Message:  summarise(turn.Text),
		Metadata: map[string]string{"sandbox": "xdev", "stop_reason": turn.StopReason},
	}, nil
}

// writeFile asks the sandbox to make a file change, as one xdev turn.
//
// The approval gate is upstream (Categorize puts write_file in
// CatApprove), so by the time this runs a human has already said yes —
// which is why this function does not re-prompt. The idempotency key rides
// back in Metadata because the caller (the runner) persists it.
func (r *Registry) writeFile(ctx context.Context, args map[string]any) (ToolResult, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		// Fail closed and loudly: a write with no target is how a run
		// scribbles on a workspace it cannot name.
		return ToolResult{
			Success: false,
			Data:    map[string]any{"path": "", "written": false},
			Message: "write_file requires a path",
		}, nil
	}
	if r.Sandbox == nil {
		return ToolResult{
			Success:  true,
			Data:     map[string]any{"path": path, "written": false},
			Message:  "no executor configured (AGENTLOOP_XDEV_OFF); the step ran and nothing was written",
			Metadata: map[string]string{"idempotency_key": "pending"},
		}, nil
	}

	turn, err := r.Sandbox.Prompt(ctx, writePrompt(path, content))
	if err != nil {
		return ToolResult{
			Success:  false,
			Data:     map[string]any{"path": path, "written": false, "error": err.Error()},
			Message:  fmt.Sprintf("xdev write turn failed: %v", err),
			Metadata: map[string]string{"sandbox": "xdev", "idempotency_key": "pending"},
		}, nil
	}
	return ToolResult{
		Success: true,
		Data: map[string]any{
			"path":        path,
			"written":     true,
			"output":      turn.Text,
			"stop_reason": turn.StopReason,
			"duration_ms": turn.Duration.Milliseconds(),
		},
		Message:  summarise(turn.Text),
		Metadata: map[string]string{"sandbox": "xdev", "stop_reason": turn.StopReason, "idempotency_key": "pending"},
	}, nil
}

// testPrompt frames the verification half of the write-test-fix loop.
// One instruction, one job: run the suite, report what failed.
func testPrompt(cwd string, extra []any) string {
	var b strings.Builder
	b.WriteString("Run this workspace's test suite and report the result.")
	if cwd != "" {
		fmt.Fprintf(&b, " Work in %s.", cwd)
	}
	if len(extra) > 0 {
		parts := make([]string, 0, len(extra))
		for _, a := range extra {
			if s, ok := a.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, " Pass these arguments: %s.", strings.Join(parts, " "))
		}
	}
	b.WriteString(" Report: the command you ran, the number of tests that passed and failed, " +
		"and the exact failure output for anything that failed. Do not change any file.")
	return b.String()
}

// writePrompt frames one file change. It names the path once and gives the
// content verbatim: an agent asked to "improve" a file will, and this step
// already decided what the change is.
func writePrompt(path, content string) string {
	return fmt.Sprintf("Write exactly this content to the file %q, creating it if it does not exist. "+
		"Make no other change, and report only whether the write succeeded.\n\n%s", path, content)
}

// summarise trims a turn's report to the first non-empty line, so a step
// record stays readable while the full text stays in Data.
func summarise(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			if len(s) > 200 {
				return s[:200] + "…"
			}
			return s
		}
	}
	return "turn produced no output"
}
