package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

func startAnchorLossRuntime(ctx context.Context, t *testing.T, opts servingRuntimeOptions) (*servingRuntime, func()) {
	t.Helper()
	s, err := newServingRuntime(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(life) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("all-loss runtime completion", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("all-loss runtime did not join")
		}
		cleanup, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		if err := s.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(stop)
	return s, stop
}

func captureAnchorLossWriter(ctx context.Context, t *testing.T, s *servingRuntime) (context.Context, serving.TermActivation, providercoordinator.HostCapture, func()) {
	t.Helper()
	life, cancel := context.WithTimeout(ctx, 25*time.Second)
	carried := make(chan context.Context, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("canonical-anchor-loss").Run(life, func(original context.Context) {
			carried <- original
			<-original.Done()
		}, nil)
	}()
	var original context.Context
	select {
	case original = <-carried:
	case <-life.Done():
		t.Fatal("all-loss fixture did not receive original writer")
	}
	token, ok := sqlite.WriterTokenFromContext(original)
	if !ok {
		t.Fatal("all-loss elector omitted original token")
	}
	target := serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}
	host, err := s.resetWriter(original, target)
	if err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("all-loss elector completion", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("all-loss elector did not join")
		}
		if host.Release != nil {
			host.Release()
		}
	})
	t.Cleanup(stop)
	if err := s.prepareAnchorWriter(original, target, host); err != nil {
		t.Fatal(err)
	}
	return original, target, host, stop
}

// Invoke two independent real root caller operations under an actual elector;
// this does not accelerate the scheduler's one-hour minimum or forge Samples.
// The second signed response self-revokes the sole anchor. Its durable denial
// must drain a real output write, then the paired update must retain quarantine
// and the permanent tombstone through a physical same-database restart.
func TestAnchorCanonicalLossDrainsOutputAndSurvivesActualRestart(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
	base := newServingRuntimeFixture(t)
	opts := base.opts
	cfg, err := config.FromFlat(config.Flatten(opts.Config))
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
	opts.Config = cfg
	s, stopRuntime := startAnchorLossRuntime(t.Context(), t, opts)
	original, target, host, stopWriter := captureAnchorLossWriter(t.Context(), t, s)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := s.captureAnchorOperation(original, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := bootstrap.run(); err != nil || result.Update.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("original all-loss bootstrap", err)
	}
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	lease, err := s.publisher.CaptureHTTP(original)
	if err != nil {
		t.Fatal(err)
	}
	output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: original, Serving: servingdns.Wrap(lease)})
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	defer lease.Release()
	op, err := s.captureAnchorOperation(original, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	writer, reader := net.Pipe()
	defer func() { _ = writer.Close(); _ = reader.Close() }()
	deadline, _ := original.Deadline()
	_ = writer.SetDeadline(deadline)
	_ = reader.SetDeadline(deadline)
	entered := make(chan struct{})
	emitted := make(chan error, 1)
	go func() {
		emitted <- output.Emit(func() error { close(entered); _, err := writer.Write([]byte("original")); return err })
	}()
	select {
	case <-entered:
	case <-original.Done():
		t.Fatal("actual managed output did not enter")
	}
	type result struct {
		completion anchorCompletion
		err        error
	}
	finished := make(chan result, 1)
	go func() { c, err := op.run(); finished <- result{c, err} }()
	for reads.Load() != 2 {
		select {
		case out := <-finished:
			t.Fatal("all-loss operation missed original wire acquisition", out.err)
		case <-original.Done():
			t.Fatal("all-loss original acquisition expired")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case out := <-finished:
		t.Fatal("all-loss denial crossed the active output write", out.err)
	case <-time.After(40 * time.Millisecond):
	}
	bytes := make([]byte, len("original"))
	if _, err := io.ReadFull(reader, bytes); err != nil || string(bytes) != "original" {
		t.Fatal("original managed output failed", err)
	}
	if err := <-emitted; err != nil {
		t.Fatal(err)
	}
	var out result
	select {
	case out = <-finished:
	case <-original.Done():
		t.Fatal("all-loss operation did not join after output drained")
	}
	if out.err != nil || out.completion.Denial.Outcome() != sqlite.AnchorCommitted || out.completion.Update.Outcome() != sqlite.AnchorCommitted || reads.Load() != 2 {
		t.Fatal("all-loss phases lost actual committed dispositions", out.err)
	}
	if err := output.Next(); err == nil {
		t.Fatal("retired transfer continued after canonical all-anchor loss")
	}
	owner := anchorOperationOwner(original, t, s)
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	pure := state.Inspect()
	if !pure.Quarantined || len(pure.Keys) != 1 || pure.Keys[0].Status != trustanchors.Revoked {
		t.Fatal("canonical all-loss lost quarantine or permanent tombstone")
	}
	version := owner.Version()
	encoded, err := trustanchors.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, s, func() bool { return !s.publisher.Status().ProtectedReady })
	lease.Release()
	stopWriter()
	stopRuntime()
	if err := base.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := opts.Store.Path()
	if err := opts.Store.Close(); err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"managed", "static", "disabled"} {
		peer, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = peer.Close() })
		restartCfg, err := config.FromFlat(config.Flatten(cfg))
		if err != nil {
			t.Fatal(err)
		}
		restartCfg.EnvLocked = append([]string(nil), cfg.EnvLocked...)
		switch mode {
		case "static":
			restartCfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsStatic
			restartCfg.Upstreams.Recursion.TrustAnchors = nil
		case "disabled":
			restartCfg.Upstreams.Recursion.ManagedTopology = "disabled"
		}
		restartOpts := opts
		restartOpts.Store, restartOpts.Config = peer, restartCfg
		restartOpts.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
		restarted, stop := startAnchorLossRuntime(t.Context(), t, restartOpts)
		awaitServingRuntime(t, restarted, func() bool { return restarted.publisher.Status().Reason == "prepare_failed" })
		if lease, err := restarted.publisher.CaptureHTTP(t.Context()); err == nil {
			lease.Release()
			t.Fatal("quarantined restart published managed or fallback trust", mode)
		}
		current := anchorOperationOwner(t.Context(), t, restarted)
		currentState, err := current.State()
		if err != nil {
			t.Fatal(err)
		}
		currentBytes, err := trustanchors.Encode(currentState)
		if err != nil || current.Version() != version || string(currentBytes) != string(encoded) || reads.Load() != 2 {
			t.Fatal("restart changed canonical checkpoint or reacquired default trust", mode, err)
		}
		stop()
		if err := peer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
