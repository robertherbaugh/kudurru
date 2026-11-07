package secondary

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
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

type acquisitionTestReceiver struct {
	Receiver
	afterSOA func()
}

func (r acquisitionTestReceiver) SOA(ctx context.Context, dz *public.DNSZone, primary string) (*dns.SOA, error) {
	soa, err := r.Receiver.SOA(ctx, dz, primary)
	if err == nil {
		r.afterSOA()
	}
	return soa, err
}

func TestAcquisitionCapturedNativeTransferAndStoredEpoch(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			makeKeys := func(epoch string) *dnsserver.StaticKeyring {
				t.Helper()
				k, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: transferKey, Algorithm: dns.HmacSHA256, Secret: transferSecret, CredentialEpoch: epoch})
				if err != nil {
					t.Fatal(err)
				}
				return k
			}
			first, second := makeKeys("original"), makeKeys("same-bytes-new-epoch")
			var owner snapshotowner.Owner[dnsserver.StaticKeyring]
			if err := owner.Publish(t.Context(), first, func(a, b *dnsserver.StaticKeyring) bool { return a == b }); err != nil {
				t.Fatal(err)
			}
			var pool *x509.CertPool
			var listener net.Listener
			var err error
			if protocol == "tls" {
				pair, roots := transferCertificate(t, []string{"127.0.0.1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
				pool = roots
				listener, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
			} else {
				listener, err = net.Listen("tcp", "127.0.0.1:0")
			}
			if err != nil {
				t.Fatal(err)
			}
			soa := transferSOA()
			address := transferPrimaryListener(t, first, listener, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				if q.IsTsig() == nil || w.TsigStatus() != nil {
					return
				}
				records := []dns.RR{soa}
				if q.Question[0].Qtype == dns.TypeAXFR {
					records = append(records, transferRR(t, "example.org. 60 IN NS ns.example.org."), dns.Copy(soa))
				}
				msg := transferReply(q, records...)
				msg.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
				if err := w.WriteMsg(msg); err != nil {
					t.Error("native acquisition response failed", err)
				}
			})
			dz := transferZone(true)
			dz.Name, dz.UID = "captured", "captured-zone"
			dz.Spec.Secondary.TransferProtocol, dz.Spec.Secondary.Primaries = protocol, []string{address}
			var once sync.Once
			m := &Manager{opts: Options{ClusterID: "cluster", Blobs: managerDB(t), Now: time.Now,
				CaptureKeys: func() dnsserver.AcquisitionKeys { return acquisitionTestKeys{owner: &owner, cut: owner.Capture()} },
				ReceiverForKeys: func(k *dnsserver.StaticKeyring) Receiver {
					return acquisitionTestReceiver{Receiver: &TransferClient{Keyring: k, RootCAs: pool, Timeout: time.Second}, afterSOA: func() {
						once.Do(func() {
							if err := owner.Publish(t.Context(), second, func(a, b *dnsserver.StaticKeyring) bool { return a == b }); err != nil {
								t.Error(err)
							}
						})
					}}
				},
			}}
			acquired, fingerprint, err := m.captureEntry(dz)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.prepareReceiver(&acquired); err != nil {
				t.Fatal(err)
			}
			e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: fingerprint, acquisition: acquired}
			result := m.refresh(t.Context(), e)
			if result.err != nil || result.data == nil || result.data.Credential != fingerprint || acquired.current() {
				t.Fatal("native SOA/AXFR did not retain the original acquisition cut", result.err)
			}
			if err := acquired.withCurrent(t.Context(), func() error { return m.persist(t.Context(), e.id, result.data) }); err == nil {
				t.Fatal("replaced native acquisition committed local acceptance")
			}
			if _, err := m.opts.Blobs.GetBlob(t.Context(), "secondary/"+e.id); !errors.Is(err, localdb.ErrNoBlob) {
				t.Fatal("refused acquisition left accepted durable data", err)
			}

			fresh, freshFingerprint, err := m.captureEntry(dz)
			if err != nil || freshFingerprint == fingerprint {
				t.Fatal("fresh same-byte epoch reused provenance", err)
			}
			if err := m.prepareReceiver(&fresh); err != nil {
				t.Fatal(err)
			}
			current := &entry{dz: dz, id: e.id, credential: freshFingerprint, acquisition: fresh}
			r := m.refresh(t.Context(), current)
			if r.err != nil {
				t.Fatal("fresh captured native acquisition failed", r.err)
			}
			if err := fresh.withCurrent(t.Context(), func() error { return m.persist(t.Context(), e.id, r.data) }); err != nil {
				t.Fatal(err)
			}
			current.data, current.z = r.data, r.z
			if err := m.publish(t.Context(), map[string]*entry{e.id: current}); err != nil {
				t.Fatal(err)
			}
			for _, keys := range []*dnsserver.StaticKeyring{first, second} {
				bound, err := m.Snapshot().Bind([]*public.DNSZone{dz}, keys)
				if err != nil || (bound.Lookup(e.id) != nil) != (keys == second) {
					t.Fatal("complete binding adopted another stored credential epoch", err)
				}
			}
			restored := &entry{dz: dz, id: e.id, credential: freshFingerprint}
			data, z, err := m.restore(t.Context(), restored)
			if err != nil || !data.ExpiresAt.Equal(r.data.ExpiresAt) || z.AvailableAt(r.data.ExpiresAt) {
				t.Fatal("stored native restart renewed or lost the original expiry", err)
			}
		})
	}
}

func TestAcquisitionFactoryOnlyForNewSecondaryEntry(t *testing.T) {
	m, _, dz := managerFor(t, newManagerClock(), &managerReceiver{serial: 1}, managerDB(t))
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
	m.opts.CaptureKeys = func() dnsserver.AcquisitionKeys { return acquisitionTestKeys{owner: &owner, cut: owner.Capture()} }
	m.opts.ReceiverForKeys = func(k *dnsserver.StaticKeyring) Receiver {
		if k != keys {
			t.Error("factory received another provider")
		}
		factories.Add(1)
		return &managerReceiver{serial: 1}
	}
	id := zone.SecondaryIdentity(dz)
	// Initialized closes before the first Live snapshot is published. Subscribe
	// before reading so the initial reconcile sees this exact published zone.
	published := make(chan struct{}, 1)
	m.opts.Cache.OnPublish(func(*zonecache.Snapshot) {
		select {
		case published <- struct{}{}:
		default:
		}
	})
	readyCtx, cancelReady := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelReady()
	for {
		if snap := m.opts.Cache.Snapshot(); snap != nil && snap.State == zonecache.Live {
			zones := snap.Objects.Zones()
			if len(zones) == 1 && zones[0].UID == dz.UID && zones[0].ResourceVersion == dz.ResourceVersion && zone.SecondaryIdentity(zones[0]) == id {
				break
			}
		}
		select {
		case <-readyCtx.Done():
			t.Fatal("exact Live zone was not published before initial reconciliation")
		case <-published:
		}
	}
	entries := map[string]*entry{}
	m.reconcile(t.Context(), entries)
	if len(entries) != 1 || factories.Load() != 1 {
		t.Fatal("initial entry did not construct exactly one receiver")
	}
	e := entries[id]
	for range 5 {
		m.reconcile(t.Context(), entries)
		if _, err := m.credential(e.dz); err != nil {
			t.Fatal(err)
		}
	}
	r := m.refresh(t.Context(), e)
	if r.err != nil || factories.Load() != 1 {
		t.Fatal("stable reconciliation/acquisition built discarded receivers", r.err)
	}
	install()
	m.reconcile(t.Context(), entries)
	if factories.Load() != 2 || entries[id] == e || e.acquisition.current() {
		t.Fatal("replacement reused an original entry or built extra receivers")
	}
}

type acquisitionHeldReceiver struct {
	started  chan<- struct{}
	canceled chan<- struct{}
	release  <-chan struct{}
}

func (r acquisitionHeldReceiver) SOA(ctx context.Context, _ *public.DNSZone, _ string) (*dns.SOA, error) {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	<-r.release // deliberately prove a canceled worker still owns its slot
	return nil, ctx.Err()
}
func (acquisitionHeldReceiver) AXFR(context.Context, *public.DNSZone, string) ([]dns.RR, error) {
	return nil, errors.New("unexpected transfer from canceled old acquisition")
}

func TestAcquisitionReplacementRetainsSecondaryWorkerBudgetUntilJoin(t *testing.T) {
	m, _, dz := managerFor(t, newManagerClock(), &managerReceiver{serial: 1}, managerDB(t))
	m.opts.MaxConcurrent = 1
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
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	freshStarted, freshBuilt := make(chan struct{}), make(chan struct{})
	var factories atomic.Int64
	m.opts.CaptureKeys = func() dnsserver.AcquisitionKeys { return acquisitionTestKeys{owner: &owner, cut: owner.Capture()} }
	m.opts.KeysChanged = owner.Changes
	m.opts.ReceiverForKeys = func(*dnsserver.StaticKeyring) Receiver {
		if factories.Add(1) == 1 {
			return acquisitionHeldReceiver{started: started, canceled: canceled, release: release}
		}
		close(freshBuilt)
		return &managerReceiver{serial: 2, soaHook: func(context.Context, string) error { close(freshStarted); return nil }}
	}
	var releaseOnce sync.Once
	// Register release after runManager's cleanup so it executes first, even
	// on a failed assertion; shutdown must never wait for the fixture itself.
	stop := runManager(t, m)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("captured acquisition lifetime did not reach controlled boundary")
		}
	}
	wait(started)
	install()
	wait(freshBuilt)
	wait(canceled)
	select {
	case <-freshStarted:
		t.Fatal("replacement started before old canceled worker joined")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	wait(freshStarted)
	awaitManager(t, m, dz, func(st Status) bool { return st.Available && st.Serial == 2 })
	stop()
	if factories.Load() != 2 {
		t.Fatal("unchanged scheduler constructed extra receivers")
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
	*localdb.SnapshotStore
	observed chan<- acquisitionDeadline
}

func (b acquisitionDeadlineBlobs) PutBlob(ctx context.Context, _ string, _ []byte) error {
	deadline, ok := ctx.Deadline()
	b.observed <- acquisitionDeadline{"persist", deadline, ok}
	<-ctx.Done()
	return ctx.Err()
}

func TestAcquisitionSecondaryOriginalDeadlineIncludesBlockedPersistence(t *testing.T) {
	for _, earlierParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled", true: "earlier parent"}[earlierParent], func(t *testing.T) {
			observed := make(chan acquisitionDeadline, 8)
			db := managerDB(t)
			m, _, dz := managerFor(t, newManagerClock(), acquisitionDeadlineReceiver{Receiver: &managerReceiver{serial: 1}, observed: observed}, acquisitionDeadlineBlobs{SnapshotStore: db, observed: observed})
			m.opts.Timeout = 200 * time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			var parentDeadline time.Time
			if earlierParent {
				cancel()
				parentDeadline = time.Now().Add(200 * time.Millisecond)
				ctx, cancel = context.WithDeadline(t.Context(), parentDeadline)
				m.opts.Timeout = time.Second
			}
			done := make(chan error, 1)
			go func() { done <- m.Run(ctx) }()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(3 * time.Second):
						t.Error("deadline-bound manager did not join")
					}
				}
			}()
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
			if earlierParent {
				if original != parentDeadline {
					t.Fatal("job widened the earlier parent deadline")
				}
				select {
				case err := <-done:
					joined = true
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("expired original parent did not join blocked persistence")
				}
			} else {
				awaitManager(t, m, dz, func(st Status) bool { return st.Error != "" })
			}
			if m.Snapshot().Lookup(zone.SecondaryIdentity(dz)) != nil {
				t.Fatal("expired blocked persistence published data")
			}
			if _, err := db.GetBlob(t.Context(), "secondary/"+zone.SecondaryIdentity(dz)); !errors.Is(err, localdb.ErrNoBlob) {
				t.Fatal("expired persistence left accepted data", err)
			}
		})
	}
}

type acquisitionPersistedBlobs struct {
	*localdb.SnapshotStore
	persisted chan<- context.Context
}

func (b acquisitionPersistedBlobs) PutBlob(ctx context.Context, name string, data []byte) error {
	if err := b.SnapshotStore.PutBlob(ctx, name, data); err != nil {
		return err
	}
	b.persisted <- ctx
	return nil
}

func TestAcquisitionSecondaryManagerGateDeadlineRollsBackCandidate(t *testing.T) {
	clock := newManagerClock()
	rx := &managerReceiver{serial: 1}
	persisted := make(chan context.Context, 4)
	m, _, dz := managerFor(t, clock, rx, acquisitionPersistedBlobs{SnapshotStore: managerDB(t), persisted: persisted})
	m.opts.Timeout = 200 * time.Millisecond
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	var owner snapshotowner.Owner[dnsserver.StaticKeyring]
	if err := owner.Publish(t.Context(), keys, func(*dnsserver.StaticKeyring, *dnsserver.StaticKeyring) bool { return false }); err != nil {
		t.Fatal(err)
	}
	m.opts.CaptureKeys = func() dnsserver.AcquisitionKeys { return acquisitionTestKeys{owner: &owner, cut: owner.Capture()} }
	m.opts.ReceiverForKeys = func(*dnsserver.StaticKeyring) Receiver { return rx }
	stop := runManager(t, m)
	awaitManager(t, m, dz, func(st Status) bool { return st.Available && st.Serial == 1 })
	select {
	case <-persisted:
	case <-time.After(3 * time.Second):
		t.Fatal("initial acceptance did not persist")
	}
	held := m.Capture()
	id := zone.SecondaryIdentity(dz)
	oldZone := held.Snapshot().Lookup(id)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	// Release precedes the already registered manager shutdown cleanup.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	go func() {
		done <- m.WithCurrent(t.Context(), held, func() error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("original manager owner was not pinned")
	}
	rx.set(2, nil)
	clock.advance(10 * time.Second)
	m.Wake()
	var original context.Context
	select {
	case original = <-persisted:
	case <-time.After(3 * time.Second):
		t.Fatal("candidate did not persist before the held publication gate")
	}
	select {
	case <-original.Done():
		if !errors.Is(original.Err(), context.DeadlineExceeded) {
			t.Fatal("job did not keep its original deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manager gate wait widened the original job lifetime")
	}
	if !held.Current() || held.Snapshot().Lookup(id) != oldZone {
		t.Fatal("blocked gate exposed a new acquired snapshot")
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
	awaitManager(t, m, dz, func(st Status) bool { return st.Error != "" && st.Serial == 1 })
	if m.Snapshot().Lookup(id) != oldZone {
		t.Fatal("scheduler republished the refused candidate after its original deadline")
	}
	stop()
}
