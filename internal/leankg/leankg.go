// Package leankg is agentloop's read client for the code graph.
//
// It is the same shape as internal/onegw on purpose: one POST, no retry
// ladder, no caching, because LeanKG owns the ladder, the ranking and
// freshness, and agentloop owns policy (PRD §3.1, §4.3 — "Knowledge
// retrieval + memory | LeanKG | the only place that owns the code graph").
//
// The tool contract mirrors the server's, one endpoint and one ladder:
//
//	POST /api/v1/query {action, query, limit, args}
//
// Empty action runs LeanKG's L1→L3 router; an action pins a rung or asks
// a graph verb. agentloop does not re-implement any of that — it sends the
// request and reports the rung that answered.
package leankg

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

// Request is the query tool's wire body (leankg internal/core.QueryRequest).
type Request struct {
	Action string         `json:"action,omitempty"` // "" = the L1→L3 ladder router
	Query  string         `json:"query"`
	Limit  int            `json:"limit,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
}

// Response is LeanKG's answer, kept as the decoded envelope so the tool
// never invents a shape the server did not send. The keys agentloop reads
// are the ones the server documents: retrieval{rung,reason}, freshness,
// and the hits/matches payload for whichever action answered.
type Response map[string]any

// Client talks to one LeanKG instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a Client. baseURL is LeanKG's root (e.g.
// http://127.0.0.1:8090). The timeout bounds one query; the loop's own
// wall-clock and step bounds stay the outer policy.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Query runs one request and returns the decoded answer.
//
// An empty baseURL is a configuration error, not an empty result: a run
// with no knowledge service must fail loudly rather than report "no hits",
// which would read as "the graph has nothing" (P100 honest degradation).
func (c *Client) Query(ctx context.Context, req Request) (Response, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("leankg: no base URL configured")
	}
	if req.Query == "" {
		return nil, fmt.Errorf("leankg: query text required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("leankg: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("leankg: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("leankg: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("leankg: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// LeanKG answers errors as errs.Error; surface the message rather
		// than the envelope, capped so a hostile body cannot flood a run
		// record (same rule as the onegw client).
		var e struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		msg := string(raw)
		if json.Unmarshal(raw, &e) == nil {
			if e.Message != "" {
				msg = e.Message
			} else if e.Error != "" {
				msg = e.Error
			}
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("leankg: %s: %s", resp.Status, msg)
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("leankg: decode response: %w", err)
	}
	return out, nil
}

// Rung reports the retrieval layer that answered, and why, from a
// Response. LeanKG sets retrieval{rung,reason} on every index-backed
// answer; memory/ontology/portfolio reads do not, and report "".
func (r Response) Rung() (rung, reason string) {
	ret, _ := r["retrieval"].(map[string]any)
	if ret == nil {
		return "", ""
	}
	rung, _ = ret["rung"].(string)
	reason, _ = ret["reason"].(string)
	return rung, reason
}

// Freshness reports the graph's freshness stamp, or "" when the action
// that answered does not carry one.
func (r Response) Freshness() string {
	f, _ := r["freshness"].(string)
	return f
}
