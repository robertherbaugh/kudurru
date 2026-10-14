package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func catalogDocument(t *testing.T, id, origin string) *catalogue.Catalogue {
	t.Helper()
	data := fmt.Appendf(nil, `{"schemaVersion":1,"version":"%s","license":"Apache-2.0","services":[{"id":"%s","name":"%s","category":"video","rules":["||%s.example^"],"source":"test"}],"safeSearch":[]}`, id, id, id, id)
	c, err := catalogue.Parse(data, origin)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type catalogFixture struct {
	*fixture
	holder    *catalogue.Holder
	owner     *CatalogRuntime
	build     *Builder
	publisher *serving.Publisher
	url       string
}

func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	f := &catalogFixture{fixture: newFixture(t), url: "https://catalog.example/data"}
	f.stop()
	f.deps.Host.Blocking.Catalogue.URL = f.url
	setCatalogSettings(t, f.fixture, f.deps.Host.Blocking.Catalogue)
	var p public.Policy
	if err := f.store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.BlockedServices = []string{"alpha", "beta"}
	if _, err := f.store.Apply(t.Context(), &p, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.holder = catalogue.NewHolder(catalogDocument(t, "alpha", f.url))
	var err error
	f.owner, err = NewCatalogRuntime(f.holder, f.deps.Host.Blocking.Catalogue)
	if err != nil {
		t.Fatal(err)
	}
	opts := f.builder.opts
	opts.Capture, err = f.owner.CaptureDependencies(opts.Capture)
	if err != nil {
		t.Fatal(err)
	}
	f.build, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	source, err := f.owner.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = serving.New(serving.Options{Source: source, Prepare: f.build.Prepare, Required: serving.CompleteManifest(), RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)
	go func() { done <- f.publisher.Run(ctx) }()
	go func() { done <- f.owner.Watch(ctx, f.publisher.Wake) }()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		f.publisher.Close()
	})
	select {
	case <-f.publisher.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatalf("catalog builder did not initialize: %+v", f.publisher.Status())
	}
	return f
}

func setCatalogSettings(t *testing.T, f *fixture, source config.BlockingCatalogue) {
	t.Helper()
	var settings public.Kudurru
	if err := f.store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		t.Fatal(err)
	}
	raw := settings.Spec.Settings["blocking"]
	var blocking map[string]json.RawMessage
	if err := json.Unmarshal(raw.Raw, &blocking); err != nil {
		t.Fatal(err)
	}
	var err error
	blocking["catalogue"], err = json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	raw.Raw, err = json.Marshal(blocking)
	if err != nil {
		t.Fatal(err)
	}
	settings.Spec.Settings["blocking"] = raw
	if _, err := f.store.Apply(t.Context(), &settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRuntimePublishesSameImageAndRetainsHeldPolicy(t *testing.T) {
	f := newCatalogFixture(t)
	held, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	chain := newChain(t, f.publisher)
	wantIP(t, query(t, chain, held, "alpha.example."), "192.0.2.11")
	if !f.holder.Set(catalogDocument(t, "beta", f.url)) {
		t.Fatal("catalog did not replace")
	}
	deadline := time.After(10 * time.Second)
	for {
		changed := f.publisher.Subscribe()
		fresh, err := f.publisher.CaptureHTTP(t.Context())
		if err == nil {
			id := fresh.Bundle().Identity()
			if id != held.Bundle().Identity() {
				if !id.Base.Same(held.Bundle().Identity().Base) {
					t.Fatal("catalog replacement changed canonical image")
				}
				wantIP(t, query(t, chain, fresh, "beta.example."), "192.0.2.11")
				fresh.Release()
				break
			}
			fresh.Release()
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("catalog change did not wake complete publication: %+v", f.publisher.Status())
		}
	}
	wantIP(t, query(t, chain, held, "alpha.example."), "192.0.2.11")
}

func TestCatalogRuntimePinsActualSQLitePublicationAndFinalCommit(t *testing.T) {
	for _, command := range []bool{false, true} {
		t.Run(fmt.Sprint("command-", command), func(t *testing.T) {
			f := newCatalogFixture(t)
			cut := f.holder.Capture()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			held := func() error {
				close(entered)
				<-release
				if !cut.Current() {
					return serving.ErrDependency
				}
				return nil
			}
			_, token, err := f.store.ReadServingCandidate(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			source, err := f.owner.GuardSource(f.store)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := NewKeyringOwner(f.deps.Keys)
			if err != nil {
				t.Fatal(err)
			}
			pin := f.owner.PrimaryPin(keys.PrimaryPin(f.publisher))
			a := f.store.Authority()
			done := make(chan error, 1)
			go func() {
				if !command {
					done <- source.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, held)
					return
				}
				done <- pin(t.Context(), pr.PublicationFence{Term: a.Term, Incarnation: a.Incarnation}, func(currentguard.Pinned) error {
					return f.store.Command(t.Context(), a.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test/catalog-final", Data: []byte(`{}`)}); err != nil {
							return err
						}
						return tx.RequireCommitCheck(func(context.Context) error { return held() })
					})
				})
			}()
			<-entered
			replacement := catalogDocument(t, "beta", f.url)
			installed := make(chan bool, 1)
			go func() { installed <- f.holder.Set(replacement) }()
			select {
			case <-installed:
				t.Fatal("catalog changed during actual SQLite barrier")
			case <-time.After(20 * time.Millisecond):
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !<-installed || cut.Current() {
				t.Fatal("replacement did not invalidate original cut")
			}
		})
	}
}

func TestCatalogRuntimeRejectsFreshPinRepairAndChangedConfiguration(t *testing.T) {
	f := newCatalogFixture(t)
	image, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.build.Prepare(t.Context(), serving.Candidate{Image: image, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, c := range prepared.Components {
			if c.Release != nil {
				c.Release()
			}
		}
	}()
	f.holder.Set(catalogDocument(t, "beta", f.url))
	source, err := f.owner.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	err = source.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error {
		if !prepared.Current() {
			return serving.ErrDependency
		}
		return nil
	})
	if !errors.Is(err, serving.ErrDependency) {
		t.Fatal("fresh catalog pin repaired old prepared candidate", err)
	}
	changedSource := f.owner.source
	changedSource.URL = "https://next.example/catalog"
	setCatalogSettings(t, f.fixture, changedSource)
	image, token, err = f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.build.Prepare(t.Context(), serving.Candidate{Image: image, Token: token}); !errors.Is(err, serving.ErrDependency) {
		t.Fatal("candidate source edit relabeled original acquisition", err)
	}
	f.holder.Set(catalogDocument(t, "other", "https://other.example/catalog"))
	called := false
	if err := f.owner.WithCurrent(t.Context(), func() error { called = true; return nil }); !errors.Is(err, serving.ErrDependency) || called {
		t.Fatal("mismatched source entered current command", err)
	}
}

func TestCatalogRuntimeExplicitFallbackAndOriginalSource(t *testing.T) {
	h := catalogue.NewHolder(nil)
	source := config.BlockingCatalogue{URL: "https://catalog.example/original", AdGuardServicesURL: "https://catalog.example/import"}
	o, err := NewCatalogRuntime(h, source)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := o.CaptureDependencies(func(context.Context, *replication.Image) (Dependencies, error) {
		return Dependencies{Identity: "base", Current: func() bool { return true }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := capture(t.Context(), nil)
	if err != nil || d.Catalog.Source != source || d.Catalog.Catalogue.Origin() != catalogue.OriginEmbedded || d.Catalog.Catalogue == catalogue.Embedded() {
		t.Fatal("fallback relabeled or borrowed mutable catalog", err)
	}
	d.Catalog.Catalogue.Services[0].Name = "mutated"
	if h.Current().Services[0].Name == "mutated" {
		t.Fatal("captured catalog aliases holder")
	}
	if _, err := NewCatalogRuntime(catalogue.NewHolder(catalogDocument(t, "other", "https://old.example/data")), source); !errors.Is(err, serving.ErrDependency) {
		t.Fatal("new configuration relabeled old acquisition", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.WithCurrent(ctx, func() error { t.Fatal("canceled pin called back"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
