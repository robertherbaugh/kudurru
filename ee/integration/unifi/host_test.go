package unifi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// guardedFixture runs the actual host lifecycle/elector/ledger budget writer.
// Its injected root admission is test-only; production root performs durable
// source allocation, permissions, ownership and publication fencing separately.
type guardedFixture struct {
	h         *host.Host
	p         *Provider
	st        *memory.Store
	inventory *inventory.Holder
	mu        sync.RWMutex
	input     host.Input
	token     string
	permitted bool
	batches   []host.DesiredBatch
	writes    atomic.Int32
	now       atomic.Int64
	cancel    context.CancelFunc
	done      chan error
}

func capturedObjects(t *testing.T, objects ...store.Object) zonecache.ObjectSet {
	t.Helper()
	st := memory.New()
	kinds := []store.Kind{}
	for _, o := range objects {
		if _, err := st.Apply(context.Background(), o, store.ApplyOptions{Manager: "fixture"}); err != nil {
			t.Fatal(err)
		}
		kind := store.KindOf(o)
		found := false
		for _, k := range kinds {
			found = found || k == kind
		}
		if !found {
			kinds = append(kinds, kind)
		}
	}
	cache, err := zonecache.New(st, zonecache.Options{Kinds: kinds, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan zonecache.ObjectSet, 1)
	cache.OnPublish(func(s *zonecache.Snapshot) {
		count := 0
		for _, k := range kinds {
			count += s.Objects.Count(k)
		}
		if count == len(objects) {
			select {
			case ready <- s.Objects:
			default:
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cache.Start(ctx) }()
	select {
	case objects := <-ready:
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return objects
	case <-ctx.Done():
		t.Fatal("object cut did not publish")
		return zonecache.ObjectSet{}
	}
}
func guardedProvider(t *testing.T, sTransport *http.Transport, endpoint string) *guardedFixture {
	t.Helper()
	f := &guardedFixture{st: memory.New(), token: fixtureKey, permitted: true, done: make(chan error, 1)}
	f.now.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, f.now.Load()).UTC() }
	f.inventory = inventory.New(now)
	c := public.UniFiIntegration{Endpoint: endpoint, Poll: metav1.Duration{Duration: time.Hour}, Inventory: &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "lan"}}, Groups: public.UniFiGroups{ByNetwork: map[string]string{"LAN": "home"}}}
	definition := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "unifi-home", Namespace: "dns", UID: "integration-original"}, Spec: public.IntegrationSpec{Type: "unifi", UniFi: &c, Credentials: map[string]public.CredentialReference{"apiKey": {Env: "UNIFI_FIXTURE_KEY"}}}}
	zone := translationFixture().Zone.DeepCopy()
	objects := capturedObjects(t, definition, zone)
	resolver, err := credentials.New(credentials.Options{Environment: []string{"UNIFI_FIXTURE_KEY"}, FingerprintKey: []byte(strings.Repeat("k", 32)), Getenv: func(name string) (string, bool) {
		f.mu.RLock()
		defer f.mu.RUnlock()
		return f.token, name == "UNIFI_FIXTURE_KEY"
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.input = host.Input{Objects: objects, Credentials: resolver, Complete: true, BundleID: "original-cut", Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}}
	f.p = New()
	f.p.transport = sTransport
	f.p.now = now
	repo, err := ledger.New(ledger.Options{Reader: f.st, Now: now, Write: func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		var written store.Object
		err := f.h.WithLease(ctx, g, func(ctx context.Context) error {
			if err := f.fence(ctx, host.ExecutionGuard{Ledger: g, BundleID: f.bundle()}); err != nil {
				return err
			}
			var err error
			written, err = f.st.Apply(ctx, o, a)
			if err == nil {
				f.writes.Add(1)
			}
			return err
		})
		return written, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.h, err = host.New(host.Options{Capture: func(context.Context) (host.Input, error) { f.mu.RLock(); defer f.mu.RUnlock(); return f.input, nil }, Factory: func(string) (integration.Integration, error) { return f.p, nil }, Elector: f.st.Elector, Fence: f.fence, Ledger: repo, Reader: f.st, Inventory: f.inventory, Publish: func(ctx context.Context, g host.ExecutionGuard, b host.DesiredBatch) error {
		if err := f.fence(ctx, g); err != nil {
			return err
		}
		return f.h.WithLease(ctx, g.Ledger, func(context.Context) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if g.BundleID != f.input.BundleID {
				return host.ErrFenced
			}
			f.batches = append(f.batches, b)
			return nil
		})
	}, Allowed: func() bool { f.mu.RLock(); defer f.mu.RUnlock(); return f.permitted }, Now: now, PollInterval: time.Minute, OperationTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() { f.done <- f.h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-f.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("host failed to quiesce")
		}
	})
	await(t, func() bool {
		return f.inventory.Snapshot() != nil && f.inventory.Snapshot().Lookup(netip.MustParseAddr("192.0.2.3"), now()) != nil
	})
	return f
}
func (f *guardedFixture) bundle() string { f.mu.RLock(); defer f.mu.RUnlock(); return f.input.BundleID }
func (f *guardedFixture) fence(ctx context.Context, g host.ExecutionGuard) error {
	f.mu.RLock()
	input, allowed := f.input, f.permitted
	f.mu.RUnlock()
	if !allowed || !input.Complete || input.BundleID != g.BundleID || input.Term != g.Ledger.Term {
		return host.ErrFenced
	}
	instances, err := host.CaptureInstances(ctx, input)
	if err != nil || len(instances) != 1 {
		return host.ErrFenced
	}
	current, err := instances[0].Guard(g.Ledger.LeaseID)
	if err != nil || current != g.Ledger {
		return host.ErrFenced
	}
	return nil
}
func (f *guardedFixture) owner() host.InstanceID { return f.p.host.IntegrationOwner() }
func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("guarded host did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGuardedHostPublishesAndQueriesNeverCallRemote(t *testing.T) {
	s, remote := controller(t)
	f := guardedProvider(t, s.Client().Transport.(*http.Transport), s.URL)
	if f.writes.Load() == 0 {
		t.Fatal("HTTP attempts did not reserve durable budgets")
	}
	f.mu.RLock()
	if len(f.batches) != 1 || len(f.batches[0].RecordSets) != 2 || len(f.batches[0].ClientGroups) != 1 {
		t.Fatal("host skipped bounded desired publication")
	}
	group := f.batches[0].ClientGroups[0].DeepCopy()
	f.mu.RUnlock()
	before := f.inventory.Snapshot()
	at := time.Unix(0, f.now.Load()).UTC()
	ip := netip.MustParseAddr("192.0.2.3")
	s.Close()
	remote.mu.Lock()
	callsBefore := 0
	for _, n := range remote.calls {
		callsBefore += n
	}
	remote.mu.Unlock()
	ix := policy.Build(policy.Input{ClientGroups: []*public.ClientGroup{group}}, policy.Options{Inventory: true})
	stage := identify.New(policy.Static{Idx: ix}, nil)
	stage.Inventory = f.inventory
	msg := new(dns.Msg)
	msg.SetQuestion("a.external.", dns.TypeA)
	req := &pipeline.Request{Ctx: context.Background(), Msg: msg, Received: at, Client: pipeline.Client{Addr: netip.AddrPortFrom(ip, 12345)}}
	next := func(*pipeline.Request) (*pipeline.Response, error) { return &pipeline.Response{Msg: new(dns.Msg)}, nil }
	if _, err := stage.Serve(req, next); err != nil {
		t.Fatal(err)
	}
	if !req.Client.IdentityCaptured || len(req.Client.MACs) != 1 || len(req.Client.GroupIDs) != 1 || req.Client.GroupIDs[0] != "dns/home" || len(req.Client.Names) == 0 {
		t.Fatal("memory identity not captured", req.Client)
	}
	// A query and its synchronous alias dependency retain their original identity
	// after a complete inventory withdrawal. Known-empty results pin absence too.
	source := host.SourceID(f.owner())
	if err := f.inventory.Replace(source, before.Generation()+1, nil); err != nil {
		t.Fatal(err)
	}
	alias := *req
	alias.Msg = new(dns.Msg)
	alias.Msg.SetQuestion("alias.external.", dns.TypeAAAA)
	if _, err := stage.Serve(&alias, next); err != nil || len(alias.Client.MACs) != 1 {
		t.Fatal("alias rebound to newer inventory", err)
	}
	empty := &pipeline.Request{Ctx: context.Background(), Msg: msg, Received: at, Client: pipeline.Client{Addr: req.Client.Addr}}
	if _, err := stage.Serve(empty, next); err != nil || !empty.Client.IdentityCaptured || len(empty.Client.MACs) != 0 {
		t.Fatal("known-empty not captured", err)
	}
	f.now.Store(at.Add(time.Second).UnixNano())
	if err := f.inventory.Replace(source, before.Generation()+2, []inventory.Client{{ID: "new-owner", Source: source, MACs: []string{"02:00:00:00:00:02"}, IPs: []netip.Addr{ip}, ObservedAt: at.Add(time.Second), ExpiresAt: at.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	empty.Received = at.Add(time.Second)
	if _, err := stage.Serve(empty, next); err != nil || len(empty.Client.MACs) != 0 {
		t.Fatal("known-empty inherited new IP owner", err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	callsAfter := 0
	for _, n := range remote.calls {
		callsAfter += n
	}
	if callsAfter != callsBefore {
		t.Fatal("query entered provider transport", callsBefore, callsAfter)
	}
}

func TestGuardedHostOriginalCaptureFencesAcquisitionAndPublication(t *testing.T) {
	for _, mode := range []string{"config", "credentials", "uid", "term", "source", "license", "incomplete", "during_read", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			s, remote := controller(t)
			f := guardedProvider(t, s.Client().Transport.(*http.Transport), s.URL)
			snapshot := f.inventory.Snapshot()
			f.mu.RLock()
			published := len(f.batches)
			input := f.input
			f.mu.RUnlock()
			owner := f.owner()
			invalidate := func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				switch mode {
				case "credentials":
					f.token = "rotated-fixture-key"
				case "term":
					f.input.Term.Epoch++
				case "license":
					f.permitted = false
				case "incomplete":
					f.input.Complete = false
				default:
					f.input.BundleID = "changed-cut"
				}
			}
			if mode == "config" || mode == "uid" {
				obj, _ := input.Objects.List("Integration")[0].(*public.Integration)
				changed := obj.DeepCopy()
				if mode == "uid" {
					changed.UID = "recreated"
				} else {
					changed.Spec.UniFi.Site = "other"
				}
				zone, _ := input.Objects.Zone("lan")
				objects := capturedObjects(t, changed, zone)
				f.mu.Lock()
				f.input.Objects = objects
				f.mu.Unlock()
			} else if mode != "during_read" && mode != "cancellation" {
				invalidate()
			}
			entered := make(chan struct{})
			if mode == "during_read" || mode == "cancellation" {
				remote.mu.Lock()
				remote.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
					if strings.HasSuffix(r.URL.Path, "/clients") {
						close(entered)
						if mode == "during_read" {
							invalidate()
							fixturePage(w, r, remote.clients)
						} else {
							<-r.Context().Done()
						}
						return true
					}
					return false
				}
				remote.mu.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.h.Sync(ctx, owner, ""); done <- err }()
			if mode == "cancellation" {
				select {
				case <-entered:
					cancel()
				case <-ctx.Done():
					t.Fatal("original request never started")
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("obsolete capture admitted")
				}
				if mode == "cancellation" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("fenced operation blocked")
			}
			if f.inventory.Snapshot() != snapshot {
				t.Fatal("stale operation renewed inventory")
			}
			f.mu.RLock()
			defer f.mu.RUnlock()
			if len(f.batches) != published {
				t.Fatal("stale operation published source data")
			}
		})
	}
}

func TestGuardedHostFailedPollPreservesAbsoluteExpiry(t *testing.T) {
	s, remote := controller(t)
	f := guardedProvider(t, s.Client().Transport.(*http.Transport), s.URL)
	snapshot := f.inventory.Snapshot()
	ip := netip.MustParseAddr("192.0.2.3")
	original := snapshot.Lookup(ip, time.Unix(0, f.now.Load()).UTC())
	if original == nil {
		t.Fatal("missing initial assignment")
	}
	f.mu.RLock()
	published := len(f.batches)
	f.mu.RUnlock()
	remote.mu.Lock()
	remote.hook = func(w http.ResponseWriter, _ *http.Request, _ int) bool { w.WriteHeader(503); return true }
	remote.mu.Unlock()
	f.now.Store(original.ExpiresAt.Add(-time.Nanosecond).UnixNano())
	if _, err := f.h.Sync(context.Background(), f.owner(), ""); err == nil {
		t.Fatal("failed poll reported ready")
	}
	if f.inventory.Snapshot() != snapshot || snapshot.Lookup(ip, original.ExpiresAt.Add(-time.Nanosecond)) == nil || snapshot.Lookup(ip, original.ExpiresAt) != nil {
		t.Fatal("failed poll renewed/removed last-good expiry")
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.batches) != published {
		t.Fatal("failed poll published DNS or groups")
	}
}
