package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FreePeak/agentloop/internal/leankg"
)

// The registry must carry LeanKG's own envelope back to the loop, and
// the rung that answered in Metadata — the whole point of the tool is
// that the loop can see which retrieval layer held.
func TestQuery_ReachesLeanKGAndReportsTheRung(t *testing.T) {
	var gotAction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body leankg.Request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		gotAction = body.Action
		w.Write([]byte(`{"query":"parseConfig","freshness":"fresh",` +
			`"retrieval":{"rung":"L2","reason":"keyword"},` +
			`"hits":[{"qualified_name":"config.parseConfig"}]}`))
	}))
	defer srv.Close()

	reg := NewRegistryWithKnowledge(leankg.New(srv.URL))
	got, err := reg.Execute(context.Background(), "query", map[string]any{
		"query": "parseConfig", "action": "fuzzy",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !got.Success {
		t.Fatalf("Success = false: %s", got.Message)
	}
	if gotAction != "fuzzy" {
		t.Errorf("action on the wire = %q, want fuzzy", gotAction)
	}
	if got.Metadata["retrieval_rung"] != "L2" {
		t.Errorf("retrieval_rung = %q, want L2", got.Metadata["retrieval_rung"])
	}
	if got.Metadata["freshness"] != "fresh" {
		t.Errorf("freshness = %q, want fresh", got.Metadata["freshness"])
	}
	// The server's payload must survive unflattened.
	hits, ok := got.Data["hits"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("Data[hits] = %#v, want the server's single hit", got.Data["hits"])
	}
}

// A run with no knowledge service must say so, not answer with an empty
// hit list that reads like "the graph has nothing".
func TestQuery_WithoutAKnowledgeServiceSaysSo(t *testing.T) {
	reg := NewRegistry()
	got, err := reg.Execute(context.Background(), "query", map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !contains(got.Message, "no knowledge service") {
		t.Errorf("Message = %q, want it to name the missing configuration", got.Message)
	}
}

// A LeanKG outage is an observation, not a panic and not a hard tool
// error: the loop records the step and keeps its bounds.
func TestQuery_LeanKGDownBecomesAnObservation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"database is locked"}`))
	}))
	defer srv.Close()

	reg := NewRegistryWithKnowledge(leankg.New(srv.URL))
	got, err := reg.Execute(context.Background(), "query", map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("Execute() returned a hard error: %v (it should be an observation)", err)
	}
	if got.Success {
		t.Error("Success = true on a 503")
	}
	if !contains(got.Message, "database is locked") {
		t.Errorf("Message = %q, want the server's reason", got.Message)
	}
}

// The planner's default args carry the goal, not a query string. Falling
// back to the goal beats sending an empty query the server rejects.
func TestQuery_FallsBackToTheGoalForQueryText(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body leankg.Request
		json.NewDecoder(r.Body).Decode(&body)
		gotQuery = body.Query
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	reg := NewRegistryWithKnowledge(leankg.New(srv.URL))
	if _, err := reg.Execute(context.Background(), "query", map[string]any{"goal": "explore the repository"}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotQuery != "explore the repository" {
		t.Errorf("query on the wire = %q, want the goal", gotQuery)
	}
}

// The other three tools are still stubs. They must say so rather than
// return a success the loop cannot tell apart from real work.
func TestStubsAdvertiseThemselves(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"web_search", "run_tests", "write_file"} {
		got, err := reg.Execute(context.Background(), name, map[string]any{})
		if err != nil {
			t.Fatalf("%s: Execute() error: %v", name, err)
		}
		if !contains(got.Message, "stub") {
			t.Errorf("%s: Message = %q, want it to say stub", name, got.Message)
		}
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
