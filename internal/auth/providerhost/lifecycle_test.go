package providerhost

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func install(t *testing.T, s *sqlite.Store, im *replication.Image) replication.Generation {
	t.Helper()
	v, err := s.ValidateImage(t.Context(), *im)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Authority()
	g, err := s.InstallReplica(t.Context(), v, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func TestActualRestartFollowerRecoveryLossAndPromotion(t *testing.T) {
	primary := newFixture(t, staticInput(t))
	addObject(t, primary, "object", spec())
	e0 := reset(t, primary)
	accept(t, primary, e0)
	im0 := image(t, primary.s)
	path := primary.s.Path()
	primary.stop()
	_ = primary.host.Close(t.Context())
	_ = primary.coordinator.Close(t.Context())
	primary.publisher.Close()
	if err := primary.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, path, replication.Standalone)
	resumed := start(t, reopened, primary.f, staticInput(t))
	cold := capture(t, resumed)
	if cold.Auth.License.Providers {
		t.Fatal("primary restart reused disk activation")
	}
	cold.Release()
	peer := openStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := peer.Enroll(t.Context(), im0.Term); err != nil {
		t.Fatal(err)
	}
	install(t, peer, im0)
	follower := start(t, peer, primary.f, staticInput(t))
	observe(t, follower)
	before := image(t, peer)
	held := capture(t, follower)
	if held.Auth.License.Providers {
		t.Fatal("historical follower boot granted E0")
	}
	held.Release()
	observe(t, follower)
	same := capture(t, follower)
	if same.Auth.License.Providers {
		t.Fatal("identical receipt cleared follower baseline")
	}
	same.Release()
	if !before.Generation().Same(image(t, peer).Generation()) {
		t.Fatal("follower observation wrote")
	}
	e1 := reset(t, resumed)
	accept(t, resumed, e1)
	if e1 == e0 {
		t.Fatal("restart did not rotate actual E")
	}
	install(t, peer, image(t, reopened))
	observe(t, follower)
	h := capture(t, follower)
	if !h.Auth.License.Providers || h.Identity == "" {
		t.Fatal("actual new primary E failed recovery")
	}
	follower.f.clock.Add(int64(time.Second))
	if err := follower.license.Set("", ""); err != nil {
		t.Fatal(err)
	}
	if err := follower.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := follower.license.Set(follower.f.raw, "test"); err != nil {
		t.Fatal(err)
	}
	if err := follower.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	observe(t, follower)
	blocked := capture(t, follower)
	if blocked.Auth.License.Providers || h.Current() {
		t.Fatal("same-byte return reused tainted E1")
	}
	blocked.Release()
	h.Release()
	e2 := reset(t, resumed)
	accept(t, resumed, e2)
	installed := install(t, peer, image(t, reopened))
	observe(t, follower)
	recovered := capture(t, follower)
	if !recovered.Auth.License.Providers {
		t.Fatal("fresh primary activation did not recover follower")
	}
	recovered.Release()
	if _, err := peer.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower minted writer")
	}
	latest := image(t, peer)
	if _, err := peer.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: latest.Term, ExpectedInstalled: installed, RequestID: "provider-host-promotion"}); err != nil {
		t.Fatal(err)
	}
	ctx, target := writer(t, peer)
	if err := follower.host.Reset(ctx, target); err != nil {
		t.Fatal("actual promoted original writer reset failed", err)
	}
	if follower.host.value.Load().entitlement == e2 || follower.host.value.Load().phase != pending {
		t.Fatal("promotion reused follower activation")
	}
	for _, a := range registry(t, peer).Authorities() {
		if a.Active {
			t.Fatal("promotion skipped withdrawal")
		}
	}
	state, err := servingauth.DecodeSecurity(image(t, peer).Payload.Security)
	if err != nil || len(state.ProviderFloors) != 1 {
		t.Fatal("restart/install/promotion lost exact floor", err)
	}
}

func TestInstalledForeignStaleMissingRootsAndUnknownEmptyHistoryDeny(t *testing.T) {
	primary := newFixture(t, staticInput(t))
	peer := openStore(t, filepath.Join(t.TempDir(), "empty-follower.db"), replication.Follower)
	im := image(t, primary.s)
	if err := peer.Enroll(t.Context(), im.Term); err != nil {
		t.Fatal(err)
	}
	install(t, peer, im)
	follower := start(t, peer, primary.f, staticInput(t))
	observe(t, follower)
	addObject(t, primary, "object", spec())
	e1 := reset(t, primary)
	accept(t, primary, e1)
	install(t, peer, image(t, primary.s))
	observe(t, follower)
	h := capture(t, follower)
	if h.Auth.License.Providers {
		t.Fatal("unknown empty history assumed fresh enrollment")
	}
	h.Release()
	old, token, err := peer.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	e2 := reset(t, primary)
	accept(t, primary, e2)
	install(t, peer, image(t, primary.s))
	observe(t, follower)
	ready := capture(t, follower)
	if !ready.Auth.License.Providers {
		t.Fatal("different actual activation did not clear baseline")
	}
	ready.Release()
	if err := follower.host.ObserveInstalled(t.Context(), old, token); !errors.Is(err, replication.ErrCAS) {
		t.Fatal("stale token adopted", err)
	}
	foreign := newFixture(t, staticInput(t))
	other, otherToken, err := foreign.s.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = follower.host.ObserveInstalled(t.Context(), other, otherToken); !errors.Is(err, ErrContinuity) {
		t.Fatal("foreign cluster adopted", err)
	}
	current, currentToken, err := peer.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	malformed := *current
	malformed.Payload.Security = nil
	if err = replication.Seal(&malformed); err != nil {
		t.Fatal(err)
	}
	if err = follower.host.ObserveInstalled(t.Context(), &malformed, currentToken); err == nil {
		t.Fatal("missing complete roots accepted")
	}
}

func TestFollowerTaintBaselinesInstalledButUnobservedActivation(t *testing.T) {
	for _, change := range []string{"license-observed", "license-in-image-observation", "static"} {
		t.Run(change, func(t *testing.T) {
			primary := newFixture(t, staticInput(t))
			addObject(t, primary, "object", spec())
			e0 := reset(t, primary)
			accept(t, primary, e0)
			peer := openStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
			im := image(t, primary.s)
			if err := peer.Enroll(t.Context(), im.Term); err != nil {
				t.Fatal(err)
			}
			install(t, peer, im)
			follower := start(t, peer, primary.f, staticInput(t))
			observe(t, follower)
			e1 := reset(t, primary)
			accept(t, primary, e1)
			install(t, peer, image(t, primary.s))
			observe(t, follower)
			h := capture(t, follower)
			if !h.Auth.License.Providers {
				t.Fatal("follower did not recover at E1")
			}
			h.Release()
			// E2 is already durably installed before the local change, but the
			// immutable host cursor still remembers E1. E2 is not recovery.
			e2 := reset(t, primary)
			accept(t, primary, e2)
			install(t, peer, image(t, primary.s))
			before := image(t, peer).Generation()
			if change == "static" {
				in := staticInput(t)
				in.Identity = "replaced-static"
				s, err := PrepareStatic(t.Context(), in)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Release()
				if err := follower.host.ReplaceStatic(t.Context(), s); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := follower.license.Set("", ""); err != nil {
					t.Fatal(err)
				}
				if err := follower.license.Set(follower.f.raw, "test"); err != nil {
					t.Fatal(err)
				}
				if change == "license-observed" {
					if err := follower.host.ObserveLicense(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			observe(t, follower)
			observe(t, follower)
			blocked := capture(t, follower)
			if blocked.Auth.License.Providers || !follower.host.value.Load().baseline[e2] {
				t.Fatal("pre-taint installed activation cleared follower fence")
			}
			blocked.Release()
			if !before.Same(image(t, peer).Generation()) {
				t.Fatal("tainted follower observation wrote durable state")
			}
			e3 := reset(t, primary)
			accept(t, primary, e3)
			install(t, peer, image(t, primary.s))
			observe(t, follower)
			recovered := capture(t, follower)
			if !recovered.Auth.License.Providers {
				t.Fatal("new primary activation failed to clear actual baseline")
			}
			recovered.Release()
		})
	}
}

func TestPinnedCurrentLifetimeDeadlineCancellationAndClosedBorrowedResources(t *testing.T) {
	x := newFixture(t, staticInput(t))
	_ = reset(t, x)
	await(t, func() bool {
		return x.resources.WithCurrent(t.Context(), func(_ *providerresources.Pinned) error { return nil }) == nil
	})
	var held func() bool
	if err := x.resources.WithCurrent(t.Context(), func(_ *providerresources.Pinned) error {
		return x.host.PinHostCurrent(t.Context(), func(current func() bool) error {
			held = current
			if !current() {
				t.Fatal("pinned entry not current")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if held() {
		t.Fatal("predicate escaped synchronous pin lifetime")
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := x.resources.WithCurrent(ctx, func(_ *providerresources.Pinned) error {
		return x.host.PinHostCurrent(ctx, func(current func() bool) error {
			cancel()
			if current() {
				t.Fatal("predicate ignores original pin cancellation")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := x.resources.WithCurrent(t.Context(), func(_ *providerresources.Pinned) error {
		return x.host.PinHostCurrent(t.Context(), func(current func() bool) error {
			x.f.clock.Store(x.host.value.Load().until.UnixNano())
			if current() {
				t.Fatal("pinned deadline not checked per call")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	x.stop()
	_ = x.coordinator.Close(t.Context())
	x.publisher.Close()
	if err := x.host.Close(t.Context()); err != nil {
		t.Fatal("host cannot close after coordinator closes resources", err)
	}
	if err := x.host.PinHostCurrent(t.Context(), func(func() bool) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal("closed host allowed pin", err)
	}
}

func TestBoundedFollowerHistoryAndGenerationOverflow(t *testing.T) {
	x := newFixture(t, staticInput(t))
	g := x.host.value.Load()
	// Private white-box fault setup, performed under actual resource/host guards.
	if err := x.host.mutate(t.Context(), func(_ *providerresources.Replacement, _ *generation, _ license.Cut) error {
		n, err := copyGeneration(g)
		if err != nil {
			return err
		}
		for i := 0; i < providerregistry.MaxAuthorities; i++ {
			addBaseline(n, fmt.Sprintf("history-%d", i))
		}
		n.entitlement = "last-activation"
		fence(n)
		if len(n.baseline) != providerregistry.MaxAuthorities || !n.historyFull || n.phase != waiting {
			t.Fatal("history cap pruned evidence or exceeded bound")
		}
		o := *n
		o.serial = math.MaxUint64
		if _, err := copyGeneration(&o); !errors.Is(err, ErrBounds) {
			t.Fatal("generation wrapped", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
