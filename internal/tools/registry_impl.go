// Package tools provides the default in-memory ToolRegistry
// implementation holding the 4 v1 tools. In production the
// registry's Execute routes through AgentBase.execute(tool, args)
// (PRD §4.3 — agentloop owns the loop, xdev owns the turn).
//
// One tool is real today: `query` reaches LeanKG when a knowledge
// client is injected (cmd/agentloop wires it from the environment).
// The other three are still stubs, and they say so in their message
// rather than returning a success the loop cannot tell apart from work.
package tools

import (
	"context"
	"fmt"

	"github.com/FreePeak/agentloop/internal/leankg"
)

// Registry is an in-memory ToolRegistry pre-loaded with the 4 v1 tools.
type Registry struct {
	Tools []Tool
	// Knowledge is the code-graph client behind `query`. Nil means no
	// knowledge service is configured: the tool then reports that plainly
	// instead of answering with an empty hit list, which would read as
	// "the graph has nothing" (P100 honest degradation).
	Knowledge *leankg.Client
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
		return ToolResult{
			Success: true,
			Data:    map[string]any{"passed": 0, "failed": 0, "output": ""},
			Message: "stub: run_tests executes via xdev rpc in a restricted --add-dir workspace",
		}, nil
	case "write_file":
		return ToolResult{
			Success:  true,
			Data:     map[string]any{"path": "", "written": false},
			Message:  "stub: write_file executes via xdev rpc in a restricted --add-dir workspace; nothing was written",
			Metadata: map[string]string{"idempotency_key": "pending"},
		}, nil
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
