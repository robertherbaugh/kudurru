package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

func edgeAnchorOptions(t *testing.T, base *servingRuntime, fleet []anchorEdgeRequirement, client *internalapi.Client) servingRuntimeOptions {
	t.Helper()
	cfg, err := config.FromFlat(config.Flatten(base.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Node.Name = "backend-a"
	cfg.Upstreams.Recursion.ManagedTopology = "edges"
	for _, e := range fleet {
		cfg.Upstreams.Recursion.ManagedEdges = append(cfg.Upstreams.Recursion.ManagedEdges, config.ManagedRootEdge{Node: e.node, Address: e.address, Deployment: e.deployment, BackendAddress: e.backend.Address, BackendInternalAddress: e.backend.InternalAddress})
	}
	opts := base.opts
	opts.Config, opts.InternalClient = cfg, client
	return opts
}

func TestAnchorEdgeActivationActualWriterAndRuntimeLifetime(t *testing.T) {
	base := newServingRuntimeFixture(t)
	var calls atomic.Int32
	observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
		calls.Add(1)
		return anchorEdgeState(id), nil
	}
	first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
	second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), observe)
	opts := edgeAnchorOptions(t, base, []anchorEdgeRequirement{first, second}, client)
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	r := &opts.Config.Upstreams.Recursion
	r.AnchorMode, r.TrustAnchors, r.RootHints, r.IPv6 = config.RootAnchorsManaged, seed.DS, []string{endpoint.String()}, "off"
	opts.Config.EnvLocked = append(opts.Config.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
	startup, finish := context.WithTimeout(t.Context(), 5*time.Second)
	s, err := newServingRuntime(startup, opts)
	finish()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	if calls.Load() != 2 || !s.anchors.admission.current() || s.anchors.admission.edges == nil {
		t.Fatal("complete fleet did not enter runtime life")
	}
	if s.anchors.admission.edges.check() == nil {
		t.Fatal("adoption extended original observation deadline")
	}
	copyAdmission := *s.anchors.admission
	if copyAdmission.current() {
		t.Fatal("equal fields replaced issued admission")
	}
	// Neither caller config mutation nor preparation cancellation can replace
	// the immutable fleet retained by the successfully admitted runtime.
	opts.Config.Upstreams.Recursion.ManagedEdges[0].Deployment = strings.Repeat("d", 64)
	runServingFixture(t, s)
	life, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	carried, done := make(chan context.Context, 1), make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("edge-anchor-test").Run(life, func(ctx context.Context) { carried <- ctx; <-ctx.Done() }, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("edge elector did not join")
		}
	})
	var original context.Context
	select {
	case original = <-carried:
	case <-life.Done():
		t.Fatal("edge writer was not elected")
	}
	stop := startDirectAnchorWriter(original, t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	if anchorOperationOwner(original, t, s).Inspect().Phase != accepted.Managed || reads.Load() != 1 || calls.Load() != 2 {
		t.Fatal("edge admission did not mount one actual bootstrap")
	}
	stop()
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.anchors.admission.current() {
		t.Fatal("closed runtime retained edge admission")
	}
}

func TestAnchorEdgeActivationRefusesChangedOriginal(t *testing.T) {
	for _, name := range []string{"authority", "canceled", "missing-member", "missing-credentials"} {
		t.Run(name, func(t *testing.T) {
			base := newServingRuntimeFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var calls atomic.Int32
			peer, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
				calls.Add(1)
				state := anchorEdgeState(id)
				switch name {
				case "authority":
					term := base.opts.Store.Authority().Term
					term.Epoch++
					term.PrimaryID, term.PromotionID = "replacement", "new"
					if _, err := base.opts.Store.ObserveAuthority(t.Context(), term); err != nil {
						return state, err
					}
				case "canceled":
					cancel()
				case "missing-member":
					state.Backends[0].Name = "other-backend"
				}
				return state, nil
			})
			if name == "missing-credentials" {
				client = nil
			}
			s, err := newServingRuntime(ctx, edgeAnchorOptions(t, base, []anchorEdgeRequirement{peer}, client))
			if err == nil {
				_ = s.Close(t.Context())
				t.Fatal("refused original edge input enabled startup")
			}
			want := int32(1)
			if name == "missing-credentials" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("edge admission replayed or acquired without credentials", calls.Load())
			}
		})
	}
}

// TestAnchorEdgeActivationRefusesUnsupportedPeer (F28-MV-2 (c)): a declared
// edge whose internal listener has no observation route, as a peer of an
// earlier release has none, answers 404. Startup is refused with
// ErrEdgeBypassUnsupported, the edges after it are not asked, the root is
// never asked and the anchor owner stays as it was. A released 0.1.0 peer
// is the same refusal end to end in TestAnchorUpgradeFromReleasedV010.
func TestAnchorEdgeActivationRefusesUnsupportedPeer(t *testing.T) {
	base := newServingRuntimeFixture(t)
	var calls atomic.Int32
	observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
		calls.Add(1)
		return anchorEdgeState(id), nil
	}
	first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
	old, _ := anchorEdgePeer(t, "edge-old", strings.Repeat("b", 64), nil)
	last, _ := anchorEdgePeer(t, "edge-c", strings.Repeat("d", 64), observe)
	opts := edgeAnchorOptions(t, base, []anchorEdgeRequirement{first, old, last}, client)
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	r := &opts.Config.Upstreams.Recursion
	r.AnchorMode, r.TrustAnchors, r.RootHints, r.IPv6 = config.RootAnchorsManaged, seed.DS, []string{endpoint.String()}, "off"
	opts.Config.EnvLocked = append(opts.Config.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
	owner := func() accepted.Version {
		t.Helper()
		image, err := base.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		o, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		return o.Version()
	}
	before := owner()
	startup, finish := context.WithTimeout(t.Context(), 5*time.Second)
	s, err := newServingRuntime(startup, opts)
	finish()
	if err == nil {
		_ = s.Close(t.Context())
		t.Fatal("startup admitted an edge without the observation route")
	}
	if !errors.Is(err, internalapi.ErrEdgeBypassUnsupported) {
		t.Fatalf("startup refusal: %v, want %v", err, internalapi.ErrEdgeBypassUnsupported)
	}
	if calls.Load() != 1 || reads.Load() != 0 {
		t.Fatal("refused fleet contacted later edges or the root", calls.Load(), reads.Load())
	}
	if after := owner(); after != before {
		t.Fatal("refused startup changed the anchor owner", before, after)
	}
}

// A fenced or conflicted backend used to be refused at startup before any edge
// was contacted, so it could not restart behind its edges at all. D369 admits
// that DNS-only cut, but only with the same complete authenticated fleet
// observation a writable backend needs, adopted under the same original token.
// This fixture leaves anchorMode at its static default and its owner dormant:
// it proves the edges admission rule alone. The managed owner behind edges is
// TestAnchorEdgeActivationManagedDNSOnlyRestartNeedsCompleteFleet below.
func TestAnchorEdgeActivationAdmitsDNSOnlyCutOnlyWithCompleteFleet(t *testing.T) {
	for _, role := range []replication.Mode{replication.Fenced, replication.Conflict} {
		for _, name := range []string{"complete", "unavailable-edge", "missing-member", "authority"} {
			t.Run(string(role)+"/"+name, func(t *testing.T) {
				base := newServingRuntimeFixture(t)
				st := base.opts.Store
				term := st.Authority().Term
				term.PrimaryID, term.PromotionID = "replacement", "new"
				if role == replication.Fenced {
					term.Epoch++
				}
				if _, err := st.ObserveAuthority(t.Context(), term); err != nil && !errors.Is(err, replication.ErrPromotionConflict) {
					t.Fatal(err)
				}
				lost := st.Authority()
				if lost.Mode != role {
					t.Fatal("fixture did not reach the DNS-only role", lost.Mode)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var calls atomic.Int32
				observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
					n := calls.Add(1)
					state := anchorEdgeState(id)
					if n != 2 {
						return state, nil
					}
					switch name {
					case "unavailable-edge":
						return state, errors.New("original child unavailable")
					case "missing-member":
						state.Backends[0].Name = "other-backend"
					case "authority":
						// Another real authority change after the local cut was
						// captured: the original DNS-only token no longer matches.
						next := st.Authority().Term
						next.Epoch = st.Authority().MaxObservedEpoch + 1
						next.PrimaryID, next.PromotionID = "third", "later"
						if _, err := st.ObserveAuthority(t.Context(), next); err != nil {
							return state, err
						}
					}
					return state, nil
				}
				first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
				second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), observe)
				s, err := newServingRuntime(ctx, edgeAnchorOptions(t, base, []anchorEdgeRequirement{first, second}, client))
				if name != "complete" {
					if err == nil {
						_ = s.Close(t.Context())
						t.Fatal("partial or changed fleet evidence admitted a DNS-only backend")
					}
					if calls.Load() != 2 {
						t.Fatal("refused DNS-only admission replayed or skipped its fleet", calls.Load())
					}
					return
				}
				if err != nil {
					t.Fatal("complete fleet did not admit the DNS-only backend", err)
				}
				t.Cleanup(func() { _ = s.Close(t.Context()) })
				if calls.Load() != 2 || !s.anchors.admission.current() || s.anchors.admission.edges == nil {
					t.Fatal("DNS-only backend did not adopt its complete fleet observation", calls.Load())
				}
				runServingFixture(t, s)
				awaitServingRuntime(t, s, func() bool { return s.publisher.Status().Reason == "dns_only" })
				if _, err := s.publisher.CaptureHTTP(t.Context()); err == nil {
					t.Fatal("edge admission reopened protected HTTP on a DNS-only backend")
				}
				lease, err := s.publisher.Capture()
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				if !lease.Bundle().DNSOnly() || !reflect.DeepEqual(lost, st.Authority()) || calls.Load() != 2 {
					t.Fatal("edge admission changed the DNS-only disposition, authority or fleet evidence")
				}
			})
		}
	}
}

// anchorEdgeManagedBootstrap runs one actual managed backend behind its edge
// fleet until the elected writer has committed the bootstrap, then stops and
// joins the writer, the elector and the runtime. It returns the accepted owner
// version that every later restart must still serve.
func anchorEdgeManagedBootstrap(t *testing.T, opts servingRuntimeOptions) accepted.Version {
	t.Helper()
	startup, finish := context.WithTimeout(t.Context(), 5*time.Second)
	s, err := newServingRuntime(startup, opts)
	finish()
	if err != nil {
		t.Fatal("writable managed backend did not start behind its edges", err)
	}
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	runCtx, stopRun := context.WithCancel(t.Context())
	defer stopRun()
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(runCtx) }()
	life, cancelLife := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelLife()
	carried, electorDone := make(chan context.Context, 1), make(chan error, 1)
	go func() {
		electorDone <- opts.Store.Elector("edge-managed-dns-only").Run(life, func(ctx context.Context) { carried <- ctx; <-ctx.Done() }, nil)
	}()
	var original context.Context
	select {
	case original = <-carried:
	case <-life.Done():
		t.Fatal("edge writer was not elected")
	}
	stopWriter := startDirectAnchorWriter(original, t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	owner := anchorOperationOwner(original, t, s)
	if owner.Inspect().Phase != accepted.Managed {
		t.Fatal("elected writer did not bootstrap managed trust behind its edges", owner.Inspect().Phase)
	}
	stopWriter()
	cancelLife()
	if err := <-electorDone; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	stopRun()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return owner.Version()
}

// The same rule with anchorMode managed, which is the mode D369 changed: an
// actual backend bootstraps managed trust through its elected writer behind a
// complete two-edge fleet, loses its term durably, and its database is closed
// and reopened. The restart is admitted only with the complete fleet
// observation. It then serves the last accepted managed root as DNS-only
// output, keeps protected HTTP closed and makes exactly one supervision
// acquisition; a missing, mismatched or changed observation refuses startup
// after contacting the whole fleet exactly once.
func TestAnchorEdgeActivationManagedDNSOnlyRestartNeedsCompleteFleet(t *testing.T) {
	for _, role := range []replication.Mode{replication.Fenced, replication.Conflict} {
		for _, name := range []string{"complete", "unavailable-edge", "missing-member", "authority"} {
			t.Run(string(role)+"/"+name, func(t *testing.T) {
				base := newServingRuntimeFixture(t)
				var restarting atomic.Bool
				var bootstrap, calls atomic.Int32
				var reopened atomic.Pointer[sqlite.Store]
				observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
					state := anchorEdgeState(id)
					if !restarting.Load() {
						bootstrap.Add(1)
						return state, nil
					}
					if calls.Add(1) != 2 {
						return state, nil
					}
					switch name {
					case "unavailable-edge":
						return state, errors.New("original child unavailable")
					case "missing-member":
						state.Backends[0].Name = "other-backend"
					case "authority":
						// Another real authority change after the local cut was
						// captured: the original DNS-only token no longer matches.
						st := reopened.Load()
						next := st.Authority().Term
						next.Epoch = st.Authority().MaxObservedEpoch + 1
						next.PrimaryID, next.PromotionID = "third", "later"
						if _, err := st.ObserveAuthority(t.Context(), next); err != nil {
							return state, err
						}
					}
					return state, nil
				}
				first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
				second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), observe)
				opts := edgeAnchorOptions(t, base, []anchorEdgeRequirement{first, second}, client)
				endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
				r := &opts.Config.Upstreams.Recursion
				r.AnchorMode, r.TrustAnchors, r.RootHints, r.IPv6 = config.RootAnchorsManaged, seeds, []string{endpoint}, "off"
				opts.Config.EnvLocked = append(opts.Config.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
				owner := anchorEdgeManagedBootstrap(t, opts)
				if reads.Load() != 1 || bootstrap.Load() != 2 {
					t.Fatal("fixture did not bootstrap once behind one complete fleet observation", reads.Load(), bootstrap.Load())
				}
				st := opts.Store
				term := st.Authority().Term
				term.PrimaryID, term.PromotionID = "replacement", "new"
				if role == replication.Fenced {
					term.Epoch++
				}
				if _, err := st.ObserveAuthority(t.Context(), term); err != nil && !errors.Is(err, replication.ErrPromotionConflict) {
					t.Fatal(err)
				}
				if st.Authority().Mode != role {
					t.Fatal("fixture did not reach the DNS-only role", st.Authority().Mode)
				}
				// A physical restart: the original database handle and the unstarted
				// fixture runtime that borrowed it are closed first.
				if err := base.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				path := st.Path()
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				scheme, err := runtimekinds.NewScheme()
				if err != nil {
					t.Fatal(err)
				}
				st, err = sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				reopened.Store(st)
				opts.Store = st
				lost := st.Authority()
				if lost.Mode != role || lost.Writable() {
					t.Fatal("reopened database lost its DNS-only role", lost.Mode)
				}
				restarting.Store(true)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				s, err := newServingRuntime(ctx, opts)
				if name != "complete" {
					if err == nil {
						_ = s.Close(t.Context())
						t.Fatal("partial or changed fleet evidence admitted a managed DNS-only backend")
					}
					if calls.Load() != 2 || reads.Load() != 1 {
						t.Fatal("refused managed DNS-only admission replayed or skipped its fleet, or acquired", calls.Load(), reads.Load())
					}
					return
				}
				if err != nil {
					t.Fatal("complete fleet did not admit the managed DNS-only backend", err)
				}
				t.Cleanup(func() {
					closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := s.Close(closing); err != nil {
						t.Error(err)
					}
				})
				if calls.Load() != 2 || !s.anchors.admission.current() || s.anchors.admission.edges == nil {
					t.Fatal("managed DNS-only backend did not adopt its complete fleet observation", calls.Load())
				}
				runServingFixture(t, s)
				awaitServingRuntime(t, s, func() bool { return s.publisher.Status().Reason == "dns_only" })
				if _, err := s.publisher.CaptureHTTP(t.Context()); err == nil {
					t.Fatal("edge admission reopened protected HTTP on a managed DNS-only backend")
				}
				lease, err := s.publisher.Capture()
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				part, _ := lease.Bundle().Component(recursiveRootComponent)
				seal, _ := part.(*recursiveRootSeal)
				if seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || !lease.Bundle().DNSOnly() {
					t.Fatal("DNS-only bundle behind edges did not keep the accepted managed root")
				}
				// A managed output needs a finite original lifetime of at most 30 s.
				query, done := context.WithTimeout(t.Context(), 10*time.Second)
				defer done()
				output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: query, Serving: servingdns.Wrap(lease)})
				if err != nil {
					t.Fatal("output capture", err)
				}
				if err := output.Emit(func() error { return nil }); err != nil {
					t.Fatal("managed DNS-only backend behind edges refused its last-good output", err)
				}
				// Exactly one deny-only supervision acquisition, and nothing more
				// inside its cooldown; no writer, bootstrap or fleet replay.
				awaitServingRuntime(t, s, func() bool { return reads.Load() == 2 })
				time.Sleep(1200 * time.Millisecond)
				if reads.Load() != 2 || calls.Load() != 2 || bootstrap.Load() != 2 {
					t.Fatal("managed DNS-only backend reacquired or replayed its fleet", reads.Load(), calls.Load())
				}
				if anchorOperationOwner(t.Context(), t, s).Version() != owner || !reflect.DeepEqual(lost, st.Authority()) {
					t.Fatal("managed DNS-only restart changed accepted trust history or authority")
				}
			})
		}
	}
}
