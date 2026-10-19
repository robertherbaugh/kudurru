package servingbuild

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func emptyProviderOwner(t *testing.T, now func() time.Time) (*providerresources.Owner, providerresources.Desired) {
	t.Helper()
	host, err := providerresources.NewHost(providerresources.HostOptions{Identity: "test-host", CredentialPolicy: "test-policy", EntitlementIdentity: "test-entitlement", FingerprintKeyID: "test-key", ConfigSources: []servingauth.ProviderSource{}, AllowedOrigins: []string{}, LocalOrigins: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := providerresources.New(providerresources.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	desired := providerresources.Desired{RegistryDigest: strings.Repeat("a", 64), Host: host, Sources: []servingauth.ProviderDescriptor{}, Authorities: []providerregistry.AuthorityRecord{}}
	if err := owner.SetDesired(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	return owner, desired
}

func TestProviderSourcePinsAllReplacementsThroughActualSQLiteBarrier(t *testing.T) {
	f := newFixture(t)
	owner, desired := emptyProviderOwner(t, time.Now)
	licenses := license.Unrestricted()
	keys, err := NewKeyringOwner(ring(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := keys.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	source, err = GuardProviderSource(source, ProviderSourceOptions{Resources: owner, PinHost: func(ctx context.Context, fn func() error) error {
		return licenses.WithCurrent(ctx, func(license.Cut) error { return fn() })
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	licenseCut, keyCut := licenses.Capture(), keys.Capture()
	providerCut, err := owner.Acquire(t.Context(), desired)
	if err != nil {
		t.Fatal(err)
	}
	defer providerCut.Release()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- source.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error {
			close(entered)
			<-release
			if !licenseCut.Current() || !keyCut.Current() || !providerCut.Current() {
				return serving.ErrDependency
			}
			return nil
		})
	}()
	<-entered
	// These are the actual replacement methods, including Store's writer gate.
	replacement := make(chan error, 1)
	go func() { replacement <- licenses.Set("", "") }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	resourceDone, keyDone, storeDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { resourceDone <- owner.SetDesired(ctx, desired) }()
	nextKeys := ring(t, "b")
	go func() { keyDone <- keys.Replace(ctx, nextKeys) }()
	go func() {
		storeDone <- f.store.Command(ctx, f.store.Authority().Term, func(context.Context, *sqlite.CommandTx) error { return nil })
	}()
	resourceErr, keyErr := <-resourceDone, <-keyDone
	var premature bool
	select {
	case err := <-replacement:
		premature = true
		if err != nil {
			t.Error(err)
		}
	default:
	}
	close(release)
	publicationErr := <-done
	storeErr := <-storeDone
	if !premature {
		if err := <-replacement; err != nil {
			t.Fatal(err)
		}
	}
	if premature || publicationErr != nil || !errors.Is(resourceErr, context.DeadlineExceeded) || !errors.Is(keyErr, context.DeadlineExceeded) || !errors.Is(storeErr, context.DeadlineExceeded) {
		t.Fatal("replacement crossed actual publication exclusion", premature, publicationErr, resourceErr, keyErr, storeErr)
	}
	if licenseCut.Current() {
		t.Fatal("license replacement did not finish after publication")
	}
}

type providerBarrierSource struct {
	serving.Source
	inside func()
	result chan error
}

func (s providerBarrierSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, opts sqlite.PublicationOptions, fn func() error) error {
	err := s.Source.PublishIfCurrent(ctx, token, opts, func() error {
		if s.inside != nil {
			s.inside()
		}
		return fn()
	})
	if s.result != nil {
		s.result <- err
	}
	return err
}

func TestProviderSourceChecksOriginalClockAndCancellationInsideSQLite(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "clock-regression", true: "cancellation"}[canceled], func(t *testing.T) {
			f := newFixture(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			owner, _ := emptyProviderOwner(t, now)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lower := providerBarrierSource{Source: f.store, inside: func() {
				if canceled {
					cancel()
				} else {
					clock.Add(-int64(time.Hour))
				}
			}}
			source, err := GuardProviderSource(lower, ProviderSourceOptions{Resources: owner, Now: now, PinHost: func(_ context.Context, fn func() error) error { return fn() }})
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := f.store.ReadServingCandidate(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			err = source.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error { called = true; return nil })
			want := providerresources.ErrStale
			if canceled {
				want = context.Canceled
			}
			if called || !errors.Is(err, want) {
				t.Fatal("final barrier admitted invalid cut", called, err)
			}
		})
	}
}

func TestProviderSourceNewPinCannotRepairPreparedStaleCompleteBundle(t *testing.T) {
	for _, change := range []string{"provider", "license", "keyring"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			f.stop()
			owner, desired := emptyProviderOwner(t, time.Now)
			licenses := license.Unrestricted()
			keys, err := NewKeyringOwner(ring(t, "a"))
			if err != nil {
				t.Fatal(err)
			}
			source, err := keys.GuardSource(f.store)
			if err != nil {
				t.Fatal(err)
			}
			source, err = GuardProviderSource(source, ProviderSourceOptions{Resources: owner, PinHost: func(ctx context.Context, fn func() error) error {
				return licenses.WithCurrent(ctx, func(license.Cut) error { return fn() })
			}})
			if err != nil {
				t.Fatal(err)
			}
			results := make(chan error, 16)
			source = providerBarrierSource{Source: source, result: results}
			options := f.builder.opts
			baseCapture := options.Capture
			var blocked atomic.Bool
			options.Capture = func(ctx context.Context, img *replication.Image) (Dependencies, error) {
				d, err := baseCapture(ctx, img)
				if err != nil {
					return d, err
				}
				if blocked.Load() {
					return d, serving.ErrDependency
				}
				lease, err := owner.Acquire(ctx, desired)
				if err != nil {
					return d, err
				}
				licenseCut, keyCut := licenses.Capture(), keys.Capture()
				previousCurrent, previousRelease := d.Current, d.Release
				d.Current = func() bool { return previousCurrent() && lease.Current() && licenseCut.Current() && keyCut.Current() }
				d.Release = func() { lease.Release(); previousRelease() }
				d.Keys, d.KeyringIdentity = keyCut.Keys(), keyCut.Identity()
				return d, nil
			}
			builder, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			var hold atomic.Bool
			prepared, proceed := make(chan struct{}), make(chan struct{})
			pub, err := serving.New(serving.Options{Source: source, Required: serving.CompleteManifest(), RetryInterval: time.Millisecond, Prepare: func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
				p, err := builder.Prepare(ctx, c)
				if err == nil && hold.CompareAndSwap(true, false) {
					close(prepared)
					select {
					case <-proceed:
					case <-ctx.Done():
					}
				}
				return p, err
			}})
			if err != nil {
				t.Fatal(err)
			}
			runctx, stop := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); _ = pub.Run(runctx) }()
			defer func() { stop(); <-done }()
			select {
			case <-pub.Initialized():
			case <-time.After(5 * time.Second):
				t.Fatal(pub.Status())
			}
			if err := <-results; err != nil {
				t.Fatal(err)
			}
			old, err := pub.CaptureHTTP(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer old.Release()
			chain := newChain(t, pub)
			hold.Store(true)
			f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := tx.Apply(ctx, settings("192.0.2.33"), store.ApplyOptions{Manager: store.ManagerSystem})
				return err
			})
			select {
			case <-prepared:
			case <-time.After(5 * time.Second):
				t.Fatal("did not prepare candidate")
			}
			blocked.Store(true)
			switch change {
			case "provider":
				err = owner.WithReplacement(t.Context(), func(r *providerresources.Replacement) error { r.Retire(); return r.SetDesired(desired) })
			case "license":
				err = licenses.Set("", "")
			case "keyring":
				err = keys.Replace(t.Context(), ring(t, "b"))
			}
			close(proceed)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-results:
			case <-time.After(5 * time.Second):
				t.Fatal("candidate never reached admission")
			}
			if !errors.Is(err, serving.ErrDependency) {
				t.Fatal("new pin repaired stale candidate", err)
			}
			wantIP(t, query(t, chain, nil, "blocked.example."), "192.0.2.11")
			if lease, err := pub.CaptureHTTP(t.Context()); !errors.Is(err, serving.ErrPending) {
				if lease != nil {
					lease.Release()
				}
				t.Fatal("stale protected view remained available", err)
			}
			blocked.Store(false)
			pub.Wake()
			deadline := time.Now().Add(5 * time.Second)
			for !pub.Status().ProtectedReady && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !pub.Status().ProtectedReady {
				t.Fatal("fresh complete capture failed", pub.Status())
			}
			wantIP(t, query(t, chain, nil, "blocked.example."), "192.0.2.33")
			wantIP(t, query(t, chain, old, "blocked.example."), "192.0.2.11")
		})
	}
}
