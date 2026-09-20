package supervisor

import (
	"fmt"
	"sync"
	"time"
)

// MessageKind is the typed topic of a coordination message
// (design.md §7: messages task|result|question|feedback).
// Typed, not free text — an untyped bus is how coordination cost
// escapes the 30% budget without anyone noticing.
type MessageKind string

const (
	MsgTask     MessageKind = "task"     // supervisor → specialist: work to do
	MsgResult   MessageKind = "result"   // specialist → supervisor: outcome
	MsgQuestion MessageKind = "question" // specialist → supervisor: blocked
	MsgFeedback MessageKind = "feedback" // supervisor → specialist: revise
)

// AllMessageKinds lists every kind in declaration order. Used by
// tests to prove the enum is exhaustive.
func AllMessageKinds() []MessageKind {
	return []MessageKind{MsgTask, MsgResult, MsgQuestion, MsgFeedback}
}

// Valid reports whether k is a known message kind. The bus rejects
// unknown kinds: a schema with a hole is not a schema.
func (k MessageKind) Valid() bool {
	switch k {
	case MsgTask, MsgResult, MsgQuestion, MsgFeedback:
		return true
	}
	return false
}

// Message is one typed coordination message. Tokens is the payload
// size in tokens, counted so the 30% coordination budget can be
// enforced as arithmetic rather than estimated after the fact.
type Message struct {
	Kind    MessageKind `json:"kind"`
	From    string      `json:"from"`
	To      string      `json:"to"`
	Topic   string      `json:"topic,omitempty"` // subtask id / thread key
	Payload string      `json:"payload"`
	Tokens  int         `json:"tokens"`
	SentAt  time.Time   `json:"sent_at"`
}

// Bus is the typed message bus (P58). Every send is logged — the
// log is the observability surface and the coordination-spend
// ledger at once. Per-receiver FIFO: a specialist reads its own
// messages in the order the supervisor sent them.
type Bus struct {
	mu      sync.Mutex
	queues  map[string][]Message // receiver → FIFO queue
	log     []Message            // every send, in order
	tokens  int                  // coordination tokens spent so far
	sentBy  map[MessageKind]int  // kind → count
	nowFunc func() time.Time     // overridable for tests
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{
		queues:  make(map[string][]Message),
		sentBy:  make(map[MessageKind]int),
		nowFunc: time.Now,
	}
}

// Send enqueues one message for its receiver. Fails closed on an
// unknown kind, an empty receiver, or a non-positive token count —
// an uncounted message cannot be budgeted.
func (b *Bus) Send(m Message) error {
	if !m.Kind.Valid() {
		return fmt.Errorf("bus: unknown message kind %q", m.Kind)
	}
	if m.To == "" {
		return fmt.Errorf("bus: receiver required")
	}
	if m.Tokens <= 0 {
		return fmt.Errorf("bus: token count must be positive (uncounted messages are unbudgetable)")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.SentAt.IsZero() {
		m.SentAt = b.nowFunc()
	}
	b.queues[m.To] = append(b.queues[m.To], m)
	b.log = append(b.log, m)
	b.tokens += m.Tokens
	b.sentBy[m.Kind]++
	return nil
}

// Receive pops the oldest message for a receiver (FIFO). ok=false
// means the queue is empty.
func (b *Bus) Receive(receiver string) (Message, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queues[receiver]
	if len(q) == 0 {
		return Message{}, false
	}
	m := q[0]
	b.queues[receiver] = q[1:]
	return m, true
}

// Len returns the pending message count for a receiver.
func (b *Bus) Len(receiver string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queues[receiver])
}

// Log returns a copy of every send, in order (observability).
func (b *Bus) Log() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Message, len(b.log))
	copy(out, b.log)
	return out
}

// CoordinationTokens returns the total tokens spent on coordination.
func (b *Bus) CoordinationTokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

// CountByKind returns how many messages of a kind were sent.
func (b *Bus) CountByKind(k MessageKind) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sentBy[k]
}

// TestClock sets the bus clock for tests; defer TestClock(time.Now).
func (b *Bus) TestClock(clock func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nowFunc = clock
}
