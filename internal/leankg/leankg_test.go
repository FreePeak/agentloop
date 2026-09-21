package leankg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuery_SendsTheServersOwnContract(t *testing.T) {
	var gotPath string
	var gotBody Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"query":"parseConfig","limit":10,` +
			`"freshness":"fresh",` +
			`"retrieval":{"rung":"L1","reason":"exact identifier match"},` +
			`"hits":[{"qualified_name":"config.parseConfig","file_path":"config.go"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Query(context.Background(), Request{Query: "parseConfig", Limit: 10})
	if err != nil {
		t.Fatalf("Query() error: %v", err)
	}
	if gotPath != "/api/v1/query" {
		t.Errorf("path = %q, want /api/v1/query", gotPath)
	}
	if gotBody.Query != "parseConfig" || gotBody.Limit != 10 {
		t.Errorf("body = %+v, want the query and limit passed through", gotBody)
	}
	// An empty action must stay empty on the wire: it IS the ladder
	// request, not a missing field.
	if gotBody.Action != "" {
		t.Errorf("action = %q, want empty (the ladder router)", gotBody.Action)
	}
	if rung, reason := got.Rung(); rung != "L1" || reason != "exact identifier match" {
		t.Errorf("Rung() = %q/%q, want L1/exact identifier match", rung, reason)
	}
	if got.Freshness() != "fresh" {
		t.Errorf("Freshness() = %q, want fresh", got.Freshness())
	}
	if _, ok := got["hits"]; !ok {
		t.Error("hits missing from the response — the envelope must survive intact")
	}
}

// A deploy with no knowledge service has to fail loudly. Returning an
// empty hit list would read as "the graph has nothing", which is the
// silent-wrong-answer failure this whole service exists to prevent.
func TestQuery_NoBaseURLIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	c := New("")
	if _, err := c.Query(context.Background(), Request{Query: "anything"}); err == nil {
		t.Fatal("Query() with no base URL returned nil error")
	}
}

func TestQuery_EmptyQueryTextIsRejectedBeforeTheRoundTrip(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.Query(context.Background(), Request{}); err == nil {
		t.Fatal("Query() with no query text returned nil error")
	}
	if hit {
		t.Error("server was called despite an empty query — validate before the round trip")
	}
}

// LeanKG reports failures as a typed error envelope; the message has to
// reach the loop, not the whole body.
func TestQuery_ErrorEnvelopeBecomesAMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"unknown_action","message":"query action \"nope\" (valid: search, exact, …)"}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Query(context.Background(), Request{Query: "x", Action: "nope"})
	if err == nil {
		t.Fatal("Query() returned nil error on a 400")
	}
	if !contains(err.Error(), "unknown_action") && !contains(err.Error(), "valid: search") {
		t.Errorf("error = %q, want the server's message text", err.Error())
	}
}

// An answer with no retrieval block (memory/ontology/portfolio reads)
// reports an empty rung rather than a fabricated one.
func TestRung_AbsentOnNonIndexAnswers(t *testing.T) {
	var r Response
	if err := json.Unmarshal([]byte(`{"incidents":[],"query":{"service":"x"}}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rung, reason := r.Rung(); rung != "" || reason != "" {
		t.Errorf("Rung() = %q/%q, want empty on a non-index answer", rung, reason)
	}
	if r.Freshness() != "" {
		t.Errorf("Freshness() = %q, want empty", r.Freshness())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
