package providercoordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func candidates(t *testing.T, f *fixture, count int) []*providerresources.AuthorityCandidate {
	t.Helper()
	out := []*providerresources.AuthorityCandidate{}
	wait(t, func() bool { out = append(out, f.resources.TakeCandidates()...); return len(out) >= count })
	return out
}
func ready(t *testing.T, f *fixture, count int) {
	t.Helper()
	wait(t, func() bool {
		if !f.publisher.Status().ProtectedReady {
			return false
		}
		image, err := f.store.ReadGeneration(t.Context())
		if err != nil {
			return false
		}
		capture, err := f.coordinator.Capture(t.Context(), image)
		if err != nil {
			return false
		}
		defer capture.Release()
		if len(capture.ProviderResources) != count {
			return false
		}
		for _, r := range capture.ProviderResources {
			if r.Availability != servingauth.ProviderReady {
				return false
			}
		}
		return true
	})
}
func accept(t *testing.T, f *fixture, cs []*providerresources.AuthorityCandidate) {
	t.Helper()
	ctx, target := writer(t, f.store)
	if err := f.coordinator.admit(ctx, target, cs); err != nil {
		t.Fatal(err)
	}
}

func TestAlwaysOnRunPublishesCompleteUnavailableAndLocalAuthWithoutWrites(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	before, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cs := candidates(t, f, 1)
	if len(cs) != 1 {
		t.Fatal("missing verified candidate")
	}
	after, _ := f.store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("always-on Run changed durable registry")
	}
	capture := f.capture(t)
	defer capture.Release()
	if capture.ConfigProviders == nil || len(capture.ConfigProviders) != 0 || capture.ProviderResources[0].Availability != servingauth.ProviderUnavailable {
		t.Fatal("unadmitted inventory omitted or activated")
	}
	view := f.view(t)
	calls := server.calls.Load()
	if _, err := view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}, f.host.now()); err != nil {
		t.Fatal("local token denied by unavailable provider", err)
	}
	if _, err := view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now().Add(-time.Minute))}, f.host.now()); !isDenied(err) {
		t.Fatal("unadmitted external proof accepted", err)
	}
	if server.calls.Load() != calls {
		t.Fatal("auth path did provider I/O")
	}
	if lease, err := f.publisher.Capture(); err != nil {
		t.Fatal("DNS unavailable", err)
	} else {
		lease.Release()
	}
}

func TestCompleteBothOriginAdmissionDetachedDecisionRollbackAndOldCuts(t *testing.T) {
	a, b := newProviderServer(t), newProviderServer(t)
	f := newFixture(t, a, b)
	f.host.install(&hostGeneration{id: "installation-config", allowed: true, secret: "test-only", config: []providerregistry.DesiredConfig{{ID: "corp0", Spec: a.spec()}}})
	ctxConfig, targetConfig := writer(t, f.store)
	if err := f.resources.WithReplacement(ctxConfig, func(r *providerresources.Replacement) error {
		err := f.host.pin(ctxConfig, func() error {
			return f.store.Command(ctxConfig, targetConfig.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return security.ReconcileProviderConfigs(ctx, tx, []providerregistry.DesiredConfig{{ID: "corp0", Spec: a.spec()}}, f.host.now())
			})
		})
		r.Retire()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	old := f.capture(t)
	defer old.Release()
	ctx, target := writer(t, f.store)
	cs := candidates(t, f, 2)
	before, _ := f.store.ReadGeneration(t.Context())
	original := f.coordinator.opts.Admit
	var held *Admission
	abort := errors.New("test final admission abort")
	f.coordinator.opts.Admit = func(ctx context.Context, tx *sqlite.CommandTx, admission *Admission) error {
		held = admission
		detached := admission.Receipts()
		detached[0].Source.Spec.ClientSecretRef.Env = "detached"
		if admission.Receipts()[0].Source.Spec.ClientSecretRef.Env == "detached" {
			t.Fatal("nested admission output aliases")
		}
		if err := original(ctx, tx, admission); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return abort })
	}
	if err := f.coordinator.admit(ctx, target, cs); err != abort {
		t.Fatal("final hook did not reject staged receipts", err)
	}
	if held.Sources() != nil || held.Receipts() != nil || held.RegistryDigest() != "" {
		t.Fatal("admission retained callback capability")
	}
	after, _ := f.store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("failed receipt command committed")
	}
	if old.Current() {
		t.Fatal("failure did not retire previous resource lease")
	}
	f.coordinator.opts.Admit = original
	accept(t, f, candidates(t, f, 2))
	ready(t, f, 2)
	r := f.registry(t)
	if len(r.Authorities()) != 2 || len(r.ConfigSources()) != 1 {
		t.Fatal("partial BOTH-origin inventory")
	}
	f.clock.Add(int64(2 * time.Second))
	for _, server := range []*providerServer{a, b} {
		if _, err := f.view(t).Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now())}, f.host.now()); err != nil {
			t.Fatal("real JWT denied after admission", err)
		}
	}
	capture := f.capture(t)
	capture.ProviderResources[0].UnavailableReason = "changed"
	capture.Release()
	fresh := f.capture(t)
	defer fresh.Release()
	if fresh.ProviderResources[0].UnavailableReason != "" {
		t.Fatal("capture resources alias")
	}
}

func TestOriginalWriterTokenRequiredAndNoSuccessorAdoption(t *testing.T) {
	f := newFixture(t)
	ctx, target := writer(t, f.store)
	called := false
	fn := func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { called = true; return nil }
	if err := f.coordinator.WithCommand(t.Context(), target, fn); !errors.Is(err, replication.ErrFenced) || called {
		t.Fatal("missing opaque writer token adopted authority", err)
	}
	wrong := target
	wrong.Incarnation++
	if err := f.coordinator.WithCommand(ctx, wrong, fn); !errors.Is(err, replication.ErrFenced) || called {
		t.Fatal("target mismatch admitted", err)
	}
	if err := f.coordinator.WithCommand(ctx, target, fn); err != nil || !called {
		t.Fatal("original current token rejected", err)
	}
	writerctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.coordinator.RunWriter(writerctx, target) }()
	wait(t, func() bool {
		f.coordinator.mu.Lock()
		defer f.coordinator.mu.Unlock()
		return f.coordinator.writerRunning
	})
	image, _ := f.store.ReadGeneration(t.Context())
	if _, err := f.store.EnablePrimary(t.Context(), image.Generation()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, replication.ErrFenced) {
			t.Fatal("writer silently adopted successor", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old writer did not stop")
	}
	cancel()
	called = false
	if err := f.coordinator.WithCommand(ctx, target, fn); !errors.Is(err, replication.ErrFenced) || called {
		t.Fatal("old token admitted after incarnation rotation", err)
	}
	newctx, newtarget := writer(t, f.store)
	if err := f.coordinator.WithCommand(newctx, newtarget, fn); err != nil {
		t.Fatal("explicit new writer cannot resume", err)
	}
}

func TestNilAdmissionAndInputCapsDenyWithoutResettingRoots(t *testing.T) {
	f := newFixture(t)
	o := f.coordinator.opts
	o.Admit = nil
	nilAdmission, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer nilAdmission.cancel()
	ctx, target := writer(t, f.store)
	before, _ := f.store.ReadGeneration(t.Context())
	if err := nilAdmission.RunWriter(ctx, target); !errors.Is(err, ErrAdmission) {
		t.Fatal("nil admission permitted writer", err)
	}
	after, _ := f.store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("nil admission wrote")
	}
	for _, change := range []func(*Options){func(o *Options) { o.PinHost = nil }, func(o *Options) { o.Host = nil }, func(o *Options) { o.OperationTimeout = 16 * time.Second }, func(o *Options) { o.RetryInterval = 2 * time.Minute }} {
		bad := o
		change(&bad)
		if _, err := New(bad); !errors.Is(err, ErrInvalid) {
			t.Fatal("bad dependency/cap accepted", err)
		}
	}
	badImage := *before
	badImage.Payload.Security = append([]replication.Entry(nil), before.Payload.Security...)
	for i := range badImage.Payload.Security {
		if strings.Contains(badImage.Payload.Security[i].Key, "root") {
			badImage.Payload.Security = append(badImage.Payload.Security[:i], badImage.Payload.Security[i+1:]...)
			break
		}
	}
	if err := replication.Seal(&badImage); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coordinator.Capture(t.Context(), &badImage); err == nil {
		t.Fatal("missing owner manifest treated as empty")
	}
}
