package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Actual root construction, publisher and elected RunWriter mount. No direct
// call to bootstrap, step, runAnchorUpdater or builder options enables serving.
func directAnchorRuntime(t *testing.T, endpoint string, seeds []string) (*servingRuntime, context.Context) {
	t.Helper()
	base := newServingRuntimeFixture(t)
	cfg, err := config.FromFlat(config.Flatten(base.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	cfg.EnvLocked = append(cfg.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
	opts := base.opts
	opts.Config = cfg
	startup, finish := context.WithTimeout(t.Context(), 5*time.Second)
	s, err := newServingRuntime(startup, opts)
	finish()
	if err != nil {
		t.Fatal(err)
	}
	if !s.anchors.admission.current() {
		t.Fatal("successful direct input retained canceled startup lifetime")
	}
	copyAdmission := *s.anchors.admission
	if copyAdmission.current() {
		t.Fatal("equal fields fabricated root admission")
	}
	cfg.Upstreams.Recursion.ManagedTopology = "disabled"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	runServingFixture(t, s)
	parent, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	carried := make(chan context.Context, 1)
	joined := make(chan error, 1)
	go func() {
		joined <- s.opts.Store.Elector("direct-anchor-test").Run(parent, func(ctx context.Context) { carried <- ctx; <-ctx.Done() }, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-joined:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("direct elector failed to join")
		}
	})
	select {
	case ctx := <-carried:
		return s, ctx
	case <-parent.Done():
		t.Fatal("direct elected writer unavailable")
		return nil, nil
	}
}

func startDirectAnchorWriter(original context.Context, t *testing.T, s *servingRuntime) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(original)
	done := make(chan error, 1)
	go func() { done <- s.RunWriter(ctx) }()
	joined := false
	stop := func() {
		if joined {
			return
		}
		select {
		case err := <-done:
			joined = true
			cancel()
			t.Error("direct writer stopped before caller cancellation", err)
			return
		default:
		}
		token, carried := sqlite.WriterTokenFromContext(original)
		cancel()
		select {
		case err := <-done:
			joined = true
			if err != nil && !errors.Is(err, context.Canceled) {
				// The existing primary driver can select a generation wake as
				// caller cancellation arrives, then report unavailable from its
				// original writer predicate. Accept only that documented joined
				// outcome with the same actual writer and canceled child.
				a := s.opts.Store.Authority()
				if !errors.Is(err, primaryruntime.ErrUnavailable) || ctx.Err() != context.Canceled || original.Err() != nil || !carried || !a.Term.Same(token.Term()) || a.Incarnation != token.Incarnation() {
					t.Error("direct writer", err)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("direct writer did not join")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestDirectAnchorActualWriterBootstrapAndCompletePublication(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	requested, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	endpoint, reads := anchorWireServer(t, "tcp", func(raw []byte) []byte {
		close(requested)
		<-release
		return reply(raw)
	})
	s, original := directAnchorRuntime(t, endpoint.String(), seed.DS)
	stop := startDirectAnchorWriter(original, t, s)
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("original bootstrap request did not arrive")
	}
	if s.primary.driver.Status().Running {
		t.Fatal("primary startup writes can race the original bootstrap capture")
	}
	unblock()
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	owner := anchorOperationOwner(original, t, s)
	if owner.Inspect().Phase != accepted.Managed || reads.Load() != 1 {
		t.Fatal("actual elected mount did not bootstrap once", reads.Load())
	}
	lease, err := s.publisher.CaptureHTTP(original)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	part, ok := lease.Bundle().Component(recursiveRootComponent)
	seal, typed := part.(*recursiveRootSeal)
	if !ok || !typed || seal.managed == nil || seal.static != nil || !seal.current() {
		t.Fatal("complete publication omitted admitted managed root")
	}
	read, err := seal.captureRead(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := seal.withRead(read, func(check func() error) error { return check() }); err != nil {
		t.Fatal("managed output admission", err)
	}
	stop()
	if reads.Load() != 1 {
		t.Fatal("shutdown acquired a replacement observation")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if seal.current() || s.anchors.admission.current() {
		t.Fatal("runtime shutdown retained root admission")
	}
}

func TestDirectAnchorActualWriterTerminalKeepsOtherServices(t *testing.T) {
	seed, _ := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", func([]byte) []byte { return []byte{1} })
	s, original := directAnchorRuntime(t, endpoint.String(), seed.DS)
	before := anchorOperationOwner(original, t, s).Version()
	for range 2 {
		stop := startDirectAnchorWriter(original, t, s)
		waitAnchorWriter(t, s, true)
		awaitServingRuntime(t, s, func() bool { return s.primary.driver.Status().Ready })
		if reads.Load() != 1 || anchorOperationOwner(original, t, s).Version() != before || s.publisher.Status().ProtectedReady {
			t.Fatal("terminal writer replayed, changed history or enabled publication")
		}
		stop()
	}
}

func TestDirectAnchorDisabledDoesNotGrantManagedPublication(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	s, original, _, _ := anchorOperationFixture(t, endpoint.String(), seed.DS)
	if s.anchors.admission != nil {
		t.Fatal("ordinary configuration fabricated direct admission")
	}
	stop := startDirectAnchorWriter(original, t, s)
	awaitServingRuntime(t, s, func() bool { return s.primary.driver.Status().Ready })
	stop()
	if reads.Load() != 0 || s.publisher.Status().ProtectedReady {
		t.Fatal("disabled runtime acquired or published managed state")
	}
	if lease, err := s.publisher.Capture(); err == nil {
		lease.Release()
		t.Fatal("disabled managed capture succeeded")
	}
}

func TestDirectAnchorEnrolledFollowerWaitsWithoutInitializing(t *testing.T) {
	base := newServingRuntimeFixture(t)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "direct-follower"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	term := replication.Term{ClusterID: "direct-follower-cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "initial"}
	if err := peer.Enroll(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	before := peer.Authority()
	cfg, err := config.FromFlat(config.Flatten(base.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	opts := base.opts
	opts.Store, opts.Config = peer, cfg
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal("direct follower startup", err)
	}
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	runServingFixture(t, s)
	ready, err := s.anchorFollowerReady(t.Context(), followerEpoch(before))
	if err != nil || ready || s.publisher.Status().ProtectedReady {
		t.Fatal("empty enrolled follower initialized managed history", ready, err)
	}
	after := peer.Authority()
	if !after.Term.Same(before.Term) || after.Mode != before.Mode || after.Incarnation != before.Incarnation || after.Installed != before.Installed {
		t.Fatal("waiting direct startup changed original follower authority")
	}
}

// restartDueAnchorWriter bootstraps a direct managed writer once, stops it,
// moves its updater wall clock just past the committed refresh and starts it
// again; the root holds the restarted writer's due poll (reply 2) until the
// returned release runs. It returns the canonical generation before the
// restart and the root's read count.
func restartDueAnchorWriter(t *testing.T, replies int) (s *servingRuntime, original context.Context, before replication.Generation, reads *atomic.Int32, release func()) {
	t.Helper()
	var wall anchorAheadWall
	requested, held := make(chan struct{}), make(chan struct{})
	release = sync.OnceFunc(func() { close(held) })
	endpoint, seeds, reads := anchorOperationServer(t, replies, func(n int) {
		if n == 2 {
			wall.ahead.Store(0) // later steps read the real schedule again
			close(requested)
			<-held
		}
	})
	s, original = directAnchorRuntime(t, endpoint, seeds)
	s.anchorUpdaterWall = wall.now
	stop := startDirectAnchorWriter(original, t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady && s.primary.driver.Status().Running })
	if reads.Load() != 1 {
		t.Fatal("fixture did not bootstrap once", reads.Load())
	}
	stop()
	state, err := anchorOperationOwner(original, t, s).State()
	if err != nil {
		t.Fatal(err)
	}
	wall.ahead.Store(int64(time.Until(state.Inspect().Schedule.NextRefresh) + time.Second))
	image, err := s.opts.Store.ReadGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	startDirectAnchorWriter(original, t, s)
	// Cleanups run last-registered first: release the held reply before the
	// writer is stopped and the root fixture joins its handler.
	t.Cleanup(release)
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("restarted writer did not poll the root")
	}
	return s, original, image.Generation(), reads, release
}

// requireWriterServicesHeld requires that, for hold, the restarted writer's
// other elected services did not start and nothing committed over the
// generation its due poll captured. The fixture's connection deadline is 4 s.
func requireWriterServicesHeld(t *testing.T, s *servingRuntime, original context.Context, before replication.Generation, hold time.Duration) {
	t.Helper()
	for end := time.Now().Add(hold); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if s.primary.driver.Status().Running {
			t.Fatal("writer services started while a due root poll was in flight")
		}
	}
	image, err := s.opts.Store.ReadGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	if !image.Generation().Same(before) {
		t.Fatal("a startup write committed while a due root poll was in flight")
	}
}

// A restarted (or promoted) managed writer whose committed refresh is due
// keeps its first step ahead of the other elected services, as before the
// bootstrap: the reply may be a REVOKE, and a revocation denial that a
// startup write rolls back is terminal for the writer term
// (TestAnchorUpdaterRacedRevokeDenialStaysTerminal). The poll commits exactly
// once with no raced retry, then the services start (F28-OPEN6).
func TestDirectAnchorRestartedWriterDuePollPrecedesWriterServices(t *testing.T) {
	s, original, before, reads, release := restartDueAnchorWriter(t, 3)
	requireWriterServicesHeld(t, s, original, before, 1500*time.Millisecond)
	release()
	awaitServingRuntime(t, s, func() bool {
		view := s.anchorStatus.snapshot().updater
		return s.primary.driver.Status().Running && view.state == anchorWorkerRunning && view.next.After(time.Now().Add(30*time.Minute))
	})
	if s.anchorWriter.terminalError() != nil {
		t.Fatal("released poll stopped the updater", s.anchorWriter.terminalError())
	}
	if n := reads.Load(); n != 2 {
		t.Fatal("due poll was not exactly one acquisition", n)
	}
}

// The reviewer's case for F28-OPEN6: the restarted writer's due poll observes
// a REVOKE of the sole key while it is held. No startup write may race it,
// so the revocation commits (the key is Revoked, the owner moved) and the
// updater is not terminal; then the writer services start. With the startup
// gate released before a due first step, this ended 3 of 3 with a terminal
// updater and the revocation uncommitted.
func TestDirectAnchorRestartedWriterDueRevokeCommitsBeforeWriterServices(t *testing.T) {
	s, original, before, reads, release := restartDueAnchorWriter(t, 2)
	requireWriterServicesHeld(t, s, original, before, 1500*time.Millisecond)
	release()
	awaitServingRuntime(t, s, func() bool {
		if s.anchorWriter.terminalError() != nil {
			return true
		}
		state, err := anchorOperationOwner(original, t, s).State()
		if err != nil {
			return false
		}
		keys := state.Inspect().Keys
		return len(keys) == 1 && keys[0].Status == trustanchors.Revoked
	})
	if err := s.anchorWriter.terminalError(); err != nil {
		t.Fatal("the restarted writer's REVOKE was not committed; the updater stopped", err)
	}
	if n := reads.Load(); n != 2 {
		t.Fatal("REVOKE poll was not exactly one acquisition", n)
	}
	awaitServingRuntime(t, s, func() bool { return s.primary.driver.Status().Running })
}

// The same holds when the restarted (or promoted) writer's first step is not
// due: the services do not wait for its capture either (F28-OPEN6). The
// capture is held here right after its image read.
func TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 3, nil)
	s, original := directAnchorRuntime(t, endpoint, seeds)
	stop := startDirectAnchorWriter(original, t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady && s.primary.driver.Status().Running })
	stop()
	if reads.Load() != 1 {
		t.Fatal("fixture did not bootstrap once", reads.Load())
	}
	held, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var once sync.Once
	s.anchorCaptureRead = func() {
		once.Do(func() {
			close(held)
			<-release
		})
	}
	startDirectAnchorWriter(original, t, s)
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("restarted writer did not capture")
	}
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady && s.primary.driver.Status().Running })
	unblock()
	awaitServingRuntime(t, s, func() bool {
		view := s.anchorStatus.snapshot().updater
		return view.state == anchorWorkerRunning && view.next.After(time.Now().Add(30*time.Minute))
	})
	if s.anchorWriter.terminalError() != nil || reads.Load() != 1 {
		t.Fatal("restarted writer stopped or acquired the root again", s.anchorWriter.terminalError(), reads.Load())
	}
}
