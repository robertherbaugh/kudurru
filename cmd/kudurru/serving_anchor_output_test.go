package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

// Capture a genuine signed bootstrap before the competing emission. This is
// the actual driver mode transition; automatic polling is not mounted here.
func captureOutputBootstrap(ctx context.Context, t *testing.T, f *servingCommandFixture) func() error {
	t.Helper()
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx = sqlite.WithWriterToken(ctx, writer)
	image, err := f.s.opts.Store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	original, err := persistence.CaptureTransition(ctx, owner.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	update, err := f.s.opts.Store.CaptureAnchorUpdate(ctx, owner.Version())
	if err != nil {
		t.Fatal(err)
	}
	seed, reply := anchorWireSignedReply(t)
	policy, err := (config.Recursion{AnchorMode: config.RootAnchorsManaged, DNSSEC: true, TrustAnchors: seed.DS}).RootAnchorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	seed.ClusterID, seed.PolicyID, seed.DS = image.Term.ClusterID, policy.PolicyID, policy.Seeds
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	q.RecursionDesired = false
	q.SetEdns0(1232, true)
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	m := new(dns.Msg)
	if err := m.Unpack(reply(wire)); err != nil {
		t.Fatal(err)
	}
	state, observation, err := trustanchors.Bootstrap(seed, m, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	return func() error {
		receipt, err := f.s.opts.Store.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := persistence.Bootstrap(ctx, tx, original, state, observation)
			return err
		})
		if err == nil && receipt.Outcome() != sqlite.AnchorCommitted {
			return errors.New("actual bootstrap omitted committed disposition")
		}
		return err
	}
}

func actualRootOutput(ctx context.Context, t *testing.T, f *servingCommandFixture) *rootAnchorOutput {
	t.Helper()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)})
	if err != nil {
		t.Fatal(err)
	}
	return output.(*rootAnchorOutput)
}

func TestRootAnchorOutputActualBlockedWriteDrainsBeforeFirstManagedCommit(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := actualRootOutput(ctx, t, f)
	bootstrap := captureOutputBootstrap(ctx, t, f)
	writer, reader := net.Pipe()
	defer func() { _ = writer.Close(); _ = reader.Close() }()
	deadline, _ := ctx.Deadline()
	_ = writer.SetDeadline(deadline)
	_ = reader.SetDeadline(deadline)
	entered := make(chan struct{})
	emitted := make(chan error, 1)
	go func() {
		emitted <- output.Emit(func() error { close(entered); _, err := writer.Write([]byte("original")); return err })
	}()
	<-entered
	started := make(chan struct{})
	committed := make(chan error, 1)
	go func() { close(started); committed <- bootstrap() }()
	<-started
	select {
	case err := <-committed:
		t.Fatal("first managed transition crossed active actual write", err)
	case <-time.After(40 * time.Millisecond):
	}
	bytes := make([]byte, len("original"))
	if _, err := io.ReadFull(reader, bytes); err != nil || string(bytes) != "original" {
		t.Fatal("actual scoped write failed", err)
	}
	if err := <-emitted; err != nil {
		t.Fatal(err)
	}
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if err := output.Next(); err == nil {
		t.Fatal("transfer continuation adopted managed successor")
	}
	called := false
	if err := output.Emit(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("retired original output emitted again")
	}
}

func TestRootAnchorOutputOriginalLifetimeAndTerminalFailure(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := actualRootOutput(ctx, t, f)
	if err := output.Next(); err == nil {
		t.Fatal("unwritten output advanced")
	}
	if err := output.Emit(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := output.Emit(func() error { t.Fatal("reused output entered"); return nil }); err == nil {
		t.Fatal("output reused")
	}
	if err := output.Next(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("partial write")
	if err := output.Emit(func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := output.Next(); err == nil {
		t.Fatal("failed frame was repaired")
	}
	canceled := actualRootOutput(ctx, t, f)
	cancel()
	if err := canceled.Emit(func() error { t.Fatal("canceled write entered"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
