package main

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

func startAnchorFollowerFixture(ctx context.Context, t *testing.T, f *anchorFollowerFixture) func() {
	t.Helper()
	if err := f.s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromFlat(config.Flatten(f.runtime.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.EnvLocked = append([]string(nil), f.runtime.Config.EnvLocked...)
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	f.runtime.Config = cfg
	f.s, err = newServingRuntime(ctx, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.s.Run(run) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("follower runtime completion", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("follower supervisor did not join")
		}
	})
	t.Cleanup(stop)
	return stop
}

func TestAnchorFollowerSupervisorActualDenialRetiresPublishedInput(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	endpoint, seeds, reads := anchorOperationServer(t, 2, func(n int) {
		if n == 2 {
			close(entered)
			<-release
		}
	})
	f, ctx, _ := anchorFollowerPollerFixture(t, endpoint, seeds)
	before, authority := anchorFollowerImage(ctx, t, f.store), f.store.Authority()
	stop := startAnchorFollowerFixture(ctx, t, f)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("mounted follower never acquired its independent receipt")
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)})
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	awaitServingRuntime(t, f.s, func() bool { return !f.s.publisher.Status().ProtectedReady })
	if err := output.Emit(func() error { t.Error("old follower input emitted after durable denial"); return nil }); err == nil {
		t.Fatal("old output attempt survived local denial")
	}
	if reads.Load() != 2 || !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || !reflect.DeepEqual(authority, f.store.Authority()) {
		t.Fatal("mounted follower changed canonical history or authority")
	}
	stop()
	f.reopen(ctx, t)
	_, token, err := f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CaptureAnchorPublication(ctx, token); !errors.Is(err, sqlite.ErrAnchorUnavailable) {
		t.Fatal("actual restart lost mounted follower's durable denial", err)
	}
}

// This server continues accepting after a malformed receipt, so an unintended
// third acquisition is observable rather than hidden by a one-shot listener.
func anchorSupervisorServer(t *testing.T, after func(int32, *dns.Msg)) (string, []string, *atomic.Int32) {
	t.Helper()
	seed, reply := anchorWireSignedReply(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	started := make(chan struct{})
	server := &dns.Server{Listener: listener, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		raw, err := q.Pack()
		if err != nil {
			t.Error(err)
			return
		}
		m := new(dns.Msg)
		if err := m.Unpack(reply(raw)); err != nil {
			t.Error(err)
			return
		}
		after(reads.Add(1), m)
		_ = w.WriteMsg(m) // cancellation may close this original socket
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.ShutdownContext(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("follower authority fixture did not join")
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("follower authority fixture did not start")
	}
	return listener.Addr().String(), seed.DS, &reads
}

func installAnchorFollower(ctx context.Context, t *testing.T, f *anchorFollowerFixture, primary *servingRuntime) *replication.Image {
	t.Helper()
	original := captureServingReplica(t, f.store)
	expected := f.store.Authority().Installed
	image, err := primary.opts.Store.ReadGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := f.store.ValidateImage(original, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.InstallReplica(original, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: expected}); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	installed := anchorFollowerImage(ctx, t, f.store)
	if !installed.Generation().Same(image.Generation()) || installed.ObjectResourceVersion != image.ObjectResourceVersion {
		t.Fatal("follower installed a different source generation")
	}
	return installed
}

func TestAnchorFollowerSupervisorWaitsForManagedInstall(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(_ int32, _ *dns.Msg) {})
	primary, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	f := newAnchorFollowerFixture(ctx, t, primary) // actual dormant image
	stop := startAnchorFollowerFixture(ctx, t, f)
	ready, err := f.s.anchorFollowerReady(ctx, followerEpoch(f.store.Authority()))
	if err != nil || ready || reads.Load() != 0 {
		t.Fatal("dormant image acquired trust before primary bootstrap", ready, err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	op, err := primary.captureAnchorOperation(ctx, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := op.run(); err != nil || result.Update.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("primary bootstrap", err)
	}
	installed := installAnchorFollower(ctx, t, f, primary)
	awaitServingRuntime(t, f.s, func() bool { return reads.Load() == 2 && f.s.publisher.Status().ProtectedReady })
	stop()
	if !anchorFollowerSameImage(installed, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("follower normal poll advanced installed canonical history")
	}
}

func TestAnchorFollowerSupervisorTerminalSurvivesGenerationInstall(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(n int32, m *dns.Msg) {
		if n == 2 {
			m.Id ^= 1
		}
	})
	primary, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	advance := func() {
		op, err := primary.captureAnchorOperation(ctx, target, host, clock, 0)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := op.run(); err != nil || result.Update.Outcome() != sqlite.AnchorCommitted {
			t.Fatal("original primary operation", err)
		}
	}
	advance()
	f := newAnchorFollowerFixture(ctx, t, primary)
	stop := startAnchorFollowerFixture(ctx, t, f)
	awaitServingRuntime(t, f.s, func() bool { return reads.Load() >= 2 })
	// Wait for the malformed original receipt to reach terminal processing,
	// then install a genuinely new complete owner in the same follower epoch.
	time.Sleep(100 * time.Millisecond)
	advance()
	installed := installAnchorFollower(ctx, t, f, primary)
	time.Sleep(1200 * time.Millisecond)
	stop()
	if reads.Load() != 3 || !anchorFollowerSameImage(installed, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("ordinary installed generation rescued a terminal follower poll", reads.Load())
	}
}

func TestAnchorFollowerSupervisorPromotionJoinsHeldAcquisition(t *testing.T) {
	entered, closed := make(chan struct{}), make(chan error, 1)
	endpoint, seeds, reads := anchorFollowerPollerServer(t, func(conn net.Conn, _ []byte) []byte {
		close(entered)
		var b [1]byte
		_, err := conn.Read(b[:])
		closed <- err
		return nil
	})
	f, ctx, _ := anchorFollowerPollerFixture(t, endpoint, seeds)
	stop := startAnchorFollowerFixture(ctx, t, f)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("follower did not enter held original acquisition")
	}
	a := f.store.Authority()
	if _, err := f.store.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed, RequestID: "supervisor-held"}); err != nil {
		t.Fatal(err)
	}
	before := anchorFollowerImage(ctx, t, f.store)
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatal("promotion did not close original follower socket", err)
		}
	case <-ctx.Done():
		t.Fatal("promoted follower retained original socket")
	}
	stop() // role change already canceled the socket, before runtime shutdown
	if reads.Load() != 2 || !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("old follower continued under promoted authority")
	}
}

// Every follower is fenced from the moment it observes a higher term until it
// installs that term's image, and stays fenced if it cannot. Before D369 the
// supervisor ran for the follower role only, so that member served nothing and
// watched nothing. It now keeps serving its last accepted managed owner as
// DNS-only data under a new child for the fenced role, whose independent signed
// REVOKE still retires the output durably, including across a physical reopen
// of the fenced database.
func TestAnchorFollowerSupervisorFencedRoleKeepsDenyOnlyPollingAcrossReopen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	endpoint, seeds, reads := anchorOperationServer(t, 3, func(n int) {
		if n == 3 {
			close(entered)
			<-release
		}
	})
	f, ctx, _ := anchorFollowerPollerFixture(t, endpoint, seeds)
	stop := startAnchorFollowerFixture(ctx, t, f)
	awaitServingRuntime(t, f.s, func() bool { return reads.Load() == 2 && f.s.publisher.Status().ProtectedReady })
	before := anchorFollowerImage(ctx, t, f.store)
	term := f.store.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "successor", "later"
	if _, err := f.store.ObserveAuthority(ctx, term); err != nil {
		t.Fatal(err)
	}
	fenced := f.store.Authority()
	if fenced.Mode != replication.Fenced || !fenced.Installed.Same(before.Generation()) {
		t.Fatal("fixture did not fence the follower on its installed image", fenced.Mode)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fenced follower never acquired its own independent receipt")
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().Reason == "dns_only" })
	if _, err := f.s.publisher.CaptureHTTP(ctx); err == nil {
		t.Fatal("fenced follower admitted protected HTTP")
	}
	lease, err := f.s.publisher.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	request := func() *pipeline.Request { return &pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)} }
	served, err := captureRootAnchorOutput(request())
	if err != nil {
		t.Fatal(err)
	}
	if err := served.Emit(func() error { return nil }); err != nil {
		t.Fatal("fenced follower refused its last-good DNS-only output", err)
	}
	held, err := captureRootAnchorOutput(request())
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	denied := func() bool {
		_, token, err := f.store.ReadServingCandidate(ctx)
		if err != nil {
			return false
		}
		_, err = f.store.CaptureAnchorPublication(ctx, token)
		return errors.Is(err, sqlite.ErrAnchorUnavailable)
	}
	awaitServingRuntime(t, f.s, denied)
	if err := held.Emit(func() error { t.Error("fenced follower emitted after durable denial"); return nil }); err == nil {
		t.Fatal("held fenced output survived local denial")
	}
	if _, err := captureRootAnchorOutput(request()); err == nil {
		t.Fatal("retired fenced input admitted a new output attempt")
	}
	if reads.Load() != 3 || !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || !reflect.DeepEqual(fenced, f.store.Authority()) {
		t.Fatal("fenced supervision changed canonical history or authority", reads.Load())
	}
	stop()
	// Startup admission accepts the fenced database; the denial is still there.
	f.reopen(ctx, t)
	reopened := f.store.Authority()
	if reopened.Mode != replication.Fenced || !reopened.Term.Same(fenced.Term) || !reopened.Installed.Same(fenced.Installed) || !f.s.anchors.admission.current() || !denied() {
		t.Fatal("fenced reopen lost its authority, admission or durable denial", reopened.Mode)
	}
	if reads.Load() != 3 {
		t.Fatal("fenced reopen acquired without a running supervisor", reads.Load())
	}
}
