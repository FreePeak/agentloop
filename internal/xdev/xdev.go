// Package xdev is agentloop's execution client: the JSONL-over-stdio
// protocol xdev exposes for embedders (`xdev rpc`).
//
// The separation of powers is the whole point of this package existing at
// all (PRD §4.3): agentloop owns the loop, the bounds and the approval
// gate; **xdev owns the turn** — the sandbox, the file mutation, the test
// run, the model call inside a step. agentloop drives xdev as a tool
// executor for ONE already-planned step and never hands it an unbounded
// goal. Loop count stays 1.
//
// Wire shape (xdev internal/protocol, v1):
//
//	{"type":"ready","frame":{"protocol":1,"frameLimit":1048576}}
//	-> {"type":"prompt","id":"<id>","text":"<prompt>"}
//	<- {"type":"event","frame":{...}}            (zero or more, streamed)
//	<- {"type":"response","id":"<id>","frame":{"ok":true,"text":"...",...}}
//
// Reads are sequential and writes are serialized, which matters because a
// long turn streams events before its response: a client that only reads
// the response deadlocks behind a full pipe (xdev's own Serve comment
// names this).
package xdev

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol constants, mirrored from xdev's internal/protocol. Duplicated
// deliberately: agentloop must not import xdev's module, and a wire
// version is a contract, not a shared type. The ready frame is validated
// against ProtocolVersion so a future v2 fails loudly instead of
// mis-parsing.
const (
	ProtocolVersion = 1
	// FrameLimit caps one JSONL frame. xdev advertises its own in ready;
	// this is the client-side ceiling used when deciding to truncate a
	// prompt before sending it.
	FrameLimit = 1 << 20
)

// Frame types.
const (
	typeReady    = "ready"
	typePrompt   = "prompt"
	typeSteer    = "steer"
	typeAbort    = "abort"
	typeResponse = "response"
	typeEvent    = "event"
)

// frame is the envelope every line carries, in both directions.
type frame struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Frame json.RawMessage `json:"frame,omitempty"`
}

// readyFrame is xdev's first frame on the stream.
type readyFrame struct {
	Protocol   int `json:"protocol"`
	FrameLimit int `json:"frameLimit"`
}

// responseFrame answers one prompt by id.
type responseFrame struct {
	OK         bool   `json:"ok"`
	Err        string `json:"error,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Model      string `json:"model,omitempty"`
	Text       string `json:"text,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

// eventFrame is one streamed event. Only the fields a loop needs to
// observe are decoded; the rest are ignored on purpose — agentloop is not
// a UI.
type eventFrame struct {
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Delta      string `json:"delta,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

// Turn is the outcome of one prompt.
type Turn struct {
	Text       string        // the assistant's final text
	StopReason string        // xdev's stop reason, passed through
	SessionID  string        // set once a session exists
	Model      string        // the model xdev used, as it reports it
	Events     []eventFrame  // the stream, for the trace
	Duration   time.Duration // wall-clock for the turn
	EventLimit bool          // true if Events was truncated at MaxEvents
}

// Client drives one xdev process over stdio.
//
// It is deliberately single-turn-at-a-time: xdev's own server refuses a
// second prompt while one is running ("send steer/follow_up instead"), so
// serialising here turns a server-side error into a client-side wait
// rather than a failed step.
type Client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader

	mu     sync.Mutex // one turn at a time, and one writer at a time
	seq    atomic.Uint64
	closed atomic.Bool

	// MaxEvents caps how much of a turn's stream is retained on the Turn.
	// The trace wants the shape, not every token; a run that streams a
	// megabyte of deltas must not become a megabyte of run record.
	MaxEvents int
}

// Config describes how to start the child.
type Config struct {
	// Command is the xdev binary. Default "xdev".
	Command string
	// Args are extra arguments after the subcommand. `rpc` is always added.
	Args []string
	// Dir is the working directory a turn runs in. This is the sandbox
	// boundary agentloop actually controls: xdev's own --add-dir
	// restriction is applied inside it.
	Dir string
	// Env is the child environment (nil = inherit).
	Env []string
}

// Start launches `xdev rpc` and waits for its ready frame.
//
// The ready frame is a hard gate: if the child does not speak the protocol
// version this client implements, Start fails and the loop is told no
// executor is available — better a step that reports "no sandbox" than one
// that mis-parses a future wire format.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Command == "" {
		cfg.Command = "xdev"
	}
	args := append([]string{"rpc"}, cfg.Args...)
	cmd := exec.CommandContext(ctx, cfg.Command, args...)
	cmd.Dir = cfg.Dir
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("xdev: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("xdev: stdout pipe: %w", err)
	}
	// The child's stderr is kept off the protocol stream but not thrown
	// away: a client that swallows it cannot explain a failed start.
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("xdev: start %s: %w", cfg.Command, err)
	}

	c := &Client{cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, FrameLimit), MaxEvents: 200}

	// The ready frame: the child's first line, always (xdev Serve writes it
	// before dispatching anything).
	line, err := c.readLine()
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("xdev: no ready frame: %w (stderr: %s)", err, strings.TrimSpace(errBuf.String()))
	}
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("xdev: ready frame is not JSONL: %w", err)
	}
	if f.Type != typeReady {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("xdev: first frame was %q, want %q", f.Type, typeReady)
	}
	var rf readyFrame
	if err := json.Unmarshal(f.Frame, &rf); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("xdev: decode ready: %w", err)
	}
	if rf.Protocol != ProtocolVersion {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("xdev: protocol %d, this client speaks %d", rf.Protocol, ProtocolVersion)
	}
	return c, nil
}

// Close terminates the child and waits for it.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

// Prompt runs one turn and returns its outcome.
//
// ctx bounds the turn: if it expires the client kills the child rather
// than leaving a half-read stream behind, because a turn that outlived its
// step budget has no business continuing to mutate a workspace.
func (c *Client) Prompt(ctx context.Context, text string) (Turn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed.Load() {
		return Turn{}, fmt.Errorf("xdev: client closed")
	}
	if text == "" {
		return Turn{}, fmt.Errorf("xdev: prompt text required")
	}
	id := fmt.Sprintf("al-%d", c.seq.Add(1))

	start := time.Now()
	if err := c.writeFrame(typePrompt, id, map[string]any{"text": text}); err != nil {
		return Turn{}, err
	}

	turn := Turn{}
	// Read until this prompt's response. Events interleave and are kept
	// (bounded) because the trace wants the shape of the turn.
	for {
		select {
		case <-ctx.Done():
			// Kill rather than wait: the step's budget is gone.
			_ = c.cmd.Process.Kill()
			return turn, fmt.Errorf("xdev: turn %s: %w", id, ctx.Err())
		default:
		}

		line, err := c.readLine()
		if err != nil {
			return turn, fmt.Errorf("xdev: read after %s: %w", id, err)
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue // a malformed line is not worth failing a turn over
		}
		switch f.Type {
		case typeEvent:
			var ev eventFrame
			if json.Unmarshal(f.Frame, &ev) == nil && len(turn.Events) < c.MaxEvents {
				turn.Events = append(turn.Events, ev)
			} else if len(turn.Events) >= c.MaxEvents {
				turn.EventLimit = true
			}
		case typeResponse:
			// A response for a different id is a protocol violation this
			// client cannot act on; ignore it and keep reading for ours.
			if f.ID != id {
				continue
			}
			var resp responseFrame
			if err := json.Unmarshal(f.Frame, &resp); err != nil {
				return turn, fmt.Errorf("xdev: decode response %s: %w", id, err)
			}
			turn.Duration = time.Since(start)
			turn.SessionID, turn.Model, turn.StopReason = resp.SessionID, resp.Model, resp.StopReason
			if !resp.OK {
				return turn, fmt.Errorf("xdev: turn failed: %s", resp.Err)
			}
			turn.Text = resp.Text
			return turn, nil
		}
	}
}

// Steer injects a message into the running turn. Not used by the loop
// yet; present because the protocol has it and a future step that watches
// a long turn will want it.
func (c *Client) Steer(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := fmt.Sprintf("al-steer-%d", c.seq.Add(1))
	if err := c.writeFrame(typeSteer, id, map[string]any{"text": text}); err != nil {
		return err
	}
	return c.awaitOK(id)
}

// Abort cancels the running turn.
func (c *Client) Abort(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := fmt.Sprintf("al-abort-%d", c.seq.Add(1))
	if err := c.writeFrame(typeAbort, id, nil); err != nil {
		return err
	}
	return c.awaitOK(id)
}

// awaitOK reads until the response with this id (skipping events).
func (c *Client) awaitOK(id string) error {
	for {
		line, err := c.readLine()
		if err != nil {
			return fmt.Errorf("xdev: read after %s: %w", id, err)
		}
		var f frame
		if json.Unmarshal(line, &f) != nil || f.Type != typeResponse || f.ID != id {
			continue
		}
		var resp responseFrame
		if err := json.Unmarshal(f.Frame, &resp); err != nil {
			return fmt.Errorf("xdev: decode response %s: %w", id, err)
		}
		if !resp.OK {
			return fmt.Errorf("xdev: %s: %s", id, resp.Err)
		}
		return nil
	}
}

func (c *Client) writeFrame(typ, id string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("xdev: encode %s: %w", typ, err)
		}
		raw = b
	}
	body, err := json.Marshal(frame{Type: typ, ID: id, Frame: raw})
	if err != nil {
		return fmt.Errorf("xdev: encode frame: %w", err)
	}
	body = append(body, '\n')
	if len(body) > FrameLimit {
		return fmt.Errorf("xdev: frame is %d bytes, limit is %d", len(body), FrameLimit)
	}
	if _, err := c.stdin.Write(body); err != nil {
		return fmt.Errorf("xdev: write %s: %w", typ, err)
	}
	return nil
}

// readLine reads one frame line, bounding it so a hostile or broken child
// cannot exhaust memory.
func (c *Client) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := c.out.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			return buf, nil
		}
		if err == bufio.ErrBufferFull {
			if len(buf) > FrameLimit {
				return nil, fmt.Errorf("xdev: frame exceeds %d bytes", FrameLimit)
			}
			continue
		}
		return nil, err
	}
}
