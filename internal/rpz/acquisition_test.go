package rpz

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

type acquisitionTestKeys struct {
	owner *snapshotowner.Owner[dnsserver.StaticKeyring]
	cut   snapshotowner.Cut[dnsserver.StaticKeyring]
}

func (c acquisitionTestKeys) Keys() *dnsserver.StaticKeyring { return c.cut.Value() }
func (c acquisitionTestKeys) Identity() string               { return c.cut.Identity() }
func (c acquisitionTestKeys) Current() bool                  { return c.cut.Current() }
func (c acquisitionTestKeys) WithCurrent(ctx context.Context, cache func() error, publish func() error) error {
	return c.owner.WithCurrent(ctx, c.cut, func() error {
		if err := cache(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if publish != nil {
			return publish()
		}
		return nil
	})
}

func TestAcquisitionFactoryOnlyForNewRPZEntry(t *testing.T) {
	fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	var owner snapshotowner.Owner[dnsserver.StaticKeyring]
	install := func() {
		t.Helper()
		if err := owner.Publish(t.Context(), keys, func(*dnsserver.StaticKeyring, *dnsserver.StaticKeyring) bool { return false }); err != nil {
			t.Fatal(err)
		}
	}
	install()
	var factories atomic.Int64
	fx.m.opts.CaptureKeys = func() dnsserver.AcquisitionKeys { return acquisitionTestKeys{owner: &owner, cut: owner.Capture()} }
	fx.m.opts.ReceiverForKeys = func(k *dnsserver.StaticKeyring) Receiver {
		if k != keys {
			t.Error("factory received another provider")
		}
		factories.Add(1)
		return &testReceiver{serial: 1}
	}
	entries := map[string]*entry{}
	config := fx.cache.Snapshot()
	fx.m.reconcile(t.Context(), entries, config)
	if len(entries) != 1 || factories.Load() != 1 {
		t.Fatal("initial entry did not construct exactly one receiver")
	}
	id := SourceIdentity(fx.list, 0)
	e := entries[id]
	job := transferJob{entry: e, config: config, spec: e.spec, dz: e.dz, credential: e.credential, acquisition: e.acquisition}
	for range 5 {
		fx.m.reconcile(t.Context(), entries, config)
		if _, err := fx.m.credential(e.dz); err != nil {
			t.Fatal(err)
		}
		if !fx.m.jobCredentialsMatch(job) {
			t.Fatal("original job failed pure credential comparison")
		}
	}
	r := fx.m.refresh(t.Context(), job)
	if r.err != nil {
		t.Fatal(r.err)
	}
	fx.m.complete(t.Context(), entries, r)
	if factories.Load() != 1 || e.z == nil {
		t.Fatal("reconcile/final pins constructed discarded receivers")
	}
	held := fx.m.Capture()
	install()
	fx.m.reconcile(t.Context(), entries, config)
	if factories.Load() != 2 || entries[id] == e || e.acquisition.current() || fx.m.jobCredentialsMatch(job) {
		t.Fatal("replacement reused an original entry or built extra receivers")
	}
	fx.m.complete(t.Context(), entries, r)
	if factories.Load() != 2 || !held.Current() {
		t.Fatal("old completion republished or recaptured a successor receiver")
	}
}

type acquisitionDeadline struct {
	kind     string
	deadline time.Time
	bounded  bool
}

type acquisitionDeadlineReceiver struct {
	Receiver
	observed chan<- acquisitionDeadline
}

func (r acquisitionDeadlineReceiver) SOA(ctx context.Context, dz *public.DNSZone, primary string) (*dns.SOA, error) {
	deadline, ok := ctx.Deadline()
	r.observed <- acquisitionDeadline{"SOA", deadline, ok}
	return r.Receiver.SOA(ctx, dz, primary)
}
func (r acquisitionDeadlineReceiver) AXFR(ctx context.Context, dz *public.DNSZone, primary string) ([]dns.RR, error) {
	deadline, ok := ctx.Deadline()
	r.observed <- acquisitionDeadline{"AXFR", deadline, ok}
	return r.Receiver.AXFR(ctx, dz, primary)
}

type acquisitionDeadlineBlobs struct {
	BlobStore
	observed chan<- acquisitionDeadline
}

func (b acquisitionDeadlineBlobs) PutBlob(ctx context.Context, _ string, _ []byte) error {
	deadline, ok := ctx.Deadline()
	b.observed <- acquisitionDeadline{"persist", deadline, ok}
	<-ctx.Done()
	return ctx.Err()
}

func TestAcquisitionRPZOriginalDeadlineIncludesBlockedPersistence(t *testing.T) {
	observed := make(chan acquisitionDeadline, 8)
	db := runtimeDB(t)
	fx := newRuntime(t, acquisitionDeadlineReceiver{Receiver: &testReceiver{serial: 1}, observed: observed}, acquisitionDeadlineBlobs{BlobStore: db, observed: observed}, runtimeList())
	fx.m.opts.Timeout = 200 * time.Millisecond
	startRuntime(t, fx.m)
	var original time.Time
	for _, want := range []string{"SOA", "AXFR", "persist"} {
		select {
		case got := <-observed:
			if !got.bounded || got.kind != want {
				t.Fatal("scheduled acquisition lost original bounded phase", got.kind)
			}
			if original.IsZero() {
				original = got.deadline
			}
			if got.deadline != original {
				t.Fatal("persistence minted a successor deadline")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("scheduled acquisition did not enter blocked persistence")
		}
	}
	eventually(t, func() bool {
		statuses := fx.m.Statuses()
		return len(statuses) == 1 && statuses[0].Error != ""
	})
	if !fx.m.Snapshot().Select([]*public.BlockList{fx.list}, true).Evaluate(Context{Name: "bad.test."}, fx.clock.now()).Unavailable {
		t.Fatal("expired blocked persistence published a feed")
	}
	if _, err := db.GetBlob(t.Context(), acceptanceName(SourceIdentity(fx.list, 0))); !errors.Is(err, localdb.ErrNoBlob) {
		t.Fatal("expired persistence left accepted data", err)
	}
}

func TestAcquisitionRPZManagerGateDeadlineRetainsOriginalSnapshot(t *testing.T) {
	rx := &testReceiver{serial: 1}
	blobs := &hookedBlobs{BlobStore: runtimeDB(t)}
	fx := newRuntime(t, rx, blobs, runtimeList())
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	var owner snapshotowner.Owner[dnsserver.StaticKeyring]
	if err := owner.Publish(t.Context(), keys, func(*dnsserver.StaticKeyring, *dnsserver.StaticKeyring) bool { return false }); err != nil {
		t.Fatal(err)
	}
	fx.m.opts.CaptureKeys = func() dnsserver.AcquisitionKeys { return acquisitionTestKeys{owner: &owner, cut: owner.Capture()} }
	fx.m.opts.ReceiverForKeys = func(*dnsserver.StaticKeyring) Receiver { return rx }
	entries := map[string]*entry{}
	config := fx.cache.Snapshot()
	fx.m.reconcile(t.Context(), entries, config)
	e := entries[SourceIdentity(fx.list, 0)]
	job := transferJob{entry: e, config: config, spec: e.spec, dz: e.dz, credential: e.credential, acquisition: e.acquisition}
	first := fx.m.refresh(t.Context(), job)
	if first.err != nil {
		t.Fatal(first.err)
	}
	fx.m.complete(t.Context(), entries, first)
	if e.z == nil {
		t.Fatal("initial feed was not accepted")
	}
	held := fx.m.Capture()
	oldData, oldZone := e.data, e.z
	job.z, job.data = e.z, e.data
	rx.set(2, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	job.ctx, job.cancel = ctx, cancel
	second := fx.m.refresh(ctx, job)
	if second.err != nil {
		t.Fatal(second.err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	go func() {
		done <- fx.m.WithCurrent(t.Context(), held, func() error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("manager gate was not pinned")
	}
	accepted := false
	blobs.put = func(name string) error {
		if name == acceptanceName(e.spec.Identity) {
			accepted = true
		}
		return nil
	}
	// Real cache persistence succeeds; only the memory publication is held.
	// It must use the ORIGINAL job context while waiting for this manager pin.
	fx.m.complete(ctx, entries, second)
	if !accepted || !errors.Is(ctx.Err(), context.DeadlineExceeded) || e.z != oldZone || e.data != oldData || !held.Current() || e.status.Error == "" {
		t.Fatal("expired gate wait changed accepted snapshot or retained scheduler candidate")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("held manager pin did not join")
	}
}

// D390: the Kubernetes startup source captures per feed. It receives a
// detached copy of the entry's own transfer configuration, so an unsigned feed
// can be admitted while stored keys are pending, and the result is the entry's
// acquisition exactly as with CaptureKeys.
func TestAcquisitionZoneScopedCapture(t *testing.T) {
	db := runtimeDB(t)
	cache := newRuntime(t, &testReceiver{serial: 1}, db, runtimeList()).cache
	if _, err := New(Options{Cache: cache, Blobs: db, ClusterID: "c", CaptureKeys: func() dnsserver.AcquisitionKeys { return nil },
		CaptureZoneKeys: func(*public.DNSZone) dnsserver.AcquisitionKeys { return nil }, ReceiverForKeys: func(*dnsserver.StaticKeyring) Receiver { return nil }}); err == nil {
		t.Fatal("both capture hooks were accepted")
	}
	if _, err := New(Options{Cache: cache, Blobs: db, ClusterID: "c", Receiver: &testReceiver{}, CaptureZoneKeys: func(*public.DNSZone) dnsserver.AcquisitionKeys { return nil }}); err == nil {
		t.Fatal("zone capture without a receiver factory was accepted")
	}
	fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	var owner snapshotowner.Owner[dnsserver.StaticKeyring]
	if err := owner.Publish(t.Context(), keys, func(*dnsserver.StaticKeyring, *dnsserver.StaticKeyring) bool { return false }); err != nil {
		t.Fatal(err)
	}
	var seen []*public.DNSZone
	fx.m.opts.Receiver = nil
	fx.m.opts.CaptureZoneKeys = func(dz *public.DNSZone) dnsserver.AcquisitionKeys {
		seen = append(seen, dz)
		if dz.Spec.Secondary.TSIGKeyRef != "" {
			return nil // stored keys pending
		}
		return acquisitionTestKeys{owner: &owner, cut: owner.Capture()}
	}
	fx.m.opts.ReceiverForKeys = func(k *dnsserver.StaticKeyring) Receiver {
		if k != keys {
			t.Error("factory received another provider")
		}
		return &testReceiver{serial: 1}
	}
	entries := map[string]*entry{}
	fx.m.reconcile(t.Context(), entries, fx.cache.Snapshot())
	e := entries[SourceIdentity(fx.list, 0)]
	if e == nil || e.status.ConfigurationError != "" || e.acquisition.receiver == nil || e.credential != "unsigned" {
		t.Fatal("unsigned feed was not admitted through the zone capture")
	}
	if len(seen) == 0 || seen[0].Spec.ZoneName != "policy.test." || seen[0] == e.dz {
		t.Fatal("zone capture did not receive a detached copy of the entry's configuration")
	}
}
