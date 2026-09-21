package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/agentloop/internal/leankg"
	"github.com/FreePeak/agentloop/internal/xdev"
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

// `web_search` has no client. It must say so rather than return a success
// the loop cannot tell apart from real work.
func TestWebSearchAdvertisesItselfAsAStub(t *testing.T) {
	reg := NewRegistry()
	got, err := reg.Execute(context.Background(), "web_search", map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !contains(got.Message, "stub") {
		t.Errorf("Message = %q, want it to say stub", got.Message)
	}
}

// `run_tests` and `write_file` with no executor must report that, not a
// success. "No sandbox" and "the tests passed" must never look alike.
func TestSandboxToolsReportNoExecutor(t *testing.T) {
	reg := NewRegistry() // no sandbox client
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"run_tests", map[string]any{}, "no executor configured"},
		{"write_file", map[string]any{"path": "a.txt", "content": "x"}, "no executor configured"},
	}
	for _, c := range cases {
		got, err := reg.Execute(context.Background(), c.name, c.args)
		if err != nil {
			t.Fatalf("%s: Execute() error: %v", c.name, err)
		}
		if !contains(got.Message, c.want) {
			t.Errorf("%s: Message = %q, want it to mention %q", c.name, got.Message, c.want)
		}
		if written, ok := got.Data["written"].(bool); ok && written {
			t.Errorf("%s: Data[written] = true with no executor", c.name)
		}
		if ran, ok := got.Data["ran"].(bool); ok && ran {
			t.Errorf("%s: Data[ran] = true with no executor", c.name)
		}
	}
}

// A write with no target fails closed: a run must not scribble on a
// workspace it cannot name.
func TestWriteFileRequiresAPath(t *testing.T) {
	reg := NewRegistry()
	got, err := reg.Execute(context.Background(), "write_file", map[string]any{"content": "x"})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if got.Success {
		t.Error("write_file with no path reported success")
	}
	if !contains(got.Message, "path") {
		t.Errorf("Message = %q, want it to name the missing path", got.Message)
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

// With a sandbox client, `write_file` and `run_tests` become real turns.
// The fixture is a process speaking xdev's wire protocol, so this asserts
// the whole path: registry -> client -> child -> result.
func TestSandboxToolsRunRealTurns(t *testing.T) {
	bin := xdevFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := xdev.Start(ctx, xdev.Config{Command: bin})
	if err != nil {
		t.Fatalf("xdev.Start() error: %v", err)
	}
	defer c.Close()

	reg := NewRegistry().WithSandbox(c, t.TempDir())

	// write_file: the prompt must carry the path and the content verbatim.
	got, err := reg.Execute(ctx, "write_file", map[string]any{"path": "b.txt", "content": "hello"})
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if !got.Success {
		t.Fatalf("write_file failed: %s", got.Message)
	}
	if written, _ := got.Data["written"].(bool); !written {
		t.Errorf("write_file: written = %v, want true with a live sandbox", got.Data["written"])
	}
	if got.Metadata["sandbox"] != "xdev" {
		t.Errorf("write_file metadata = %v, want sandbox=xdev", got.Metadata)
	}

	// run_tests: same path, different prompt.
	got, err = reg.Execute(ctx, "run_tests", map[string]any{})
	if err != nil {
		t.Fatalf("run_tests: %v", err)
	}
	if ran, _ := got.Data["ran"].(bool); !ran {
		t.Errorf("run_tests: ran = %v, want true with a live sandbox", got.Data["ran"])
	}
	if !contains(got.Message, "did:") {
		t.Errorf("run_tests Message = %q, want the turn's report", got.Message)
	}
}

// A sandbox turn that fails is an observation with the reason recorded —
// not a hard error that would abort the run.
func TestSandboxTurnFailureIsAnObservation(t *testing.T) {
	bin := xdevFixture(t)
	t.Setenv("FAKE_MODE", "fail-turn")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := xdev.Start(ctx, xdev.Config{Command: bin})
	if err != nil {
		t.Fatalf("xdev.Start() error: %v", err)
	}
	defer c.Close()

	reg := NewRegistry().WithSandbox(c, t.TempDir())
	got, err := reg.Execute(ctx, "run_tests", map[string]any{})
	if err != nil {
		t.Fatalf("Execute() returned a hard error: %v (it must be an observation)", err)
	}
	if got.Success {
		t.Error("Success = true on a failed turn")
	}
	if !contains(got.Message, "model exploded") {
		t.Errorf("Message = %q, want the child's reason", got.Message)
	}
}

// xdevFixture builds the same wire-protocol child internal/xdev's tests
// use, so this package's tests exercise the real client against a real
// process rather than a stub.
func xdevFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-xdev")
	src := filepath.Join(dir, "fakexdev.go")
	if err := os.WriteFile(src, []byte(fakeXdevSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fakexdev\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake xdev: %v\n%s", err, out)
	}
	return bin
}

const fakeXdevSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	w := func(typ, id string, payload any) {
		b, _ := json.Marshal(payload)
		env := map[string]any{"type": typ, "id": id, "frame": json.RawMessage(b)}
		line, _ := json.Marshal(env)
		out.Write(append(line, '\n'))
		out.Flush()
	}
	w("ready", "", map[string]any{"protocol": 1, "frameLimit": 1048576})
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var cmd struct {
			Type  string ` + "`json:\"type\"`" + `
			ID    string ` + "`json:\"id\"`" + `
			Frame struct {
				Text string ` + "`json:\"text\"`" + `
			} ` + "`json:\"frame\"`" + `
		}
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			continue
		}
		if cmd.Type != "prompt" {
			continue
		}
		if os.Getenv("FAKE_MODE") == "fail-turn" {
			w("response", cmd.ID, map[string]any{"ok": false, "error": "model exploded"})
			continue
		}
		w("event", "", map[string]any{"type": "text_delta", "delta": "ok"})
		w("response", cmd.ID, map[string]any{
			"ok": true, "text": "did: " + cmd.Frame.Text,
			"stopReason": "stop", "sessionId": "s1", "model": "fake",
		})
	}
}
`
