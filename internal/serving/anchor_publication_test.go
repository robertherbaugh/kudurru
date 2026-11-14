package serving

import (
	"context"
	"crypto"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

// This fixture commits actual signed history through the public driver. The
// complete publisher below uses test owner components, not a production DNS
// resolver or transport; its assertions concern the real pointer boundary.
func managedPublicationStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s := primary(t)
	original, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx := sqlite.WithWriterToken(original, writer)
	init, err := persistence.CaptureInitialization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Command(ctx, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := persistence.InitializeDormant(ctx, tx, init); err != nil {
			return err
		}
		return tx.InitializeAnchorLocal(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	image, err := s.ReadGeneration(ctx)
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
	transition, err := persistence.CaptureTransition(ctx, owner.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	update, err := s.CaptureAnchorUpdate(ctx, owner.Version())
	if err != nil {
		t.Fatal(err)
	}
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".", Inception: uint32(time.Now().Add(-time.Minute).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(time.Hour).Unix() & 0xffffffff)}
	if err := sig.Sign(private.(crypto.Signer), []dns.RR{key}); err != nil {
		t.Fatal(err)
	}
	query := new(dns.Msg)
	query.SetQuestion(".", dns.TypeDNSKEY)
	message := new(dns.Msg)
	message.SetReply(query)
	message.Authoritative = true
	message.Answer = []dns.RR{key, sig}
	state, observation, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: image.Term.ClusterID, PolicyID: "publication-fixture", DS: []string{key.ToDS(dns.SHA256).String()}}, message, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, transition, state, observation)
		return err
	})
	if err != nil || result.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("actual managed publication fixture", err)
	}
	return s
}

func preparePublication(s *sqlite.Store, released *atomic.Int32) PrepareFunc {
	return func(ctx context.Context, candidate Candidate) (Prepared, error) {
		prepared, err := allComponents(candidate)
		if err != nil {
			return prepared, err
		}
		prepared.Components[0].Release = func() { released.Add(1) }
		prepared.Anchor, err = s.CaptureAnchorPublication(ctx, candidate.Token)
		return prepared, err
	}
}

func readPublication(t *testing.T, input *sqlite.AnchorResolverInput) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	read, err := input.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := input.WithRead(read, func(scope sqlite.AnchorReadScope) error { return scope.Check() }); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherAnchorAppliesOnlyAtCompleteSwap(t *testing.T) {
	s := managedPublicationStore(t)
	var released atomic.Int32
	p, err := New(Options{Source: s, Prepare: preparePublication(s, &released), Required: CompleteManifest()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	next := p.prepare(ctx)
	if next.err != nil {
		t.Fatal(next.err)
	}
	input := next.bundle.anchor.Input()
	if _, err := input.CaptureRead(ctx); !errors.Is(err, sqlite.ErrAnchorUnavailable) {
		t.Fatal("unpublished input admitted", err)
	}
	if err := p.publish(ctx, next.bundle); err != nil {
		t.Fatal(err)
	}
	cancel()
	lease, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if lease.Bundle() != next.bundle || lease.Bundle().Identity().Publication != 1 || released.Load() != 0 {
		t.Fatal("registration was not paired with complete pointer")
	}
	readPublication(t, input)
	lease.Release()
	p.Close()
	if released.Load() != 1 {
		t.Fatal("complete resource reference not released once")
	}
}

type completionErrorSource struct {
	*sqlite.Store
	refusal error
}

func (s *completionErrorSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, opts sqlite.PublicationOptions, publish func() error) error {
	if err := s.Store.PublishIfCurrent(ctx, token, opts, publish); err != nil {
		return err
	}
	return s.refusal
}

func TestPublisherAnchorActualSwapSurvivesOuterCompletionError(t *testing.T) {
	s := managedPublicationStore(t)
	refusal := errors.New("fixture completion failure after actual Store callback")
	var released atomic.Int32
	p, err := New(Options{Source: &completionErrorSource{Store: s, refusal: refusal}, Prepare: preparePublication(s, &released), Required: CompleteManifest()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	next := p.prepare(ctx)
	if next.err != nil {
		t.Fatal(next.err)
	}
	if err := p.publish(ctx, next.bundle); !errors.Is(err, refusal) {
		t.Fatal("outer completion error lost", err)
	}
	lease, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if lease.Bundle() != next.bundle || released.Load() != 0 {
		t.Fatal("live pointer released or mislabeled rollback")
	}
	readPublication(t, next.bundle.anchor.Input())
	p.Close()
	if released.Load() != 0 {
		t.Fatal("held live bundle released before lease")
	}
	lease.Release()
	if released.Load() != 1 {
		t.Fatal("actual swapped bundle reference leaked")
	}
}

type originalBoundSource struct {
	*sqlite.Store
	original chan context.Context
}

func (s *originalBoundSource) ReadServingCandidate(ctx context.Context) (*replication.Image, sqlite.GenerationToken, error) {
	select {
	case s.original <- ctx:
	default:
	}
	return s.Store.ReadServingCandidate(ctx)
}

func TestPublisherAnchorOriginalBoundPrecedesReadAndSurvivesWorkCancel(t *testing.T) {
	s := managedPublicationStore(t)
	source := &originalBoundSource{Store: s, original: make(chan context.Context, 1)}
	var released atomic.Int32
	p := runPublisher(t, Options{Source: source, Prepare: preparePublication(s, &released), PreparationTimeout: 5 * time.Second})
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	var original context.Context
	select {
	case original = <-source.original:
	default:
		t.Fatal("candidate read did not retain original lifetime")
	}
	deadline, finite := original.Deadline()
	if !finite || time.Until(deadline) > 5*time.Second {
		t.Fatal("candidate read preceded original bound")
	}
	select {
	case <-original.Done():
	case <-time.After(time.Second):
		t.Fatal("publisher did not cancel completed preparation")
	}
	lease, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	readPublication(t, lease.Bundle().anchor.Input())
}
