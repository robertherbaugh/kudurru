package main

import (
	"context"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// anchorStartupGate belongs to one actual elected term/incarnation. It holds
// legacy elected workers, which are outside writerEpisode, through the same
// first anchor step as the provider and primary services. It grants no writer
// authority and never holds a lock across acquisition or worker execution.
type anchorStartupGate struct {
	mu      sync.Mutex
	target  serving.TermActivation
	ready   chan struct{}
	release func()
}

func (s *servingRuntime) anchorWriterStartup(ctx context.Context) (<-chan struct{}, func(), error) {
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok {
		return nil, nil, replication.ErrFenced
	}
	target := serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}
	gate := &s.anchorStartup
	gate.mu.Lock()
	defer gate.mu.Unlock()
	// Check under the gate lock so an old term cannot replace a new term's
	// channel after its authority has changed. The store never takes this lock.
	if err := s.writerCurrent(ctx, target); err != nil {
		return nil, nil, err
	}
	if gate.ready == nil || !gate.target.Term.Same(target.Term) || gate.target.Incarnation != target.Incarnation {
		ready := make(chan struct{})
		gate.target, gate.ready = target, ready
		gate.release = sync.OnceFunc(func() { close(ready) })
	}
	return gate.ready, gate.release, nil
}

func (s *servingRuntime) afterAnchorStartup(ctx context.Context, run func(context.Context) error) error {
	if s.anchors == nil || s.anchors.admission == nil {
		return run(ctx)
	}
	ready, _, err := s.anchorWriterStartup(ctx)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
	}
	token, _ := sqlite.WriterTokenFromContext(ctx)
	if err := s.writerCurrent(ctx, serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}); err != nil {
		return err
	}
	return run(ctx)
}
