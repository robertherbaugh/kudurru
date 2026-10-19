package providerresources

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testValidatorOnce sync.Once
var testValidator *validator.Validator
var testValidatorError error

func resourceStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	testValidatorOnce.Do(func() { testValidator, testValidatorError = validator.New() })
	if testValidatorError != nil {
		t.Fatal(testValidatorError)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := []store.Kind{}
	for _, k := range runtimekinds.Private() {
		private = append(private, store.Kind(k))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: testValidator, InitialMode: mode, ValidateReplicaEntry: func(section string, e replication.Entry) error {
		if err := servingauth.ValidateEntry(section, e); err != nil {
			return err
		}
		return providerregistry.ValidateEntry(section, e)
	}, ValidateReplicaImage: func(image *replication.Image) error {
		if _, err := servingauth.DecodeSecurity(image.Payload.Security); err != nil {
			return err
		}
		return providerregistry.ValidateImage(image)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func mustCommand(t *testing.T, s *sqlite.Store, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, fn); err != nil {
		t.Fatal(err)
	}
}
func writeSecurity(ctx context.Context, tx *sqlite.CommandTx, state servingauth.Security) error {
	rows, err := servingauth.EncodeSecurity(state)
	if err != nil {
		return err
	}
	for _, e := range rows {
		if err := tx.PutSection(ctx, "security", e); err != nil {
			return err
		}
	}
	return nil
}
func actualDesired(t *testing.T, s *sqlite.Store, h *HostCut) Desired {
	t.Helper()
	var d Desired
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		r, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
		if err != nil {
			return err
		}
		d = Desired{r.Digest(), h, sources, r.Authorities()}
		return nil
	})
	return d
}
func realOwner(t *testing.T) (*resourceFixture, *sqlite.Store, *Owner, Desired) {
	t.Helper()
	f := newResourceFixture(t)
	s := resourceStore(t, filepath.Join(t.TempDir(), "owner.db"), replication.Standalone)
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		if err := writeSecurity(ctx, tx, servingauth.EmptySecurity(1)); err != nil {
			return err
		}
		p, err := providerregistry.PlanInitialize(tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		if err = p.Stage(ctx, tx); err != nil {
			return err
		}
		spec := publicv1.IdentityProviderSpec{Issuer: f.server.URL, ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "TEST_CREDENTIAL"}}
		if _, err = tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp", CreationTimestamp: metav1.NewTime(f.now().Add(-time.Hour))}, Spec: spec}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "corp-read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "corp:alice"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	d := actualDesired(t, s, nil)
	d.Host = f.host(t, d.Sources[0], "test-only", true)
	o := f.owner(t, d)
	c := candidate(t, o)
	if err := c.Match(d); err != nil {
		t.Fatal(err)
	}
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		r, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		p, err := providerregistry.PlanAcceptAuthority(r, []providerregistry.DesiredAuthority{c.Receipt()}, f.now())
		if err != nil {
			return err
		}
		return p.Stage(ctx, tx)
	})
	d = actualDesired(t, s, d.Host)
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderReady
	})
	return f, s, o, d
}
func authDependencies(t *testing.T, identity string, config []servingauth.ProviderSource, resources []servingauth.ProviderResource) servingauth.Dependencies {
	t.Helper()
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	return servingauth.Dependencies{Identity: identity, ExpectedIdentity: identity, Permissions: permissions, License: servingauth.LicensePolicy{RBAC: true, Providers: true}, CurrentKey: []byte(strings.Repeat("k", 32)), ConfigProviders: config, ProviderResources: resources}
}
func TestCurrentDependenciesActualStagedRegistrySpecUIDAndOfflineView(t *testing.T) {
	f, s, o, d := realOwner(t)
	lease, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, _ := runtimekinds.NewScheme()
	cut, err := servingauth.CaptureImage(t.Context(), image, servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	deps := authDependencies(t, lease.Identity(), []servingauth.ProviderSource{}, lease.Resources())
	view, err := servingauth.Prepare(t.Context(), cut, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	raw := f.token(t, "test")
	calls := f.calls.Load()
	if _, err = view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.OIDCJWT, Value: raw}, f.now()); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != calls {
		t.Fatal("view performed network")
	}
	abort := errors.New("test rollback")
	err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			current, err := p.DependenciesCurrent(ctx, tx, d.Host)
			if err != nil {
				return err
			}
			if current.RegistryDigest != d.RegistryDigest || len(current.ConfigProviders) != 0 {
				return errors.New("wrong current registry")
			}
			currentDeps := deps
			currentDeps.ConfigProviders = current.ConfigProviders
			currentDeps.ProviderResources = current.ProviderResources
			if _, err := servingauth.AuthenticateCurrent(ctx, tx, currentDeps, servingauth.Credential{Kind: servingauth.OIDCJWT, Value: raw}, f.now()); err != nil {
				return err
			}
			var object publicv1.IdentityProvider
			if err := tx.Get(ctx, store.Key{Kind: "IdentityProvider", Name: "corp"}, &object); err != nil {
				return err
			}
			object.Spec.ClientID = "replacement"
			if _, err := tx.Apply(ctx, &object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			if _, err := p.DependenciesCurrent(ctx, tx, d.Host); !errors.Is(err, ErrStale) {
				return errors.New("staged spec reused old resource")
			}
			return abort
		})
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	// A registry edit with the same object source still invalidates the cut, and
	// canonical config comes from the concrete transaction rather than deps.
	err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			rows, err := tx.ListSections(ctx, "security")
			if err != nil {
				return err
			}
			r, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
			if err != nil {
				return err
			}
			spec := publicv1.IdentityProviderSpec{Issuer: "https://other.example", ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "TEST_CREDENTIAL"}}
			plan, err := providerregistry.PlanReconcile(r, []providerregistry.DesiredConfig{{ID: "other", Spec: spec}}, f.now(), func() (string, error) { return strings.Repeat("b", 32), nil })
			if err != nil {
				return err
			}
			if err := plan.Stage(ctx, tx); err != nil {
				return err
			}
			if _, err := p.DependenciesCurrent(ctx, tx, d.Host); !errors.Is(err, ErrStale) {
				return errors.New("staged registry reused cached config")
			}
			return abort
		})
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	// Recreated same-name object receives a new actual UID. Neither the prior
	// resource nor registry history can be relabeled as the replacement.
	err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			key := store.Key{Kind: "IdentityProvider", Name: "corp"}
			var original publicv1.IdentityProvider
			if err := tx.Get(ctx, key, &original); err != nil {
				return err
			}
			if err := tx.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			recreated, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp"}, Spec: original.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				return err
			}
			if string(recreated.GetUID()) == d.Sources[0].Ref.UID {
				return errors.New("object recreation reused UID")
			}
			if _, err := p.DependenciesCurrent(ctx, tx, d.Host); !errors.Is(err, ErrStale) {
				return errors.New("recreated UID reused old resource")
			}
			return abort
		})
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	// Complete offline input remains usable after its actual SQLite source closes.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.OIDCJWT, Value: raw}, f.now()); err != nil {
		t.Fatal("store read after Prepare", err)
	}
}
func TestRealCommandFinalFenceExcludesReplacementAndExpiryRollsBack(t *testing.T) {
	f, s, o, d := realOwner(t)
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- o.WithCurrent(t.Context(), func(p *Pinned) error {
			return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if _, err := p.DependenciesCurrent(ctx, tx, d.Host); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "resource-test", Data: []byte(`{"value":1}`)}); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(ctx context.Context) error {
					close(entered)
					select {
					case <-release:
						return ctx.Err()
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no final fence")
	}
	next := d
	next.Host = f.host(t, d.Sources[0], "changed", true)
	replacement := make(chan error, 1)
	go func() { replacement <- o.SetDesired(t.Context(), next) }()
	select {
	case err := <-replacement:
		t.Fatal("replacement crossed actual commit", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-replacement; err != nil {
		t.Fatal(err)
	}
	// Restore the exact durable authority and wait for reacquisition.
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderReady
	})
	before, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if _, err := p.DependenciesCurrent(ctx, tx, d.Host); err != nil {
				return err
			}
			if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "expiry-test", Data: []byte(`{"value":2}`)}); err != nil {
				return err
			}
			f.clock.Add(int64(time.Minute))
			return nil
		})
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired final check committed", err)
	}
	after, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("expiry did not roll back actual mutation")
	}
	// No-op commands cross the same final resource-time check.
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderReady
	})
	err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if _, err := p.DependenciesCurrent(ctx, tx, d.Host); err != nil {
				return err
			}
			f.clock.Add(int64(time.Minute))
			return nil
		})
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired noop succeeded", err)
	}
}

type resourcePublicationSource struct {
	*sqlite.Store
	owner            *Owner
	pause            atomic.Bool
	entered, release chan struct{}
}

func (s *resourcePublicationSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, opts sqlite.PublicationOptions, publish func() error) error {
	return s.owner.WithCurrent(ctx, func(p *Pinned) error {
		return s.Store.PublishIfCurrent(ctx, token, opts, func() error {
			if err := p.CurrentAt(s.owner.opts.Now()); err != nil {
				return err
			}
			if s.pause.CompareAndSwap(true, false) {
				close(s.entered)
				select {
				case <-s.release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if err := p.CurrentAt(s.owner.opts.Now()); err != nil {
				return err
			}
			return publish()
		})
	})
}
func TestActualCompletePublisherExcludesReplacementAtPointerSwap(t *testing.T) {
	f, s, o, d := realOwner(t)
	source := &resourcePublicationSource{Store: s, owner: o, entered: make(chan struct{}), release: make(chan struct{})}
	source.pause.Store(true)
	permissions, _ := rbac.LoadPermissions()
	scheme, _ := runtimekinds.NewScheme()
	publisher, err := serving.New(serving.Options{Source: source, Required: serving.CompleteManifest(), DependencyChanges: o.Changes, RetryInterval: 20 * time.Millisecond, Prepare: func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
		registry, err := providerregistry.Decode(c.Image.Payload.Security, c.Image.Term.ClusterID)
		if err != nil {
			return serving.Prepared{}, err
		}
		cut, err := servingauth.CaptureImage(ctx, c.Image, servingauth.Schema{Scheme: scheme, Kinds: c.Image.KindManifest, SchemaVersion: store.SchemaVersion})
		if err != nil {
			return serving.Prepared{}, err
		}
		config := registry.ConfigSources()
		sources, err := servingauth.ProviderSources(ctx, cut, config)
		if err != nil {
			return serving.Prepared{}, err
		}
		desired := Desired{registry.Digest(), d.Host, sources, registry.Authorities()}
		lease, err := o.Acquire(ctx, desired)
		if err != nil {
			return serving.Prepared{}, err
		}
		deps := servingauth.Dependencies{Identity: lease.Identity(), ExpectedIdentity: lease.Identity(), Permissions: permissions, License: servingauth.LicensePolicy{RBAC: true, Providers: true}, CurrentKey: []byte(strings.Repeat("k", 32)), ConfigProviders: config, ProviderResources: lease.Resources()}
		view, err := servingauth.Prepare(ctx, cut, deps)
		if err != nil {
			lease.Release()
			return serving.Prepared{}, err
		}
		components := []serving.Component{}
		for _, name := range serving.CompleteManifest() {
			var value any = c.Token.Generation()
			component := serving.Component{Name: name, Base: c.Token.Generation(), Identity: lease.Identity(), Value: value}
			if name == "auth" {
				component.Value = view
				component.Release = func() { view.Release(); lease.Release() }
			}
			components = append(components, component)
		}
		return serving.Prepared{Components: components, Current: lease.Current}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); publisher.Close() })
	runDone := make(chan error, 1)
	go func() { runDone <- publisher.Run(ctx) }()
	select {
	case <-source.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("publication not paused")
	}
	next := d
	next.Host = f.host(t, d.Sources[0], "new", true)
	rotated := make(chan error, 1)
	go func() { rotated <- o.SetDesired(t.Context(), next) }()
	select {
	case err := <-rotated:
		t.Fatal("rotation crossed publication swap", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(source.release)
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	if l, err := publisher.CaptureHTTP(t.Context()); err == nil {
		l.Release()
		t.Fatal("old resource cut eligible after rotation")
	}
	cancel()
	publisher.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not join")
	}
}

func imageDesired(t *testing.T, image *replication.Image, h *HostCut) Desired {
	t.Helper()
	scheme, _ := runtimekinds.NewScheme()
	r, err := providerregistry.Decode(image.Payload.Security, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := servingauth.CaptureImage(t.Context(), image, servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := servingauth.ProviderSources(t.Context(), cut, r.ConfigSources())
	if err != nil {
		t.Fatal(err)
	}
	return Desired{r.Digest(), h, sources, r.Authorities()}
}
func TestAcceptedRealImageRestartInstallPromotionNoFollowerWrites(t *testing.T) {
	f, s, o, d := realOwner(t)
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen the exact file, with no reconciliation, UID fabrication or admission.
	reopened := resourceStore(t, s.Path(), replication.Standalone)
	restarted, err := reopened.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h := f.host(t, d.Sources[0], "test-only", true)
	after := imageDesired(t, restarted, h)
	if after.RegistryDigest != d.RegistryDigest || after.Sources[0].Ref != d.Sources[0].Ref || after.Sources[0].CreatedAt != d.Sources[0].CreatedAt {
		t.Fatal("restart changed durable provider authority")
	}
	resumed := f.owner(t, after)
	await(t, func() bool {
		l, err := resumed.Acquire(t.Context(), after)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderReady
	})
	if len(resumed.TakeCandidates()) != 0 {
		t.Fatal("restart silently accepted new receipt")
	}
	follower := resourceStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	a := follower.Authority()
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed})
	if err != nil {
		t.Fatal(err)
	}
	peerImage, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	peerDesired := imageDesired(t, peerImage, f.host(t, d.Sources[0], "test-only", true))
	peer := f.owner(t, peerDesired)
	await(t, func() bool {
		l, err := peer.Acquire(t.Context(), peerDesired)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderReady
	})
	still, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !still.Generation().Same(peerImage.Generation()) {
		t.Fatal("resource worker wrote follower authority")
	}
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "provider-resources-promotion"}); err != nil {
		t.Fatal(err)
	}
	promoted, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if imageDesired(t, promoted, peerDesired.Host).RegistryDigest != d.RegistryDigest {
		t.Fatal("promotion lost admitted receipt")
	}
}
