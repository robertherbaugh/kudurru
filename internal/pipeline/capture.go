package pipeline

import (
	"errors"
	"sync"
)

// ErrServingCapture rejects incomplete request captures. Stages must never fall
// back to mutable sources when a request has a Serving lease.
var ErrServingCapture = errors.New("pipeline: serving capture unavailable")

// ServingLease owns immutable configuration resources. Value is opaque here;
// typed owner adapters interpret it. Release is idempotent. Do not race use of
// Value against Release. Clone creates independent ownership for detached work.
type ServingLease interface {
	Value() any
	Clone() (ServingLease, error)
	Release()
}

// Capture acquires a complete cut before the first stage. Ownership transfers
// to Chain even when Capture returns both a lease and an error.
type Capture func(*Request) (ServingLease, error)

// SetCapture installs the single request capture hook. Like Observe, configure
// it before serving concurrent requests. Nil keeps legacy owner behavior.
func (c *Chain) SetCapture(capture Capture) { c.capture = capture }

type responseCleanup struct {
	mu     sync.Mutex
	closed bool
	hooks  []func()
}

// AddCleanup adds resource ownership to the response. Call during construction,
// before returning it to another goroutine. Shallow copies share existing
// cleanup state. Adding to already closed state runs the hook immediately.
func (r *Response) AddCleanup(hook func()) {
	if r == nil || hook == nil {
		return
	}
	if r.cleanup == nil {
		r.cleanup = &responseCleanup{}
	}
	c := r.cleanup
	c.mu.Lock()
	if !c.closed {
		c.hooks = append(c.hooks, hook)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	runCleanup(hook)
}

// Close releases response resources even when Transfer is never iterated or a
// write fails. Every transport must defer Close. It is nil-safe and idempotent,
// including across shallow copies. It must not race active response/stream use.
func (r *Response) Close() {
	if r == nil || r.cleanup == nil {
		return
	}
	c := r.cleanup
	c.mu.Lock()
	hooks := c.hooks
	c.hooks, c.closed = nil, true
	c.mu.Unlock()
	for _, hook := range hooks {
		runCleanup(hook)
	}
}

func runCleanup(hook func()) {
	defer func() { _ = recover() }()
	hook()
}
