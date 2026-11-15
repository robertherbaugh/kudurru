package servingbuild

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestCompletePrimaryPinHoldsAllOwnersThroughActualCommit(t *testing.T) {
	f := newTransferredFixture(t)
	resources, desired := emptyProviderOwner(t, time.Now)
	licenses := license.Unrestricted()
	keys, err := NewKeyringOwner(f.deps.Keys)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://catalog.example/services.json"
	holder := catalogue.NewHolder(catalogDocument(t, "alpha", origin))
	catalog, err := NewCatalogRuntime(holder, config.BlockingCatalogue{URL: origin})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := NewPrimaryPin(PrimaryPinOptions{Publisher: f.pub, Resources: resources, Keyrings: keys, Catalog: catalog, Transferred: f.owner,
		PinHostCurrent: func(ctx context.Context, fn func(func() bool) error) error {
			// This fixture has no providerhost. The real license manager is
			// its sole mutable host dependency, with its actual guarded cut.
			return licenses.WithCurrent(ctx, func(cut license.Cut) error { return fn(cut.Current) })
		}})
	if err != nil {
		t.Fatal(err)
	}
	a := f.store.Authority()
	fence := pr.PublicationFence{Term: a.Term, Incarnation: a.Incarnation}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	oldLicense, oldKeys, oldCatalog, oldTransferred := licenses.Capture(), keys.Capture(), holder.Capture(), f.owner.capture()
	var retained currentguard.Pinned
	done := make(chan error, 1)
	go func() {
		done <- pin(t.Context(), fence, func(p currentguard.Pinned) error {
			retained = p
			if p.Keys != oldKeys.Keys() || p.Identity != oldKeys.Identity() || p.Current == nil || !p.Current() {
				return serving.ErrDependency
			}
			return f.store.Command(t.Context(), a.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test/complete-pin", Data: []byte(`{}`)}); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(context.Context) error {
					close(entered)
					<-release
					if !p.Current() {
						return serving.ErrDependency
					}
					return nil
				})
			})
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual command did not reach final check")
	}
	// Network acquisition must progress even while its new installation waits.
	managerChanged := f.manager.Changes()
	triggerTransferred(t, f, 2)
	licenseDone, catalogDone := make(chan error, 1), make(chan bool, 1)
	go func() { licenseDone <- licenses.Set("", "") }()
	nextCatalog := catalogDocument(t, "beta", origin)
	go func() { catalogDone <- holder.Set(nextCatalog) }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	keyDone, resourceDone := make(chan error, 1), make(chan error, 1)
	nextKeys := ring(t, "b")
	go func() { keyDone <- keys.Replace(ctx, nextKeys) }()
	go func() { resourceDone <- resources.SetDesired(ctx, desired) }()
	if err := <-keyDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("TSIG replacement crossed complete pin", err)
	}
	if err := <-resourceDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("provider replacement crossed complete pin", err)
	}
	if _, release, err := f.pub.Pin(ctx); !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatal("publisher replacement crossed complete pin", err)
	}
	select {
	case err := <-licenseDone:
		t.Fatal("license replacement crossed complete pin", err)
	case <-catalogDone:
		t.Fatal("catalog replacement crossed complete pin")
	case <-managerChanged:
		t.Fatal("manager installed before final commit")
	default:
	}
	if !oldLicense.Current() || !oldKeys.Current() || !oldCatalog.Current() || !oldTransferred.current() {
		t.Fatal("an original dependency changed during command")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if retained.Current() {
		t.Fatal("pin predicate survived callback exit")
	}
	if err := <-licenseDone; err != nil {
		t.Fatal(err)
	}
	if !<-catalogDone {
		t.Fatal("catalog replacement was lost")
	}
	waitTransferredSerial(t, f, 2)
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range image.Payload.Runtime {
		if row.Key == "test/complete-pin" {
			return
		}
	}
	t.Fatal("pinned transaction did not commit")
}

func TestCompletePrimaryPinRejectsMissingHostPredicate(t *testing.T) {
	f := newFixture(t)
	resources, _ := emptyProviderOwner(t, time.Now)
	keys, err := NewKeyringOwner(f.deps.Keys)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewCatalogRuntime(catalogue.NewHolder(catalogue.Embedded()), f.deps.Host.Blocking.Catalogue)
	if err != nil {
		t.Fatal(err)
	}
	o := PrimaryPinOptions{Publisher: f.pub, Resources: resources, Keyrings: keys, Catalog: catalog, Transferred: NewTransferredRuntime(nil, nil, nil),
		PinHostCurrent: func(_ context.Context, fn func(func() bool) error) error { return fn(nil) }}
	pin, err := NewPrimaryPin(o)
	if err != nil {
		t.Fatal(err)
	}
	a := f.store.Authority()
	err = pin(t.Context(), pr.PublicationFence{Term: a.Term, Incarnation: a.Incarnation}, func(currentguard.Pinned) error { t.Error("missing host capability accepted"); return nil })
	if !errors.Is(err, serving.ErrDependency) {
		t.Fatal(err)
	}
	for _, invalidate := range []func(*PrimaryPinOptions){
		func(o *PrimaryPinOptions) { o.Publisher = nil }, func(o *PrimaryPinOptions) { o.Resources = nil },
		func(o *PrimaryPinOptions) { o.PinHostCurrent = nil }, func(o *PrimaryPinOptions) { o.Keyrings = nil },
		func(o *PrimaryPinOptions) { o.Catalog = nil }, func(o *PrimaryPinOptions) { o.Transferred = nil },
	} {
		bad := o
		invalidate(&bad)
		if _, err := NewPrimaryPin(bad); !errors.Is(err, serving.ErrComponents) {
			t.Fatal("missing dependency accepted", err)
		}
	}
}

func TestCompletePrimaryPinRechecksResourceClockAtFinalAdmission(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "noop", true: "write"}[write], func(t *testing.T) {
			f := newFixture(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			resources, _ := emptyProviderOwner(t, now)
			keys, err := NewKeyringOwner(f.deps.Keys)
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := NewCatalogRuntime(catalogue.NewHolder(catalogue.Embedded()), f.deps.Host.Blocking.Catalogue)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := NewPrimaryPin(PrimaryPinOptions{Publisher: f.pub, Resources: resources, Keyrings: keys, Catalog: catalog, Transferred: NewTransferredRuntime(nil, nil, nil), Now: now,
				// Explicit fixed test host; resource time is the capability under test.
				PinHostCurrent: func(_ context.Context, fn func(func() bool) error) error { return fn(func() bool { return true }) }})
			if err != nil {
				t.Fatal(err)
			}
			a := f.store.Authority()
			before, err := f.store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = pin(t.Context(), pr.PublicationFence{Term: a.Term, Incarnation: a.Incarnation}, func(p currentguard.Pinned) error {
				return f.store.Command(t.Context(), a.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if write {
						if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test/expired-pin", Data: []byte(`{}`)}); err != nil {
							return err
						}
					}
					if err := tx.RequireCommitCheck(func(context.Context) error {
						if !p.Current() {
							return serving.ErrDependency
						}
						return nil
					}); err != nil {
						return err
					}
					clock.Add(-int64(time.Hour))
					return nil
				})
			})
			if !errors.Is(err, serving.ErrDependency) {
				t.Fatal("resource clock change crossed actual final admission", err)
			}
			after, err := f.store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("invalid resource clock changed durable image", err)
			}
		})
	}
}
