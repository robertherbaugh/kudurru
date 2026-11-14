package servingbuild

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type transferredReceiver struct {
	serial atomic.Uint32
	axfr   chan uint32
}

func (r *transferredReceiver) SOA(context.Context, *public.DNSZone, string) (*dns.SOA, error) {
	rr, err := dns.NewRR(fmt.Sprintf("transferred.example. 60 IN SOA ns.transferred.example. hostmaster.transferred.example. %d 3600 3600 7200 60", r.serial.Load()))
	if err != nil {
		return nil, err
	}
	return rr.(*dns.SOA), nil
}

func (r *transferredReceiver) AXFR(ctx context.Context, z *public.DNSZone, primary string) ([]dns.RR, error) {
	soa, err := r.SOA(ctx, z, primary)
	if err != nil {
		return nil, err
	}
	ns, _ := dns.NewRR("transferred.example. 60 IN NS ns.transferred.example.")
	a, _ := dns.NewRR(fmt.Sprintf("host.transferred.example. 60 IN A 192.0.2.%d", soa.Serial))
	r.axfr <- soa.Serial
	return []dns.RR{soa, ns, a}, nil
}

type transferredFixture struct {
	*fixture
	manager *secondary.Manager
	owner   *TransferredRuntime
	zone    *public.DNSZone
	rx      *transferredReceiver
	pub     *serving.Publisher
	build   *Builder
	clock   atomic.Int64
}

func (f *transferredFixture) time() time.Time { return time.Unix(0, f.clock.Load()).UTC() }

func newTransferredFixture(t *testing.T) *transferredFixture {
	t.Helper()
	f := &transferredFixture{fixture: newFixture(t), rx: &transferredReceiver{axfr: make(chan uint32, 16)}}
	f.rx.serial.Store(1)
	f.clock.Store(f.now.UnixNano())
	obj, err := f.store.Apply(t.Context(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "transferred"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeSecondary, ZoneName: "transferred.example.", Secondary: &public.SecondarySpec{Primaries: []string{"127.0.0.1:5300"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	f.zone = obj.(*public.DNSZone)
	cache, err := zonecache.New(f.store, zonecache.Options{Debounce: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "copies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	f.manager, err = secondary.New(secondary.Options{Cache: cache, Blobs: blobs, ClusterID: f.store.Info().ClusterID, Keyring: f.deps.Keys, Receiver: f.rx, PollInterval: time.Millisecond, Now: f.time, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.owner = NewTransferredRuntime(f.manager, nil, nil)
	opts := f.builder.opts
	opts.Now = f.time
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
	f.pub, err = serving.New(serving.Options{Source: source, Required: serving.CompleteManifest(), Prepare: f.build.Prepare, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 4)
	go func() { done <- cache.Start(ctx) }()
	go func() { done <- f.manager.Run(ctx) }()
	go func() { done <- f.pub.Run(ctx) }()
	go func() { done <- f.owner.Watch(ctx, f.pub.Wake) }()
	t.Cleanup(func() {
		cancel()
		for range 4 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		f.pub.Close()
	})
	waitTransferredSerial(t, f, 1)
	return f
}

func waitTransferredSerial(t *testing.T, f *transferredFixture, serial uint32) {
	t.Helper()
	until := time.After(10 * time.Second)
	for {
		changed := f.pub.Subscribe()
		lease, err := f.pub.CaptureHTTP(t.Context())
		if err == nil {
			value, _ := lease.Bundle().Component("zones")
			z := value.(*servingzones.Prepared).Zones(f.time()).Match(f.zone.Spec.ZoneName)
			ready := z != nil && z.AvailableAt(f.time()) && z.Serial() == serial
			lease.Release()
			if ready {
				return
			}
		}
		select {
		case <-changed:
		case <-until:
			t.Fatalf("complete transfer publication did not reach %d: manager=%+v publisher=%+v", serial, f.manager.Statuses(), f.pub.Status())
		}
	}
}

func triggerTransferred(t *testing.T, f *transferredFixture, serial uint32) {
	t.Helper()
	f.rx.serial.Store(serial)
	f.clock.Add(int64(2 * time.Second))
	if !f.manager.Trigger(zone.SecondaryIdentity(f.zone)) {
		t.Fatal("manager rejected exact current source")
	}
	until := time.After(5 * time.Second)
	for {
		select {
		case got := <-f.rx.axfr:
			if got == serial {
				return
			}
		case <-until:
			t.Fatal("manager did not acquire replacement while pinned")
		}
	}
}

func TestTransferredRuntimeWakesWholePublisherAndRetainsHeldCopy(t *testing.T) {
	f := newTransferredFixture(t)
	held, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	value, _ := held.Bundle().Component("zones")
	old := value.(*servingzones.Prepared).Zones(f.now).Match(f.zone.Spec.ZoneName)
	_, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	triggerTransferred(t, f, 2)
	waitTransferredSerial(t, f, 2)
	fresh, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	if !fresh.Bundle().Identity().Base.Same(token.Generation()) || old.Serial() != 1 || old.AvailableAt(old.NextExpiry()) {
		t.Fatal("runtime publication changed canonical base or held copy expiry")
	}
	if fresh.Bundle().Identity() == held.Bundle().Identity() {
		t.Fatal("new acquisition reused complete dependency identity")
	}
}

func TestTransferredRuntimePinsWholeSQLitePublication(t *testing.T) {
	f := newTransferredFixture(t)
	captured := f.owner.capture()
	changed := f.manager.Changes()
	_, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source, err := f.owner.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- source.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error {
			close(entered)
			<-release
			if !captured.current() {
				return serving.ErrDependency
			}
			return nil
		})
	}()
	<-entered
	triggerTransferred(t, f, 2)
	select {
	case <-changed:
		t.Fatal("manager installed while the actual SQLite callback was held")
	case <-time.After(20 * time.Millisecond):
	}
	if !captured.current() {
		t.Fatal("manager installation crossed actual SQLite publication")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitTransferredSerial(t, f, 2)
	if captured.current() {
		t.Fatal("new acquisition did not invalidate original complete cursor")
	}
}

func TestTransferredRuntimePrimaryPinSpansActualFinalCommit(t *testing.T) {
	f := newTransferredFixture(t)
	keys, err := NewKeyringOwner(f.deps.Keys)
	if err != nil {
		t.Fatal(err)
	}
	pin := f.owner.PrimaryPin(keys.PrimaryPin(f.pub))
	authority := f.store.Authority()
	fence := pr.PublicationFence{Term: authority.Term, Incarnation: authority.Incarnation}
	captured := f.owner.capture()
	changed := f.manager.Changes()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- pin(t.Context(), fence, func(currentguard.Pinned) error {
			return f.store.Command(t.Context(), fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test/final-pin", Data: []byte(`{}`)}); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(context.Context) error {
					close(entered)
					<-release
					if !captured.current() {
						return serving.ErrDependency
					}
					return nil
				})
			})
		})
	}()
	<-entered
	triggerTransferred(t, f, 2)
	select {
	case <-changed:
		t.Fatal("manager installed while the actual final commit check was held")
	case <-time.After(20 * time.Millisecond):
	}
	if !captured.current() {
		t.Fatal("manager replacement crossed the final command admission")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitTransferredSerial(t, f, 2)
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range image.Payload.Runtime {
		found = found || row.Key == "test/final-pin"
	}
	if !found {
		t.Fatal("actual pinned command did not commit")
	}
}

func TestTransferredRuntimeRejectsStalePreparedDependency(t *testing.T) {
	f := newTransferredFixture(t)
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
	triggerTransferred(t, f, 2)
	waitTransferredSerial(t, f, 2)
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
		t.Fatal("fresh manager pins repaired stale prepared data", err)
	}
}

func TestTransferredRuntimeExplicitEmptyAndCanceledPin(t *testing.T) {
	o := NewTransferredRuntime(nil, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	capture, err := o.CaptureDependencies(func(context.Context, *replication.Image) (Dependencies, error) {
		return Dependencies{Identity: "empty-test", Current: func() bool { return true }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := capture(ctx, nil)
	if err != nil || !a.Current() || a.Secondary != nil || a.Stub != nil || a.RPZ != nil {
		t.Fatal("explicit absent managers did not remain fixed empty parts", err)
	}
	b, err := capture(ctx, nil)
	if err != nil || a.Identity != b.Identity {
		t.Fatal("empty captures churned identity", err)
	}
	cancel()
	called := false
	if err := o.WithCurrent(ctx, func() error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled empty pin entered callback", err)
	}
}
