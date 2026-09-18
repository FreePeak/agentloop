// Package tools defines the tool registry interface (P16 Tool Router,
// P19 Tool Validation, P26 Idempotent Tools) that agentloop uses to
// invoke tools. The interface is the contract the acceptance suite
// (PRD §11.2) uses to inject test doubles — the registry is
// interface-based by design, not by accident.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ToolResult is the typed envelope every tool returns.
// Success=false means the tool failed in a way the loop can
// observe (never raise); Data carries the structured output;
// Message is human/agent-readable; Metadata carries routing
// signals (retrieval rung, freshness, etc.).
type ToolResult struct {
	Success  bool              `json:"success"`
	Data     map[string]any    `json:"data"`
	Message  string            `json:"message,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Tool describes one visible tool in the registry. DO NOT USE WHEN
// is the highest-ROI prompt hour (PRD §4): it tells the model what
// this tool must never be used for.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	UseWhen      string          `json:"use_when"`
	DoNotUseWhen string          `json:"do_not_use_when"`
	Schema       json.RawMessage `json:"schema"`
	TimeoutMs    int             `json:"timeout_ms"`
	Sandboxed    bool            `json:"sandboxed"`
}

// ToolRegistry is the interface agentloop calls into. Implementations
// range from the real 5-tool set to test doubles that the acceptance
// suite registers (PRD §11.2 cases 2 and 5).
type ToolRegistry interface {
	List() []Tool
	Validate(name string, args map[string]any) error
	Execute(ctx context.Context, name string, args map[string]any) (ToolResult, error)
}

// DedupKey returns the sha256 fingerprint for a tool call:
// sha256("run_id:tool:canonical_args"). This is the durable idempotency
// key agentloop persists BEFORE execution (PRD §4.2, P26). The
// canonical_args form is JSON with sorted keys so the same arguments
// always produce the same key regardless of insertion order.
func DedupKey(runID, tool string, args map[string]any) string {
	b, _ := json.Marshal(canonical(args))
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s", runID, tool, string(b))))
	return hex.EncodeToString(h[:])
}

// canonical returns a copy of args with all nested maps sorted by
// key so that two maps with the same content produce identical JSON.
func canonical(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = canonical(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = canonical(val)
		}
		return out
	default:
		return v
	}
}

// noopJSON ensures json.Marshal compiles even if nothing uses it
// directly in this file.
var _ = json.Marshal

// DefaultTools is the v1 tool surface — 5 tools over LeanKG and xdev
// (PRD §4). Each entry is a description with a USE WHEN and a DO NOT USE WHEN.
var DefaultTools = []Tool{
	{
		Name:         "repo_search",
		Description:  "Search the codebase by keyword, element, or semantic query. Returns ranked matches with retrieval rung and freshness.",
		UseWhen:      "When you need to find where something is defined, referenced, or discussed in the repo.",
		DoNotUseWhen: "When you already know the file and line — use repo_context instead.",
		Schema:       json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		TimeoutMs:    30000,
		Sandboxed:    false,
	},
	{
		Name:         "repo_context",
		Description:  "Get AST-aware context around an element: callers, callees, impact, file context. Extracts context at the AST level, not file dumps.",
		UseWhen:      "When you have a resolved element and need its neighbourhood — before writing code or reviewing.",
		DoNotUseWhen: "When you don't know which element to ask about — use repo_search first.",
		Schema:       json.RawMessage(`{"type":"object","properties":{"element":{"type":"string"},"verb":{"type":"string","enum":["context","impact","callers","callees"]}},"required":["element","verb"]}`),
		TimeoutMs:    30000,
		Sandboxed:    false,
	},
	{
		Name:         "web_search",
		Description:  "Search the web via onegw provider kind=searxng. Results come back pre-formatted.",
		UseWhen:      "When you need current information not in the repo — versions, docs, APIs.",
		DoNotUseWhen: "For anything already in the repo — use repo_search instead.",
		Schema:       json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		TimeoutMs:    30000,
		Sandboxed:    false,
	},
	{
		Name:         "run_tests",
		Description:  "Run tests in a restricted xdev sandbox workspace (--add-dir). One call per ReAct turn, never raw shell.",
		UseWhen:      "When you need to verify code changes — the verification half of the write-test-fix loop.",
		DoNotUseWhen: "To explore the repo or search for anything — tests only.",
		Schema:       json.RawMessage(`{"type":"object","properties":{"cwd":{"type":"string"},"args":{"type":"array","items":{"type":"string"}}},"required":["cwd"]}`),
		TimeoutMs:    120000,
		Sandboxed:    true,
	},
	{
		Name:         "write_file",
		Description:  "Write or modify a file in the restricted xdev sandbox workspace. Approval-gated for irreversible actions.",
		UseWhen:      "When you need to create or edit a file as a result of a loop step.",
		DoNotUseWhen: "To read a file — use repo_context (read twin). Never to delete without approval.",
		Schema:       json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		TimeoutMs:    30000,
		Sandboxed:    true,
	},
}
