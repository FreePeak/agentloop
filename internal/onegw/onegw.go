// Package onegw is agentloop's outbound model transport.
//
// It is deliberately the dumbest possible client: one POST to onegw's
// OpenAI-compatible /v1/chat/completions, with the combo name as the
// `model`. agentloop owns policy (tiers, budget, approval, kill); onegw
// owns which provider answers, fallback chains, and usage accounting
// (PRD §3.1, §4.3). Nothing here re-implements a gateway: no retry
// ladder, no key pool, no fallback logic — the combo does that upstream,
// and the loop's own budget/wall-clock bounds bound this call.
//
// An empty API key is sent as no Authorization header at all, which is
// what a keyless local gateway expects (mirrors onegw's own doc note on
// anonymous upstreams).
package onegw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one OpenAI-wire chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage is the token accounting onegw reports back. agentloop records
// it so spend is observable; enforcement stays in internal/budget.
type Usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
}

// Reply is one completion.
type Reply struct {
	Content string
	Model   string // the leg that actually answered, not the combo we asked for
	Usage   Usage
}

// Client talks to one onegw instance with one combo bound to it.
type Client struct {
	baseURL string
	key     string
	combo   string
	http    *http.Client
}

// New returns a Client. baseURL is onegw's root (e.g.
// http://127.0.0.1:8080); combo is the routing combo name used as the
// wire `model` (e.g. "dev"). An empty key means no Authorization header.
func New(baseURL, key, combo string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		combo:   combo,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// Combo reports the combo this client routes through.
func (c *Client) Combo() string { return c.combo }

// Chat sends the messages to the bound combo and returns its answer.
// The caller's context bounds the call; there is no retry here, because
// the loop's own wall-clock and budget bounds are the retry policy.
func (c *Client) Chat(ctx context.Context, msgs ...Message) (Reply, error) {
	if c.combo == "" {
		return Reply{}, fmt.Errorf("onegw: no combo configured")
	}
	body, err := json.Marshal(map[string]any{"model": c.combo, "messages": msgs})
	if err != nil {
		return Reply{}, fmt.Errorf("onegw: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Reply{}, fmt.Errorf("onegw: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("onegw: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Reply{}, fmt.Errorf("onegw: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// onegw answers errors as {"error":{"type","message"}}; surface
		// the message rather than the whole envelope, and cap it so a
		// hostile body cannot flood a log or a run record.
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := string(raw)
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Type + ": " + e.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return Reply{}, fmt.Errorf("onegw: %s: %s", resp.Status, msg)
	}

	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Reply{}, fmt.Errorf("onegw: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Reply{}, fmt.Errorf("onegw: response had no choices")
	}
	return Reply{
		Content: out.Choices[0].Message.Content,
		Model:   out.Model,
		Usage:   out.Usage,
	}, nil
}
