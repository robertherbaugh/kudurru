package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
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

// A fenced or conflicted backend used to be refused at startup before any edge
// was contacted, so it could not restart behind its edges at all. D369 admits
// that DNS-only cut, but only with the same complete authenticated fleet
// observation a writable backend needs, adopted under the same original token.
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
