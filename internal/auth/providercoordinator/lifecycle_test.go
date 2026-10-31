package providercoordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func waitWriter(t *testing.T, f *fixture) {
	t.Helper()
	wait(t, func() bool {
		f.coordinator.mu.Lock()
		defer f.coordinator.mu.Unlock()
		return f.coordinator.writerRunning
	})
}

func TestRunWriterAutomaticAdmissionCancellationAndCloseOwnership(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	ctx, target := writer(t, f.store)
	ctx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- f.coordinator.RunWriter(ctx, target) }()
	waitWriter(t, f)
	// Advance the injected actual clock to allow bounded retries after RunWriter's
	// first no-op while the background worker obtains its signed-key authority.
	wait(t, func() bool {
		f.clock.Add(int64(50 * time.Millisecond))
		return len(f.registry(t).Authorities()) == 1
	})
	ready(t, f, 1)
	f.clock.Add(int64(2 * time.Second))
	proof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now())}
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	component, _ := lease.Bundle().Component("auth")
	_, err = component.(*servingauth.View).Authenticate(t.Context(), proof, f.host.now())
	lease.Release()
	if err != nil {
		t.Fatal("automatically admitted signed proof denied", err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("writer cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer failed to join")
	}
	// A second invocation retains its own explicit original writer context.
	ctx2, target2 := writer(t, f.store)
	go func() { result <- f.coordinator.RunWriter(ctx2, target2) }()
	waitWriter(t, f)
	if err := f.coordinator.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrClosed) {
			t.Fatal("Close did not cancel current writer", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close returned before writer joined")
	}
	f.publisher.Close()
	wait(t, func() bool { return f.host.refs.Load() == 0 })
	if _, err := f.store.ReadGeneration(t.Context()); err != nil {
		t.Fatal("Close closed root-owned Store", err)
	}
	if _, err := f.coordinator.Capture(t.Context(), nil); !errors.Is(err, ErrClosed) {
		t.Fatal("closed coordinator accepted capture", err)
	}
}

func TestActualRestartFollowerInstallAndPromotionPreserveAdmittedAuthority(t *testing.T) {
	server := newProviderServer(t)
	primary := newFixture(t, server)
	accept(t, primary, candidates(t, primary, 1))
	ready(t, primary, 1)
	image, err := primary.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest := primary.registry(t).Digest()
	path := primary.store.Path()
	primary.stop()
	if err := primary.coordinator.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	primary.publisher.Close()
	if err := primary.store.Close(); err != nil {
		t.Fatal(err)
	}
	resumed := &fixture{store: openStore(t, path, replication.Standalone), wire: primary.wire}
	resumed.clock.Store(primary.clock.Load())
	startFixture(t, resumed, server)
	ready(t, resumed, 1)
	if resumed.registry(t).Digest() != digest || len(resumed.resources.TakeCandidates()) != 0 {
		t.Fatal("restart fabricated or replaced admitted authority")
	}
	followerStore := openStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := followerStore.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := followerStore.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	a := followerStore.Authority()
	installed, err := followerStore.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed})
	if err != nil {
		t.Fatal(err)
	}
	follower := &fixture{store: followerStore, wire: primary.wire}
	follower.clock.Store(primary.clock.Load())
	startFixture(t, follower, server)
	ready(t, follower, 1)
	before, _ := followerStore.ReadGeneration(t.Context())
	proof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, follower.host.now().Add(-time.Minute))}
	if _, err := follower.view(t).Authenticate(t.Context(), proof, follower.host.now()); err != nil {
		t.Fatal("matching follower authority unavailable", err)
	}
	if _, err := followerStore.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower received writer authority", err)
	}
	after, _ := followerStore.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || follower.registry(t).Digest() != digest || len(follower.resources.TakeCandidates()) != 0 {
		t.Fatal("follower acquisition rewrote or self-admitted authority")
	}
	if _, err := followerStore.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "coordinator-promotion"}); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { return follower.publisher.Status().ProtectedReady })
	ready(t, follower, 1)
	ctx, target := writer(t, followerStore)
	if err := follower.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		_, err := servingauth.AuthenticateCurrent(ctx, tx, deps, proof, follower.host.now())
		return err
	}); err != nil {
		t.Fatal("promoted explicit current command denied", err)
	}
	if follower.registry(t).Digest() != digest {
		t.Fatal("promotion changed durable receipt")
	}
}

func mutateProviders(t *testing.T, f *fixture, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	ctx, target := writer(t, f.store)
	if err := f.resources.WithReplacement(ctx, func(r *providerresources.Replacement) error {
		err := f.host.pin(ctx, func() error { return f.store.Command(ctx, target.Term, fn) })
		r.Retire()
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPartialReplacementPreservesOutageReceiptAndRetiresObjectIncarnation(t *testing.T) {
	a, b := newProviderServer(t), newProviderServer(t)
	f := newFixture(t, a, b)
	accept(t, f, candidates(t, f, 2))
	ready(t, f, 2)
	old := f.capture(t)
	defer old.Release()
	previous := f.registry(t).Authorities()
	b.status.Store(503)
	mutateProviders(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var idp publicv1.IdentityProvider
		if err := tx.Get(ctx, store.Key{Kind: "IdentityProvider", Name: "corp0"}, &idp); err != nil {
			return err
		}
		idp.Spec.Scopes = []string{"email"}
		_, err := tx.Apply(ctx, &idp, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	accept(t, f, candidates(t, f, 1))
	var unaffected providerregistry.AuthorityRecord
	for _, r := range previous {
		if r.Provider.ID == "corp1" {
			unaffected = r
		}
	}
	preserved := false
	for _, r := range f.registry(t).Authorities() {
		if r.Provider.ID == "corp1" && r != unaffected {
			t.Fatal("partial acquisition retired or rewrote unchanged outage receipt")
		}
		preserved = preserved || r == unaffected
	}
	if !preserved {
		t.Fatal("unchanged outage authority omitted from complete inventory")
	}
	if old.Current() {
		t.Fatal("changed provider retained old cut")
	}
	mutateProviders(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.Key{Kind: "IdentityProvider", Name: "corp0"}, store.DeleteOptions{}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp0", CreationTimestamp: metav1.NewTime(f.host.now())}, Spec: a.spec()}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	accept(t, f, candidates(t, f, 1))
	active := 0
	retiredUID := ""
	for _, r := range f.registry(t).Authorities() {
		if r.Active {
			active++
		} else if r.Provider.ID == "corp0" {
			retiredUID = r.Provider.UID
		}
	}
	if active != 2 || retiredUID == "" {
		t.Fatal("recreated object leaked historical active authority")
	}
	image, _ := f.store.ReadGeneration(t.Context())
	s, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	floor := false
	for _, r := range s.ProviderFloors {
		floor = floor || (r.Provider == "corp0" && r.UID == retiredUID && !r.ValidAfter.Before(f.host.now()))
	}
	if !floor {
		t.Fatal("retired object UID missing durable floor")
	}
}

func TestChangesBroadcastAndCompleteShadowUnavailableInventory(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	first, second := f.coordinator.Changes(), f.coordinator.Changes()
	f.coordinator.signal()
	for _, subscription := range []<-chan struct{}{first, second} {
		select {
		case <-subscription:
		default:
			t.Fatal("multiple subscribers competed for event")
		}
	}
	canonical := server.spec()
	mutateProviders(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.ReconcileProviderConfigs(ctx, tx, []providerregistry.DesiredConfig{{ID: "corp0", Spec: canonical}}, f.host.now())
	})
	// Deliberately mismatched LOCAL spec must remain unavailable; using canonical
	// config bytes instead would conceal missing local authority and revive object fallback.
	local := server.spec()
	local.ClientID = "different-local-client"
	f.host.install(&hostGeneration{id: "local-mismatch", allowed: true, secret: "test-only", config: []providerregistry.DesiredConfig{{ID: "corp0", Spec: local}}})
	wait(t, func() bool {
		image, err := f.store.ReadGeneration(t.Context())
		if err != nil {
			return false
		}
		capture, err := f.coordinator.Capture(t.Context(), image)
		if err != nil {
			return false
		}
		defer capture.Release()
		return len(capture.ProviderResources) == 1 && capture.ProviderResources[0].Ref.Origin == servingauth.ProviderConfig && capture.ProviderResources[0].Availability == servingauth.ProviderUnavailable
	})
	proof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now())}
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	if _, err := f.view(t).Authenticate(t.Context(), proof, f.host.now()); !isDenied(err) {
		t.Fatal("local mismatch fell back to shadowed object", err)
	}
	if _, err := f.view(t).Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}, f.host.now()); err != nil {
		t.Fatal("provider mismatch denied unrelated local token", err)
	}
}
