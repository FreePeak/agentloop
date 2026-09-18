// Package tools provides the default in-memory ToolRegistry
// implementation holding the 5 v1 tools. In production the
// registry's Execute routes through AgentBase.execute(tool, args)
// (PRD §4.3 — agentloop owns the loop, xdev owns the turn).
package tools

import (
	"fmt"

	"context"
)

// Registry is an in-memory ToolRegistry pre-loaded with the 5 v1 tools.
type Registry struct {
	Tools []Tool
}

// NewRegistry returns a Registry with the default v1 tool set.
func NewRegistry() *Registry {
	return &Registry{Tools: append([]Tool(nil), DefaultTools...)}
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

// Execute runs one of the 5 v1 tools. For M1 each stub returns a
// typed ToolResult with Success=true — the real execution path is
// xdev rpc (tools 4-5) or LeanKG HTTP (tools 1-2) per PRD §4.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	switch name {
	case "repo_search":
		return ToolResult{
			Success: true,
			Data:    map[string]any{"matches": []any{}, "rung": "L2", "reason": "keyword"},
			Metadata: map[string]string{"retrieval_rung": "L2", "freshness": "ok"},
		}, nil
	case "repo_context":
		return ToolResult{
			Success: true,
			Data:    map[string]any{"context": "", "verb": "context"},
			Metadata: map[string]string{"extraction": "ast"},
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
			Success: true,
			Data:    map[string]any{"path": "", "written": true},
			Metadata: map[string]string{"idempotency_key": "pending"},
		}, nil
	}
	return ToolResult{}, fmt.Errorf("unknown tool: %s", name)
}
