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
	// Combo is the combo this call was routed to; Model is the leg that
	// actually answered. They differ whenever onegw falls back, which is
	// exactly the event tiering has to be judged on.
	Combo string
	Model string
	Usage Usage
}

// Client talks to one onegw instance.
//
// It holds a DEFAULT combo, not a single one: the loop routes per step
// (PRD §13.1 move 3 — "route models by step type"), so the combo has to be
// choosable per call. Sending one combo forever made tiering a decorative
// field on the run record.
type Client struct {
	baseURL string
	key     string
	combo   string
	http    *http.Client

	// combos maps a tier name to the combo that serves it. When a tier is
	// absent the default combo is used, so an operator who has not set up
	// several combos still gets a working loop rather than an error.
	combos map[string]string
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

// Combo reports the default combo.
func (c *Client) Combo() string { return c.combo }

// WithTiers returns a copy of the client whose calls naming a tier route
// to that tier's combo. Nil or an empty map leaves the default in force.
//
// This is the whole of agentloop's tier routing: it picks the combo, onegw
// picks the leg behind it (§4.3, "agentloop sends the tier per step; onegw
// picks the leg").
func (c *Client) WithTiers(tiers map[string]string) *Client {
	out := *c
	out.combos = tiers
	return &out
}

// comboFor resolves the combo for a tier, falling back to the default so a
// missing mapping degrades to "the loop still runs" rather than a failed
// step.
func (c *Client) comboFor(tier string) string {
	if tier != "" && c.combos != nil {
		if combo, ok := c.combos[tier]; ok && combo != "" {
			return combo
		}
	}
	return c.combo
}

// Chat sends the messages to the default combo and returns its answer.
// The caller's context bounds the call; there is no retry here, because
// the loop's own wall-clock and budget bounds are the retry policy.
func (c *Client) Chat(ctx context.Context, msgs ...Message) (Reply, error) {
	return c.ChatTier(ctx, "", msgs...)
}

// ChatTier is Chat, routed by tier. An unknown or empty tier uses the
// default combo, so a misconfigured tier costs the wrong model, not a
// failed run.
func (c *Client) ChatTier(ctx context.Context, tier string, msgs ...Message) (Reply, error) {
	combo := c.comboFor(tier)
	if combo == "" {
		return Reply{}, fmt.Errorf("onegw: no combo configured")
	}
	body, err := json.Marshal(map[string]any{"model": combo, "messages": msgs})
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
	defer func() { _ = resp.Body.Close() }()

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
		// Combo is what we ASKED for; Model is the leg that answered.
		// Recording both is what makes "did tiering work?" answerable.
		Combo: combo,
		Usage: out.Usage,
	}, nil
}
