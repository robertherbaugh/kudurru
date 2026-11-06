// Package retirement provides bounded local exclusion and monotone mechanical
// comparisons. It authenticates no source, accepts no durable state, and grants
// no serving, transport, writer, or recovery authority. A future private owner
// must supply all independent original-proof, durable, and runtime admission.
package retirement

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Fixed bounds reserve writer admission independently of pending readers.
const (
	MaxReaders        = 256
	MaxPendingReaders = 240
	MaxPendingWriters = 16
	MaxLifetime       = 30 * time.Second
)

// Refusals contain no caller labels, proofs, or received data.
var (
	ErrInvalid      = errors.New("retirement: invalid input")
	ErrCapacity     = errors.New("retirement: capacity exceeded")
	ErrUsed         = errors.New("retirement: attempt or plan already used")
	ErrScope        = errors.New("retirement: inactive or foreign scope")
	ErrRetired      = errors.New("retirement: retired registration")
	ErrMismatch     = errors.New("retirement: original owner mismatch")
	ErrClosed       = errors.New("retirement: admission closed")
	errOriginalDone = errors.New("retirement: original context stopped")
)

// Gate owns bounded queue bookkeeping separately from its immutable CAS cursor.
// The zero value refuses; copying a Gate after construction is unsupported.
type Gate struct {
	history History
	state   atomic.Pointer[gateState]
	mu      sync.Mutex
	readers int
	writer  bool
	readQ   [MaxPendingReaders]*waiter
	readN   int
	writeQ  [MaxPendingWriters]*waiter
	writeN  int
}

type waiter struct {
	write   bool
	life    *lifetime
	ready   chan struct{}
	granted bool
	err     error
}

// New constructs no usable registration, goroutine, timer, or durable authority.
// A retained floor requires its exact already-loaded domain. New is not reset.
func New(history History, restoredDomain [32]byte, retiredThrough uint64) (*Gate, error) {
	if !validHistory(history) || (retiredThrough != 0 && restoredDomain == ([32]byte{})) {
		return nil, ErrInvalid
	}
	g := &Gate{history: history}
	g.state.Store(&gateState{domain: restoredDomain, floor: retiredThrough})
	return g, nil
}

func (g *Gate) usable() bool { return g != nil && g.state.Load() != nil }

// Close is terminal local refusal, not durable denial or forced lock release.
// Existing callbacks must finish before their counted holds can be released.
func (g *Gate) Close() {
	if !g.usable() {
		return
	}
	for {
		old := g.state.Load()
		if old.closed {
			break
		}
		next := *old
		next.closed, next.current = true, nil
		if g.state.CompareAndSwap(old, &next) {
			break
		}
	}
	g.mu.Lock()
	g.dispatchLocked()
	g.mu.Unlock()
}

// enter calls no context interface, function supplied by a caller, or I/O while
// holding mu. Only cached lifetime signals, actual time and bounded queues run.
func (g *Gate) enter(write bool, life *lifetime) error {
	node := &waiter{write: write, life: life, ready: make(chan struct{})}
	g.mu.Lock()
	if g.state.Load().closed {
		g.mu.Unlock()
		return ErrClosed
	}
	if err := life.check(); err != nil {
		g.mu.Unlock()
		return err
	}
	g.dispatchLocked()
	if write && !g.writer && g.readers == 0 && g.writeN == 0 {
		g.writer, node.granted = true, true
	} else if !write && !g.writer && g.writeN == 0 && g.readers < MaxReaders && g.readN == 0 {
		g.readers++
		node.granted = true
	} else if write {
		if g.writeN == MaxPendingWriters {
			g.mu.Unlock()
			return ErrCapacity
		}
		g.writeQ[g.writeN] = node
		g.writeN++
	} else {
		if g.readN == MaxPendingReaders {
			g.mu.Unlock()
			return ErrCapacity
		}
		g.readQ[g.readN] = node
		g.readN++
	}
	if node.granted {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	timer := time.NewTimer(max(0, time.Until(life.until)))
	defer timer.Stop()
	select {
	case <-node.ready:
	case <-life.done:
	case <-timer.C:
	}
	g.mu.Lock()
	err := node.err
	if err == nil {
		err = life.check()
	}
	if err == nil && g.state.Load().closed {
		err = ErrClosed
	}
	if err != nil && node.granted {
		g.releaseLocked(write)
		node.granted = false
	} else if !node.granted {
		g.removeLocked(node)
		if err == nil {
			err = ErrScope
		}
	}
	g.dispatchLocked()
	g.mu.Unlock()
	return err
}

func (g *Gate) removeLocked(node *waiter) {
	if node.write {
		for i := 0; i < g.writeN; i++ {
			if g.writeQ[i] == node {
				copy(g.writeQ[i:], g.writeQ[i+1:g.writeN])
				g.writeN--
				g.writeQ[g.writeN] = nil
				return
			}
		}
	} else {
		for i := 0; i < g.readN; i++ {
			if g.readQ[i] == node {
				copy(g.readQ[i:], g.readQ[i+1:g.readN])
				g.readN--
				g.readQ[g.readN] = nil
				return
			}
		}
	}
}

func prune(queue []*waiter, closed bool) int {
	kept := 0
	for _, node := range queue {
		err := node.life.check()
		if closed {
			err = ErrClosed
		}
		if err != nil {
			node.err = err
			close(node.ready)
		} else {
			queue[kept] = node
			kept++
		}
	}
	clear(queue[kept:])
	return kept
}

func (g *Gate) dispatchLocked() {
	closed := g.state.Load().closed
	g.writeN = prune(g.writeQ[:g.writeN], closed)
	g.readN = prune(g.readQ[:g.readN], closed)
	if closed || g.writer {
		return
	}
	if g.writeN != 0 {
		if g.readers == 0 {
			node := g.writeQ[0]
			copy(g.writeQ[:], g.writeQ[1:g.writeN])
			g.writeN--
			g.writeQ[g.writeN] = nil
			g.writer, node.granted = true, true
			close(node.ready)
		}
		return
	}
	for g.readN != 0 && g.readers < MaxReaders {
		node := g.readQ[0]
		copy(g.readQ[:], g.readQ[1:g.readN])
		g.readN--
		g.readQ[g.readN] = nil
		g.readers++
		node.granted = true
		close(node.ready)
	}
}

func (g *Gate) releaseLocked(write bool) {
	if write {
		g.writer = false
	} else {
		g.readers--
	}
}

func (g *Gate) release(write bool) {
	g.mu.Lock()
	g.releaseLocked(write)
	g.dispatchLocked()
	g.mu.Unlock()
}

func stopped(err error, life *lifetime) error {
	// This is intentionally OUTSIDE queue, scope mutation and final hooks.
	// A custom context may classify Done as either original cancellation kind.
	if errors.Is(err, errOriginalDone) {
		if original := life.original.Err(); original != nil {
			return original
		}
		return context.Canceled
	}
	return err
}
