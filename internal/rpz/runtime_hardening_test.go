package rpz

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type hookedBlobs struct {
	BlobStore
	put         func(string) error
	deleteError bool
}

func (b *hookedBlobs) PutBlob(ctx context.Context, name string, data []byte) error {
	if b.put != nil {
		if err := b.put(name); err != nil {
			return err
		}
	}
	return b.BlobStore.PutBlob(ctx, name, data)
}
func (b *hookedBlobs) DeleteBlob(ctx context.Context, name string) error {
	if b.deleteError {
		return errors.New("delete unavailable")
	}
	return b.BlobStore.DeleteBlob(ctx, name)
}

func runtimeJob(t *testing.T, fx *runtimeFixture) transferJob {
	t.Helper()
	spec, dz, err := sourceConfig(fx.list, 0)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := fx.m.credential(dz)
	if err != nil {
		t.Fatal(err)
	}
	e := &entry{spec: spec, dz: dz, credential: credential}
	return transferJob{entry: e, spec: spec, dz: dz, credential: credential, config: fx.cache.Snapshot()}
}

func TestRuntimeCandidateRaceCannotRestoreWithoutCleanup(t *testing.T) {
	b := &hookedBlobs{BlobStore: runtimeDB(t), deleteError: true}
	fx := newRuntime(t, &testReceiver{serial: 1}, b, runtimeList())
	job := runtimeJob(t, fx)
	result := fx.m.refresh(t.Context(), job)
	if result.err != nil {
		t.Fatal(result.err)
	}
	b.put = func(name string) error {
		if name != candidateName(job.spec.Identity, result.data.Slot) {
			t.Fatal("raced candidate was accepted")
		}
		changed := fx.list.DeepCopy()
		changed.Spec.Sources[0].RPZ.Primaries = []string{"127.0.0.2:5300"}
		if _, err := fx.store.Apply(t.Context(), changed, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { return fx.cache.Snapshot() != job.config })
		return nil
	}
	fx.m.complete(t.Context(), map[string]*entry{job.spec.Identity: job.entry}, result)
	if job.entry.z != nil {
		t.Fatal("raced candidate published")
	}
	if _, _, err := fx.m.restore(t.Context(), job.entry); err == nil {
		t.Fatal("unaccepted candidate restored despite configuration race and unavailable cleanup")
	}
}

func TestRuntimeAcceptanceFailureDoesNotPublishOrRestore(t *testing.T) {
	b := &hookedBlobs{BlobStore: runtimeDB(t)}
	fx := newRuntime(t, &testReceiver{serial: 1}, b, runtimeList())
	job := runtimeJob(t, fx)
	result := fx.m.refresh(t.Context(), job)
	b.put = func(name string) error {
		if strings.HasSuffix(name, "/accepted") {
			return errors.New("commit failed")
		}
		return nil
	}
	fx.m.complete(t.Context(), map[string]*entry{job.spec.Identity: job.entry}, result)
	if job.entry.z != nil || job.entry.status.Error == "" {
		t.Fatal("failed acceptance published candidate")
	}
	if _, _, err := fx.m.restore(t.Context(), job.entry); err == nil {
		t.Fatal("failed acceptance restored candidate")
	}
}

func TestRuntimeRestoreEnvelopeIsolation(t *testing.T) {
	for _, tc := range []string{"cluster", "identity", "credential", "checksum", "confirmation", "expiry", "records", "marker"} {
		t.Run(tc, func(t *testing.T) {
			fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
			job := runtimeJob(t, fx)
			r := fx.m.refresh(t.Context(), job)
			if r.err != nil {
				t.Fatal(r.err)
			}
			switch tc {
			case "cluster":
				r.data.ClusterID = "another-cluster"
			case "identity":
				r.data.Identity = "another-source"
			case "credential":
				r.data.Credential = "another-credential"
			case "confirmation":
				r.data.ConfirmedAt = fx.clock.now().Add(time.Hour)
			case "expiry":
				r.data.ExpiresAt = r.data.ExpiresAt.Add(time.Second)
			case "records":
				r.data.Records[1] = "not an RR"
			}
			if err := fx.m.persist(t.Context(), job.spec.Identity, r.data); err != nil {
				t.Fatal(err)
			}
			if tc != "marker" {
				if err := fx.m.accept(t.Context(), job.spec.Identity, r.data); err != nil {
					t.Fatal(err)
				}
			}
			if tc == "checksum" {
				r.data.Checksum = "corrupt"
				raw, err := json.Marshal(r.data)
				if err != nil {
					t.Fatal(err)
				}
				if err := fx.m.opts.Blobs.PutBlob(t.Context(), candidateName(job.spec.Identity, r.data.Slot), raw); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := fx.m.restore(t.Context(), job.entry); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
}

type rotatingRing struct {
	current atomic.Pointer[dnsserver.StaticKeyring]
}

func (r *rotatingRing) Lookup(name string) ([]byte, string, bool) {
	return r.current.Load().Lookup(name)
}
func (r *rotatingRing) replace(t *testing.T, value byte) {
	t.Helper()
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "feed-key.test.", Algorithm: dns.HmacSHA256, Secret: []byte{value}})
	if err != nil {
		t.Fatal(err)
	}
	r.current.Store(ring)
}

func TestRuntimeCredentialRotationRecovers(t *testing.T) {
	list := runtimeList()
	list.Spec.Sources[0].RPZ.TSIGKeyRef = "feed-key.test."
	rx := &testReceiver{serial: 1}
	fx := newRuntime(t, rx, runtimeDB(t), list)
	ring := &rotatingRing{}
	ring.replace(t, 1)
	fx.m.opts.Keyring = ring
	stop := startRuntime(t, fx.m)
	awaitStatus(t, fx, func(s Status) bool { return s.Available })
	stop()
	// Block replacement transfers so the revoked copy's disappearance can be
	// observed independently of successful acquisition with new credentials.
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	rx.mu.Lock()
	rx.serial = 8
	rx.axfrHook = func(ctx context.Context, _ uint32) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	rx.mu.Unlock()
	ring.replace(t, 2)
	startRuntime(t, fx.m)
	eventually(t, func() bool { return fx.m.Snapshot().Lookup(SourceIdentity(fx.list, 0)) == nil })
	if d := fx.m.Snapshot().Select([]*v1alpha1.BlockList{fx.list}, true).Evaluate(Context{Name: "bad.test."}, fx.clock.now()); !d.Unavailable {
		t.Fatal("revoked copy remained selectable")
	}
	once.Do(func() { close(release) })
	awaitStatus(t, fx, func(s Status) bool { return s.Available && s.Serial == 8 && s.ConfigurationError == "" })
}

func TestRuntimeReconcileClearsTransientConfigurationError(t *testing.T) {
	fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
	job := runtimeJob(t, fx)
	job.entry.status.ConfigurationError = "credentials changed during persistence"
	entries := map[string]*entry{job.spec.Identity: job.entry}
	fx.m.reconcile(t.Context(), entries, job.config)
	if entries[job.spec.Identity].status.ConfigurationError != "" {
		t.Fatal("valid current configuration remained permanently disabled")
	}
}

type expiryReceiver struct {
	testReceiver
	clock                   *runtimeClock
	advanceSOA, advanceIXFR bool
}

func (r *expiryReceiver) SOA(ctx context.Context, dz *v1alpha1.DNSZone, p string) (*dns.SOA, error) {
	soa, err := r.testReceiver.SOA(ctx, dz, p)
	if r.advanceSOA {
		r.clock.add(30 * time.Second)
	}
	return soa, err
}
func (r *expiryReceiver) IXFR(ctx context.Context, dz *v1alpha1.DNSZone, p string, base []dns.RR) ([]dns.RR, error) {
	records, err := r.testReceiver.IXFR(ctx, dz, p, base)
	if r.advanceIXFR {
		r.clock.add(30 * time.Second)
	}
	return records, err
}
func TestRuntimeExpiryDuringConfirmationOrIncremental(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		t.Run(map[bool]string{false: "SOA", true: "IXFR"}[incremental], func(t *testing.T) {
			rx := &expiryReceiver{testReceiver: testReceiver{serial: 1}}
			fx := newRuntime(t, rx, runtimeDB(t), runtimeList())
			rx.clock = fx.clock
			job := runtimeJob(t, fx)
			first := fx.m.refresh(t.Context(), job)
			if first.err != nil {
				t.Fatal(first.err)
			}
			job.z, job.data = first.z, first.data
			rx.advanceSOA, rx.advanceIXFR = !incremental, incremental
			if incremental {
				rx.set(2, nil)
			}
			got := fx.m.refresh(t.Context(), job)
			if got.err != nil {
				t.Fatal(got.err)
			}
			_, axfr, ixfr := rx.counts()
			if axfr != 2 || ixfr != map[bool]int{false: 0, true: 1}[incremental] {
				t.Fatalf("expired baseline reused: AXFR=%d IXFR=%d", axfr, ixfr)
			}
			if !got.z.AvailableAt(fx.clock.now()) {
				t.Fatal("fresh complete recovery unavailable")
			}
		})
	}
}

type deadlineReceiver struct{ active, peak, calls atomic.Int64 }

func (r *deadlineReceiver) SOA(ctx context.Context, _ *v1alpha1.DNSZone, _ string) (*dns.SOA, error) {
	n := r.active.Add(1)
	defer r.active.Add(-1)
	for old := r.peak.Load(); n > old; old = r.peak.Load() {
		if r.peak.CompareAndSwap(old, n) {
			break
		}
	}
	r.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*deadlineReceiver) AXFR(context.Context, *v1alpha1.DNSZone, string) ([]dns.RR, error) {
	return nil, errors.New("unexpected AXFR")
}
func TestRuntimeBoundsConcurrentAcquisitionAndDeadline(t *testing.T) {
	list := runtimeList()
	for range 7 {
		list.Spec.Sources = append(list.Spec.Sources, *list.Spec.Sources[0].DeepCopy())
	}
	rx := &deadlineReceiver{}
	fx := newRuntime(t, rx, runtimeDB(t), list)
	fx.m.opts.MaxConcurrent, fx.m.opts.Timeout = 2, 15*time.Millisecond
	startRuntime(t, fx.m)
	eventually(t, func() bool {
		statuses := fx.m.Statuses()
		if len(statuses) != 8 {
			return false
		}
		for _, s := range statuses {
			if s.Error == "" {
				return false
			}
		}
		return true
	})
	if rx.peak.Load() != 2 || rx.calls.Load() != 8 {
		t.Fatalf("scheduler bounds: peak=%d calls=%d", rx.peak.Load(), rx.calls.Load())
	}
}

func TestInvalidConfiguredSourceKeepsCoverage(t *testing.T) {
	fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
	for _, invalid := range []string{"origin", "exclusion", "format", "endpoint"} {
		list := fx.list.DeepCopy()
		switch invalid {
		case "origin":
			list.Spec.Sources[0].RPZ.ZoneName = " "
		case "exclusion":
			list.Spec.Sources[0].Exclude = []string{"outside.test."}
		case "format":
			list.Spec.Sources[0].Format = "hosts"
		case "endpoint":
			list.Spec.Sources[0].RPZ.Primaries = []string{"[::1"}
		}
		d := fx.m.Snapshot().Select([]*v1alpha1.BlockList{list}, true).Evaluate(Context{Name: "unrelated.test."}, fx.clock.now())
		if !d.Unavailable || d.Identity != SourceIdentity(list, 0) || d.List != "default/ads" {
			t.Fatalf("%s lost coverage/identity: %+v", invalid, d)
		}
	}
}

func TestRuntimeAcceptanceRaceInvalidatesWithoutDeletion(t *testing.T) {
	b := &hookedBlobs{BlobStore: runtimeDB(t), deleteError: true}
	fx := newRuntime(t, &testReceiver{serial: 1}, b, runtimeList())
	job := runtimeJob(t, fx)
	result := fx.m.refresh(t.Context(), job)
	if result.err != nil {
		t.Fatal(result.err)
	}
	var changed sync.Once
	b.put = func(name string) error {
		if name == acceptanceName(job.spec.Identity) {
			changed.Do(func() {
				list := fx.list.DeepCopy()
				list.Spec.Sources[0].RPZ.Primaries = []string{"127.0.0.2:5300"}
				if _, err := fx.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				eventually(t, func() bool { return fx.cache.Snapshot() != job.config })
			})
		}
		return nil
	}
	entries := map[string]*entry{job.spec.Identity: job.entry}
	fx.m.complete(t.Context(), entries, result)
	if job.entry.z != nil {
		t.Fatal("raced acceptance published")
	}
	if _, _, err := fx.m.restore(t.Context(), job.entry); err == nil {
		t.Fatal("raced acceptance restored with failed deletion")
	}
	fx.m.publish(entries)
	if s, _ := fx.m.StatusFor(fx.list, 0); s.Available {
		t.Fatal("absent runtime data claimed available")
	}
}

func TestRuntimeRepeatedConfirmationNeedsNewAcceptance(t *testing.T) {
	fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
	job := runtimeJob(t, fx)
	first := fx.m.refresh(t.Context(), job)
	if first.err != nil {
		t.Fatal(first.err)
	}
	if err := fx.m.persist(t.Context(), job.spec.Identity, first.data); err != nil {
		t.Fatal(err)
	}
	if err := fx.m.accept(t.Context(), job.spec.Identity, first.data); err != nil {
		t.Fatal(err)
	}
	// Identical records and a fixed/low-resolution clock must not let a prior
	// acceptance marker authorize a newly canceled candidate.
	second := fx.m.refresh(t.Context(), job)
	if second.err != nil {
		t.Fatal(second.err)
	}
	if err := fx.m.persist(t.Context(), job.spec.Identity, second.data); err != nil {
		t.Fatal(err)
	}
	if first.data.Checksum == second.data.Checksum {
		t.Fatal("candidate reused prior durable identity")
	}
	if _, _, err := fx.m.restore(t.Context(), job.entry); err == nil {
		t.Fatal("prior marker accepted new candidate")
	}
}

func TestRuntimeFailedAcceptanceRetainsDurableLastGood(t *testing.T) {
	b := &hookedBlobs{BlobStore: runtimeDB(t)}
	rx := &testReceiver{serial: 1}
	fx := newRuntime(t, rx, b, runtimeList())
	job := runtimeJob(t, fx)
	first := fx.m.refresh(t.Context(), job)
	if first.err != nil {
		t.Fatal(first.err)
	}
	entries := map[string]*entry{job.spec.Identity: job.entry}
	fx.m.complete(t.Context(), entries, first)
	job.data, job.z = job.entry.data, job.entry.z
	rx.set(2, nil)
	fx.clock.add(10 * time.Second)
	second := fx.m.refresh(t.Context(), job)
	if second.err != nil {
		t.Fatal(second.err)
	}
	b.put = func(name string) error {
		if name == acceptanceName(job.spec.Identity) {
			return errors.New("acceptance unavailable")
		}
		return nil
	}
	fx.m.complete(t.Context(), entries, second)
	data, z, err := fx.m.restore(t.Context(), job.entry)
	if err != nil || z.SOA().Serial != 1 || !data.ExpiresAt.Equal(first.data.ExpiresAt) {
		t.Fatalf("failed candidate destroyed durable last-good: %v", err)
	}
}
