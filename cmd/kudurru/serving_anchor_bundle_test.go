package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

func rootSealFromActualBundle(t *testing.T, f *servingCommandFixture) *recursiveRootSeal {
	t.Helper()
	lease, err := f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	bundle := lease.Bundle()
	value, exists := bundle.Component(recursiveRootComponent)
	seal, typed := value.(*recursiveRootSeal)
	if !exists || !typed || seal == nil || !seal.base.Same(bundle.Identity().Base) || !seal.current() {
		t.Fatal("actual complete publication omitted its private original root component")
	}
	return seal
}

func TestRootAnchorBundleActualStaticPublicationAndOriginalRead(t *testing.T) {
	f := newServingCommandFixture(t)
	seal := rootSealFromActualBundle(t, f)
	if seal.static == nil || seal.managed != nil || f.s.anchors == nil {
		t.Fatal("static publication substituted a managed registration")
	}
	// The publisher has completed and canceled its preparation work. A query
	// has an independent original lifetime, never that preparation context.
	original, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	read, err := seal.captureRead(original)
	if err != nil {
		t.Fatal(err)
	}
	var borrowed func() error
	called := false
	if err := seal.withRead(read, func(check func() error) error {
		called, borrowed = true, check
		return check()
	}); err != nil || !called {
		t.Fatal("actual root input did not admit bounded original read", err)
	}
	if borrowed() == nil {
		t.Fatal("escaped check retained exclusion")
	}
	called = false
	if err := seal.withRead(read, func(func() error) error { called = true; return nil }); err == nil || called {
		t.Fatal("original read was reusable", err)
	}
	retained, err := seal.captureRead(original)
	if err != nil {
		t.Fatal(err)
	}
	copySeal := *seal
	if err := copySeal.withRead(retained, func(func() error) error { called = true; return nil }); !errors.Is(err, serving.ErrComponents) || called {
		t.Fatal("copied component adopted a different original read", err)
	}
	cancel()
	if err := seal.withRead(retained, func(func() error) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("original query cancellation was replaced", err)
	}
	if !seal.current() {
		t.Fatal("query cancellation retired a successful publication")
	}
	if _, err := seal.captureRead(context.Background()); err == nil {
		t.Fatal("unbounded query admitted")
	}
}

// This is actual Store bootstrap plus a root-published input identity check.
// It deliberately invokes the existing driver directly; no updater, network
// output, cache insertion or edge activation coverage is claimed here.
func TestRootAnchorBundleStaticReadRefusesActualManagedSuccessor(t *testing.T) {
	f := newServingCommandFixture(t)
	seal := rootSealFromActualBundle(t, f)
	original, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	read, err := seal.captureRead(original)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	original = sqlite.WithWriterToken(original, writer)
	image, err := f.s.opts.Store.ReadGeneration(original)
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
	capture, err := persistence.CaptureTransition(original, owner.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	update, err := f.s.opts.Store.CaptureAnchorUpdate(original, owner.Version())
	if err != nil {
		t.Fatal(err)
	}
	seed, reply := anchorWireSignedReply(t)
	policy, err := (config.Recursion{AnchorMode: config.RootAnchorsManaged, DNSSEC: true, TrustAnchors: seed.DS}).RootAnchorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	seed.ClusterID, seed.PolicyID, seed.DS = image.Term.ClusterID, policy.PolicyID, policy.Seeds
	query := new(dns.Msg)
	query.SetQuestion(".", dns.TypeDNSKEY)
	query.RecursionDesired = false
	query.SetEdns0(1232, true)
	request, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	message := new(dns.Msg)
	if err := message.Unpack(reply(request)); err != nil {
		t.Fatal(err)
	}
	state, observation, err := trustanchors.Bootstrap(seed, message, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.s.opts.Store.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, capture, state, observation)
		return err
	})
	if err != nil || receipt.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("actual original bootstrap did not commit", err)
	}
	if seal.current() {
		t.Fatal("old static component remained current after real Managed acceptance")
	}
	called := false
	if err := seal.withRead(read, func(func() error) error { called = true; return nil }); err == nil || called {
		t.Fatal("old original static read adopted Managed successor", err)
	}
	if _, err := f.s.anchors.source.StaticInput(); err == nil {
		t.Fatal("Managed history minted a fresh static fallback")
	}
}

func TestRootAnchorBundleMissingAndUnboundedInputsRefuse(t *testing.T) {
	if _, err := newRootAnchorOwner(t.Context(), nil, nil); !errors.Is(err, serving.ErrComponents) {
		t.Fatal(err)
	}
	var seal *recursiveRootSeal
	if seal.current() {
		t.Fatal("nil root component current")
	}
	if _, err := seal.captureRead(t.Context()); !errors.Is(err, serving.ErrDependency) {
		t.Fatal(err)
	}
	if err := seal.withRead(nil, nil); !errors.Is(err, serving.ErrComponents) {
		t.Fatal(err)
	}
	f := newServingCommandFixture(t)
	seal = rootSealFromActualBundle(t, f)
	tooLong, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := seal.captureRead(tooLong); err == nil {
		t.Fatal("query lifetime widened beyond original bound")
	}
	f.stop()
	stopped, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := f.s.Close(stopped); err != nil {
		t.Fatal(err)
	}
	if seal.current() {
		t.Fatal("closed root lifetime left original component eligible")
	}
}
