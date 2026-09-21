package xdev

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests drive a real child process speaking the real wire protocol. The
// fixture is a small Go program built per test, because the protocol's hard
// parts (ready-frame gating, event-before-response interleaving, a turn that
// never ends) are exactly the parts a mocked io.Reader would paper over.

const fakeSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
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
	switch os.Getenv("FAKE_MODE") {
	case "no-ready":
		return
	case "bad-proto":
		w("ready", "", map[string]any{"protocol": 99, "frameLimit": 1048576})
		return
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
		switch cmd.Type {
		case "prompt":
			switch os.Getenv("FAKE_MODE") {
			case "fail-turn":
				w("response", cmd.ID, map[string]any{"ok": false, "error": "model exploded"})
				continue
			case "stall":
				for i := 0; ; i++ {
					w("event", "", map[string]any{"type": "text_delta", "delta": fmt.Sprint(i)})
					time.Sleep(20 * time.Millisecond)
				}
			}
			w("event", "", map[string]any{"type": "toolcall_start", "toolCallId": "c1", "toolName": "write"})
			w("event", "", map[string]any{"type": "text_delta", "delta": "ok"})
			w("response", cmd.ID, map[string]any{
				"ok": true, "text": "did: " + cmd.Frame.Text,
				"stopReason": "stop", "sessionId": "sess-1", "model": "fake-model",
			})
		case "abort", "steer":
			w("response", cmd.ID, map[string]any{"ok": true})
		}
	}
}
`

func fakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-xdev")
	src := filepath.Join(dir, "fakexdev.go")
	if err := os.WriteFile(src, []byte(fakeSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fakexdev\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("build fake xdev: %v\n%s", err, out)
	}
	return bin
}

func start(t *testing.T, bin string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	c, err := Start(ctx, Config{Command: bin})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A child that dies without a ready frame must fail Start — not block a run
// forever waiting for a second line that will never come.
func TestStartRejectsChildThatNeverSaysReady(t *testing.T) {
	bin := fakeBinary(t)
	t.Setenv("FAKE_MODE", "no-ready")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{Command: bin})
	if err == nil {
		_ = c.Close()
		t.Fatal("Start() succeeded against a child that never sent ready")
	}
	if !strings.Contains(err.Error(), "ready") {
		t.Errorf("error = %q, want it to name the missing ready frame", err)
	}
}

// A wire version this client does not implement must fail loudly rather than
// mis-parse a future format.
func TestStartRejectsUnknownProtocolVersion(t *testing.T) {
	bin := fakeBinary(t)
	t.Setenv("FAKE_MODE", "bad-proto")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{Command: bin})
	if err == nil {
		_ = c.Close()
		t.Fatal("Start() accepted protocol 99")
	}
	if !strings.Contains(err.Error(), "protocol 99") {
		t.Errorf("error = %q, want it to name the version mismatch", err)
	}
}

// The core contract: one prompt, its response, and the events that streamed
// before it. A client that ignores events until the response would deadlock
// behind a full pipe on a long turn.
func TestPromptCorrelatesResponseAndKeepsEvents(t *testing.T) {
	c := start(t, fakeBinary(t))

	turn, err := c.Prompt(context.Background(), "fix the parser")
	if err != nil {
		t.Fatalf("Prompt() error: %v", err)
	}
	if turn.Text != "did: fix the parser" {
		t.Errorf("Text = %q, want the response text", turn.Text)
	}
	if turn.SessionID != "sess-1" || turn.Model != "fake-model" {
		t.Errorf("session/model = %q/%q, want sess-1/fake-model", turn.SessionID, turn.Model)
	}
	if turn.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", turn.StopReason)
	}
	if len(turn.Events) < 2 {
		t.Errorf("events = %d, want the streamed events retained", len(turn.Events))
	}
	if turn.Duration <= 0 {
		t.Error("Duration not recorded")
	}
}

// An ok:false response is an error carrying the child's reason, not an empty
// success — a failed step must be visible as a failed step.
func TestPromptRejectsFailedTurn(t *testing.T) {
	bin := fakeBinary(t)
	t.Setenv("FAKE_MODE", "fail-turn")
	c := start(t, bin)

	_, err := c.Prompt(context.Background(), "anything")
	if err == nil {
		t.Fatal("Prompt() returned nil error on an ok:false response")
	}
	if !strings.Contains(err.Error(), "model exploded") {
		t.Errorf("error = %q, want the child's reason", err)
	}
}

// A turn that outlives its budget must stop, and the child must be killed: a
// half-read stream plus a sandbox still mutating a workspace is the failure
// the step budget exists to prevent.
func TestPromptHonoursContextOnAStalledTurn(t *testing.T) {
	bin := fakeBinary(t)
	t.Setenv("FAKE_MODE", "stall")
	c := start(t, bin)

	turnCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.Prompt(turnCtx, "stall forever")
	if err == nil {
		t.Fatal("Prompt() returned nil error after its context expired")
	}
	if c.cmd.Process != nil {
		// The process must be gone, not merely abandoned.
		done := make(chan struct{})
		go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the child survived the turn timeout")
		}
	}
}

// A deploy with no xdev on PATH must report "no executor" so the loop can
// carry on without one, rather than panic or hang.
func TestCommandMissingFailsStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{Command: "/nonexistent/xdev-binary"})
	if err == nil {
		_ = c.Close()
		t.Fatal("Start() succeeded with a missing binary")
	}
}

// An empty prompt is rejected before the round trip: xdev's own server
// answers "prompt: text required", and a client that sends it is asking for
// a wasted turn.
func TestPromptRejectsEmptyText(t *testing.T) {
	c := start(t, fakeBinary(t))
	if _, err := c.Prompt(context.Background(), ""); err == nil {
		t.Fatal("Prompt(\"\") returned nil error")
	}
}
