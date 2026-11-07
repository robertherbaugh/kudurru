package main

import (
	"context"
	"crypto"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// This fixture uses an actual elector context and the production complete pin.
// Its immutable host locks intentionally select managed policy over the empty
// canonical settings document. It does not mount an automatic writer worker.
func anchorOperationFixture(t *testing.T, endpoint string, seeds []string) (*servingRuntime, context.Context, serving.TermActivation, providercoordinator.HostCapture) {
	t.Helper()
	base := newServingRuntimeFixture(t)
	cfg, err := config.FromFlat(config.Flatten(base.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	cfg.EnvLocked = append(cfg.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
	opts := base.opts
	opts.Config = cfg
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	runServingFixture(t, s)
	parent, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	writer := make(chan context.Context, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("anchor-operation-test").Run(parent, func(ctx context.Context) {
			select {
			case writer <- ctx:
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("anchor original elector failed to join")
		}
	})
	var original context.Context
	select {
	case original = <-writer:
	case <-parent.Done():
		t.Fatal("original anchor writer unavailable")
	}
	carried, ok := sqlite.WriterTokenFromContext(original)
	if !ok {
		t.Fatal("elector omitted original writer")
	}
	target := serving.TermActivation{Term: carried.Term(), Incarnation: carried.Incarnation()}
	if _, err := s.opts.Store.BindAnchorLifecycle(original); err != nil {
		t.Fatal(err)
	}
	host, err := s.resetWriter(original, target)
	if err != nil {
		t.Fatal(err)
	}
	if host.Release != nil {
		t.Cleanup(host.Release)
	}
	for {
		err = s.anchorPin(original, primaryruntime.PublicationFence{Term: target.Term, Incarnation: target.Incarnation}, func(p currentguard.Pinned) error {
			if p.Current == nil || !p.Current() {
				return serving.ErrDependency
			}
			return nil
		})
		if err == nil {
			break
		}
		select {
		case <-original.Done():
			t.Fatal("original dependency pin unavailable", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return s, original, target, host
}

func TestAnchorOperationActualBootstrapOriginalPinsAndReceipt(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	s, ctx, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.captureAnchorOperation(ctx, target, host, clock, 0)
	if err != nil || op == nil {
		t.Fatal("capture original bootstrap", err)
	}
	result, err := op.run()
	if err != nil || result.Update.Outcome() != sqlite.AnchorCommitted || reads.Load() != 1 {
		t.Fatal("original root bootstrap did not commit once", err, result.Update.Outcome(), reads.Load())
	}
	image, err := s.opts.Store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil || owner.Inspect().Phase != accepted.Managed || owner.Inspect().PolicyID != op.policy.PolicyID || owner.Inspect().SeedDigest != op.policy.SeedDigest {
		t.Fatal("committed owner did not retain original policy", err)
	}
	if _, err := op.run(); err == nil || reads.Load() != 1 {
		t.Fatal("completed operation reacquired proof")
	}
}

func TestAnchorOperationCanonicalInterpositionRefusesBeforeEffect(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	s, ctx, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.captureAnchorOperation(ctx, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.opts.Store.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		settings.Labels = map[string]string{"anchor-policy": "interposed"}
		_, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := op.run()
	if err == nil || result.Update.Outcome() == sqlite.AnchorCommitted || reads.Load() != 1 {
		t.Fatal("interposed policy was rescued", err, result.Update.Outcome(), reads.Load())
	}
	image, err := s.opts.Store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil || owner.Version() != op.owner.Version() {
		t.Fatal("refused policy changed accepted root", err)
	}
}

func TestAnchorOperationOriginalHostAndPinnedFinalCheck(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	endpoint, _ := anchorWireServer(t, "tcp", reply)
	s, ctx, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.captureAnchorOperation(ctx, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Bool
	current.Store(true)
	entered := false
	completion, err := s.opts.Store.AnchorCommand(op.bootstrap, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := op.admit(ctx, tx, currentguard.Pinned{Current: current.Load}); err != nil {
			return err
		}
		entered = true
		// The adapter's root final hook must also run on an unchanged command.
		current.Store(false)
		return nil
	})
	if !entered || !errors.Is(err, serving.ErrDependency) || completion.Outcome() == sqlite.AnchorCommitted || completion.Outcome() == sqlite.AnchorUnchanged {
		t.Fatal("expired original pinned predicate passed final no-op", entered, err, completion.Outcome())
	}
	// A current successor pin cannot replace the original episode host.
	op.host.Current = func() bool { return false }
	if !errors.Is(op.checkOriginal(), serving.ErrDependency) {
		t.Fatal("original host loss accepted")
	}
}

func TestAnchorOperationRejectsAmbiguousSingletonAndBootstrapWire(t *testing.T) {
	s := newServingRuntimeFixture(t)
	image, _, err := s.opts.Store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	settings, err := anchorImageSettings(image)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*public.Kudurru){
		func(s *public.Kudurru) { s.UID = "" }, func(s *public.Kudurru) { s.ResourceVersion = "" },
		func(s *public.Kudurru) { s.Namespace = "other" }, func(s *public.Kudurru) { s.Name = "other" },
	} {
		next := settings.DeepCopy()
		change(next)
		raw, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		candidate := *image
		candidate.Payload.Objects = []json.RawMessage{raw}
		if _, err := anchorImageSettings(&candidate); err == nil {
			t.Fatal("ambiguous settings identity accepted")
		}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := *image
	duplicate.Payload.Objects = []json.RawMessage{raw, raw}
	if _, err := anchorImageSettings(&duplicate); err == nil {
		t.Fatal("duplicate singleton accepted")
	}
	_, reply := anchorWireSignedReply(t)
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	q.RecursionDesired = false
	q.SetEdns0(1232, true)
	request, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	response := reply(request)
	if _, err := unpackAnchorBootstrap(response); err != nil {
		t.Fatal(err)
	}
	if _, err := unpackAnchorBootstrap(append(response, 0)); err == nil {
		t.Fatal("bootstrap accepted trailing bytes")
	}
	oversized := append([]byte(nil), response...)
	oversized[6], oversized[7] = 255, 255
	if _, err := unpackAnchorBootstrap(oversized); err == nil {
		t.Fatal("bootstrap trusted unbounded record count")
	}
}

// The bounded server returns signatures from one actual key across bootstrap,
// refresh and self-revocation. Each operation still captures before its socket
// acquisition; no received response is replayed as a successor operation.
func anchorOperationServer(t *testing.T, replies int, beforeReply func(int)) (string, []string, *atomic.Int32) {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	seed := []string{key.ToDS(dns.SHA256).String()}
	var answers [2][]dns.RR
	for i := range answers {
		rr := dns.Copy(key).(*dns.DNSKEY)
		if i == 1 {
			rr.Flags |= dns.REVOKE
		}
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, Algorithm: rr.Algorithm, KeyTag: rr.KeyTag(), SignerName: ".", Inception: uint32(time.Now().Add(-time.Minute).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(time.Hour).Unix() & 0xffffffff)}
		if err := sig.Sign(private.(crypto.Signer), []dns.RR{rr}); err != nil {
			t.Fatal(err)
		}
		answers[i] = []dns.RR{rr, sig}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 1; n <= replies; n++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				var length [2]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					t.Error(err)
					return
				}
				raw := make([]byte, binary.BigEndian.Uint16(length[:]))
				if _, err := io.ReadFull(conn, raw); err != nil {
					t.Error(err)
					return
				}
				request := new(dns.Msg)
				if request.Unpack(raw) != nil || request.RecursionDesired || len(request.Question) != 1 || request.Question[0] != (dns.Question{Name: ".", Qtype: dns.TypeDNSKEY, Qclass: dns.ClassINET}) || request.IsEdns0() == nil || !request.IsEdns0().Do() {
					t.Error("root caller changed original DNSKEY acquisition")
					return
				}
				reads.Add(1)
				message := new(dns.Msg)
				message.SetReply(request)
				message.Authoritative = true
				message.Answer = answers[0]
				if n == replies {
					message.Answer = answers[1]
				}
				packet, err := message.Pack()
				if err != nil {
					t.Error("invalid signed root fixture", err)
					return
				}
				packetSize := len(packet)
				if packetSize > anchorWireLimit {
					t.Error("signed root fixture exceeds one DNS frame")
					return
				}
				if beforeReply != nil {
					beforeReply(n)
				}
				binary.BigEndian.PutUint16(length[:], uint16(packetSize))
				if _, err := conn.Write(append(length[:], packet...)); err != nil {
					t.Error(err)
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().String(), seed, &reads
}

func anchorOperationOwner(ctx context.Context, t *testing.T, s *servingRuntime) accepted.Snapshot {
	t.Helper()
	image, err := s.opts.Store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestAnchorOperationActualNormalAndTwoPhaseRevoke(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 3, nil)
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	for step := 1; step <= 3; step++ {
		op, err := s.captureAnchorOperation(ctx, target, host, clock, 0)
		if err != nil || op == nil {
			t.Fatal("original root operation capture", step, err)
		}
		result, err := op.run()
		if err != nil || result.Update.Outcome() != sqlite.AnchorCommitted || reads.Load() != int32(step) {
			t.Fatal("actual root operation did not commit once", step, err, result.Update.Outcome(), reads.Load())
		}
		if (result.Denial.Outcome() == sqlite.AnchorCommitted) != (step == 3) {
			t.Fatal("normal/revoke branch confused", step, result.Denial.Outcome())
		}
		owner := anchorOperationOwner(ctx, t, s)
		version, ok := result.Update.Version()
		if !ok || version != owner.Version() || version.Generation != op.owner.Version().Generation+1 {
			t.Fatal("root completion did not retain actual accepted version", step)
		}
		state, err := owner.State()
		if err != nil {
			t.Fatal(err)
		}
		if state.Inspect().Quarantined != (step == 3) || len(state.Inspect().Keys) != 1 {
			t.Fatal("root transition lost expected pure history", step)
		}
		if step == 3 && state.Inspect().Keys[0].Status != trustanchors.Revoked {
			t.Fatal("root two-phase command lost permanent tombstone")
		}
		if _, err := op.run(); err == nil || reads.Load() != int32(step) {
			t.Fatal("root operation acquired twice", step)
		}
	}
}

func TestAnchorOperationKnownRevokeDeniesBeforeOriginalHostRefusal(t *testing.T) {
	var hostCurrent atomic.Bool
	hostCurrent.Store(true)
	endpoint, seeds, reads := anchorOperationServer(t, 2, func(n int) {
		if n == 2 {
			hostCurrent.Store(false)
		}
	})
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	actualHostCurrent := host.Current
	// Deterministically expire this original host predicate after the actual
	// request. This tests root branch ordering, not replacement of a provider.
	host.Current = func() bool { return hostCurrent.Load() && actualHostCurrent() }
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := s.captureAnchorOperation(ctx, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := bootstrap.run(); err != nil || result.Update.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("original bootstrap", err)
	}
	before := anchorOperationOwner(ctx, t, s)
	op, err := s.captureAnchorOperation(ctx, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := op.run()
	if !errors.Is(err, serving.ErrDependency) || result.Denial.Outcome() != sqlite.AnchorCommitted || result.Update.Outcome() == sqlite.AnchorCommitted || reads.Load() != 2 {
		t.Fatal("granting host refusal masked known durable denial", err, result.Denial.Outcome(), result.Update.Outcome(), reads.Load())
	}
	after := anchorOperationOwner(ctx, t, s)
	if before.Version() != after.Version() {
		t.Fatal("refused phase2 advanced canonical owner")
	}
	if _, err := op.run(); err == nil || reads.Load() != 2 {
		t.Fatal("partially completed root revoke was replayed")
	}
}
