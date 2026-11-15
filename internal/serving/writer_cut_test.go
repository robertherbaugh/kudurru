package serving

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// A later publication of the same cut (a Wake's second preparation of the
// same source) does not refuse a command's lease at CheckWriterCut, although
// CheckWriterLease refuses it; a dependency change, a new generation and a
// changed writer still refuse it (D399).
func TestPublisherWriterCutSurvivesASameCutPublicationOnly(t *testing.T) {
	s := primary(t)
	var changed atomic.Bool
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		parts.Current = func() bool { return !changed.Load() }
		return parts, err
	}})
	first := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	authority := s.Authority()
	target := TermActivation{Term: authority.Term, Incarnation: authority.Incarnation}
	lease, err := p.CaptureHTTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx := context.Background()
	if err := p.CheckWriterCut(ctx, lease, target); err != nil {
		t.Fatal("a current lease refused:", err)
	}
	p.Wake()
	again := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Publication > first.Served.Publication })
	if !again.Served.Base.Same(first.Served.Base) || again.Served.Dependencies != first.Served.Dependencies {
		t.Fatal("the Wake published another cut")
	}
	if err := p.CheckWriterLease(ctx, lease, target); !errors.Is(err, ErrPending) {
		t.Fatal("CheckWriterLease admitted a replaced publication:", err)
	}
	if err := p.CheckWriterCut(ctx, lease, target); err != nil {
		t.Fatal("a later publication of the same cut refused the command:", err)
	}
	wrong := target
	wrong.Incarnation++
	if err := p.CheckWriterCut(ctx, lease, wrong); !errors.Is(err, ErrSuperseded) {
		t.Fatal("another writer incarnation was admitted:", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.CheckWriterCut(canceled, lease, target); !errors.Is(err, context.Canceled) {
		t.Fatal("a canceled check was admitted:", err)
	}
	changed.Store(true)
	if err := p.CheckWriterCut(ctx, lease, target); !errors.Is(err, ErrPending) {
		t.Fatal("a lease whose dependencies changed was admitted:", err)
	}
	changed.Store(false)
	if err := p.CheckWriterCut(ctx, lease, target); err != nil {
		t.Fatal(err)
	}
	applyPair(t, s, "next")
	if err := p.CheckWriterCut(ctx, lease, target); !errors.Is(err, ErrPending) {
		t.Fatal("a lease of a superseded generation was admitted:", err)
	}
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Base.Revision > first.Served.Base.Revision })
	if err := p.CheckWriterCut(ctx, lease, target); !errors.Is(err, ErrPending) {
		t.Fatal("a lease of a superseded generation was admitted after its publication:", err)
	}
	lease.Release()
	if err := p.CheckWriterCut(ctx, lease, target); err == nil {
		t.Fatal("a released lease was admitted")
	}
}
