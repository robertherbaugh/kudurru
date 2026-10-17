package dhcpkube

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	dhcpv1 "github.com/robertherbaugh/kudurru/api/dhcp/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// fakeLeases is an in-memory DHCPLease API with apiserver semantics:
// resourceVersion preconditions, unique names and an ordered watch that
// replays history after a resourceVersion.
type fakeLeases struct {
	// delay holds a write's caller after its event is out, widening the
	// window in which a watch event overtakes the write's return.
	delay    atomic.Int64
	mu       sync.Mutex
	rv       int
	objs     map[string]dhcpv1.DHCPLease
	history  []watch.Event
	watchers []*watch.FakeWatcher
}

func newFakeLeases() *fakeLeases { return &fakeLeases{objs: map[string]dhcpv1.DHCPLease{}} }

func (f *fakeLeases) pause() { time.Sleep(time.Duration(f.delay.Load())) }

func (f *fakeLeases) emit(t watch.EventType, obj dhcpv1.DHCPLease) {
	ev := watch.Event{Type: t, Object: obj.DeepCopy()}
	f.history = append(f.history, ev)
	for _, w := range f.watchers {
		w.Action(ev.Type, ev.Object)
	}
}

func (f *fakeLeases) List(context.Context) ([]dhcpv1.DHCPLease, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dhcpv1.DHCPLease, 0, len(f.objs))
	for _, o := range f.objs {
		out = append(out, *o.DeepCopy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, strconv.Itoa(f.rv), nil
}

func (f *fakeLeases) Watch(_ context.Context, rv string) (watch.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	after, _ := strconv.Atoi(rv)
	w := watch.NewFakeWithChanSize(4096, false)
	for _, ev := range f.history {
		if n, _ := strconv.Atoi(ev.Object.(*dhcpv1.DHCPLease).ResourceVersion); n > after {
			w.Action(ev.Type, ev.Object)
		}
	}
	f.watchers = append(f.watchers, w)
	return w, nil
}

func (f *fakeLeases) Get(_ context.Context, name string) (*dhcpv1.DHCPLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[name]
	if !ok {
		return nil, ErrNotFound
	}
	return o.DeepCopy(), nil
}

func (f *fakeLeases) next() string {
	f.rv++
	return strconv.Itoa(f.rv)
}

func (f *fakeLeases) Create(_ context.Context, obj *dhcpv1.DHCPLease) error {
	defer f.pause()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objs[obj.Name]; ok {
		return ErrExists
	}
	obj.ResourceVersion = f.next()
	f.objs[obj.Name] = *obj.DeepCopy()
	f.emit(watch.Added, *obj)
	return nil
}

func (f *fakeLeases) Update(_ context.Context, obj *dhcpv1.DHCPLease) error {
	defer f.pause()
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.objs[obj.Name]
	if !ok {
		return ErrNotFound
	}
	if cur.ResourceVersion != obj.ResourceVersion {
		return ErrConflict
	}
	obj.ResourceVersion = f.next()
	f.objs[obj.Name] = *obj.DeepCopy()
	f.emit(watch.Modified, *obj)
	return nil
}

func (f *fakeLeases) Delete(_ context.Context, name, rv string) error {
	defer f.pause()
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.objs[name]
	if !ok {
		return ErrNotFound
	}
	if cur.ResourceVersion != rv {
		return ErrConflict
	}
	delete(f.objs, name)
	cur.ResourceVersion = f.next()
	f.emit(watch.Deleted, cur)
	return nil
}

// putSilently creates an object the replicas have not seen yet, as if
// another allocator wrote it and the watch were behind.
func (f *fakeLeases) putSilently(obj *dhcpv1.DHCPLease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj.ResourceVersion = f.next()
	f.objs[obj.Name] = *obj.DeepCopy()
}

func (f *fakeLeases) get(name string) (dhcpv1.DHCPLease, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[name]
	return o, ok
}

// fakeElection hands one term at a time to the named replica.
type fakeElection struct {
	mu     sync.Mutex
	holder string
	change chan struct{}
}

func newFakeElection() *fakeElection { return &fakeElection{change: make(chan struct{})} }

func (e *fakeElection) grant(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holder = name
	close(e.change)
	e.change = make(chan struct{})
}

func (e *fakeElection) state() (string, chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.holder, e.change
}

type fakeElector struct {
	election *fakeElection
	name     string
	leading  bool
	mu       sync.Mutex
}

func (e *fakeElector) Run(ctx context.Context, onStarted func(context.Context), onStopped func()) error {
	for {
		holder, change := e.election.state()
		if holder != e.name {
			select {
			case <-ctx.Done():
				return nil
			case <-change:
				continue
			}
		}
		term, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-change:
			case <-ctx.Done():
			}
			cancel()
		}()
		e.mu.Lock()
		e.leading = true
		e.mu.Unlock()
		onStarted(term)
		<-term.Done()
		e.mu.Lock()
		e.leading = false
		e.mu.Unlock()
		onStopped()
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (e *fakeElector) IsLeader() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leading
}

func (e *fakeElector) Leader() string {
	holder, _ := e.election.state()
	return holder
}

type fakePods struct {
	mu     sync.Mutex
	active map[string]bool
}

func (p *fakePods) SetActive(_ context.Context, pod string, active bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if active {
		p.active[pod] = true
	} else {
		delete(p.active, pod)
	}
	return nil
}

func (p *fakePods) ActivePods(context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for pod := range p.active {
		out = append(out, pod)
	}
	sort.Strings(out)
	return out, nil
}

func testScope() *v1alpha1.DHCPScope {
	return &v1alpha1.DHCPScope{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindDHCPScope},
		ObjectMeta: metav1.ObjectMeta{Name: "lan"},
		Spec: v1alpha1.DHCPScopeSpec{Subnet: "192.0.2.0/24", RangeStart: "192.0.2.10", RangeEnd: "192.0.2.12",
			Router: "192.0.2.1", DNSServers: []string{"192.0.2.53"}, DomainName: "leases.test", RegisterDNS: true,
			ConflictDetection: "disabled",
			TrustedRelays:     []v1alpha1.DHCPTrustedRelay{{SourceIP: "192.0.2.1", GatewayIP: "192.0.2.1"}}},
	}
}

type harness struct {
	t        *testing.T
	ctx      context.Context
	st       *memory.Store
	leases   *fakeLeases
	election *fakeElection
	pods     *fakePods
	now      time.Time
	nowMu    sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, st: memory.New(), leases: newFakeLeases(), election: newFakeElection(),
		pods: &fakePods{active: map[string]bool{}}, now: time.Now().UTC().Truncate(time.Second)}
	if _, err := h.st.Apply(ctx, testScope(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) clock() time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	return h.now
}

func (h *harness) replica(name string, ip string) *Runtime {
	h.t.Helper()
	r, err := New(Options{Store: h.st, Leases: h.leases, Pods: h.pods, Elector: &fakeElector{election: h.election, name: name},
		PodName: name, ServerIdentifier: netip.MustParseAddr("198.51.100.67"), DataDir: h.t.TempDir(),
		Now: h.clock, Interval: time.Hour,
		LocalAddress: func(string) (netip.Addr, error) { return netip.MustParseAddr(ip), nil },
		Listen:       func(*dhcp.Service) error { return nil },
		Serve:        func(ctx context.Context, _ *dhcp.Service) error { <-ctx.Done(); return nil }})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-done })
	return r
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func listed(r *Runtime, scope string) []dhcp.Lease {
	s, err := r.Store(scope)
	if err != nil {
		return nil
	}
	leases, err := s.List(context.Background())
	if err != nil {
		return nil
	}
	return leases
}

func bind(t *testing.T, r *Runtime, mac, hostname string) (dhcp.Lease, error) {
	t.Helper()
	var lease dhcp.Lease
	err := r.mutate(context.Background(), "lan", func(ctx context.Context, s *dhcp.LeaseStore) error {
		client := dhcp.Client{MAC: mac, Hostname: hostname}
		offer, err := s.Offer(ctx, client, netip.Addr{})
		if err != nil {
			return err
		}
		lease, err = s.Acknowledge(ctx, client, offer.Address, dhcp.Selecting)
		return err
	})
	return lease, err
}

func TestReplicasShareLeasesAndFailOver(t *testing.T) {
	h := newHarness(t)
	a := h.replica("kudurru-a", "10.0.0.1")
	b := h.replica("kudurru-b", "10.0.0.2")
	h.election.grant("kudurru-a")
	eventually(t, "a serving", func() bool { return a.Leader() && len(a.Status().Scopes) == 1 })
	eventually(t, "a labeled", func() bool { pods, _ := h.pods.ActivePods(h.ctx); return len(pods) == 1 && pods[0] == "kudurru-a" })

	lease, err := bind(t, a, "02:00:00:00:00:01", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := h.leases.get(ObjectName("lan", lease.Address))
	if !ok || obj.Spec.State != "bound" || obj.Spec.DNSClaim == nil || obj.Spec.DNSClaim.Name != "laptop.leases.test." {
		t.Fatalf("lease object = %+v, %v", obj, ok)
	}
	// Every replica answers the lease name and lists the lease.
	eventually(t, "b sees the lease", func() bool {
		s, err := b.Store("lan")
		if err != nil {
			return false
		}
		got, ok := s.DNS().Address(lease.Address)
		return ok && got.Name == "laptop.leases.test." && len(listed(b, "lan")) == 1
	})

	// Failover: b takes the term and never reuses the bound address.
	h.election.grant("kudurru-b")
	eventually(t, "b serving", func() bool { return b.Leader() && !a.Leader() })
	eventually(t, "b labeled", func() bool { pods, _ := h.pods.ActivePods(h.ctx); return len(pods) == 1 && pods[0] == "kudurru-b" })
	second, err := bind(t, b, "02:00:00:00:00:02", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if second.Address == lease.Address {
		t.Fatalf("b reused %s", lease.Address)
	}
	eventually(t, "a sees b's lease", func() bool { return len(listed(a, "lan")) == 2 })
	if _, ok := h.leases.get(ObjectName("lan", lease.Address)); !ok {
		t.Fatal("the failover lost a's lease object")
	}
}

func TestAddressOwnershipSurvivesAStaleView(t *testing.T) {
	h := newHarness(t)
	a := h.replica("kudurru-a", "10.0.0.1")
	h.election.grant("kudurru-a")
	eventually(t, "a serving", func() bool { return a.Leader() && len(a.Status().Scopes) == 1 })

	// Another allocator already owns .10, but a has not seen it yet.
	other := leaseObject("", "lan", dhcp.ExternalLease{Record: dhcp.LeaseRecord{Address: netip.MustParseAddr("192.0.2.10"),
		Identity: dhcp.LeaseIdentity{Kind: "mac", Value: "02:00:00:00:00:99"}, MAC: "02:00:00:00:00:99",
		ExpiresAt: h.clock().Add(time.Hour), State: "bound"}})
	h.leases.putSilently(other)

	if _, err := bind(t, a, "02:00:00:00:00:01", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("first allocation = %v, want the create to be refused", err)
	}
	// The refused write rereads the object, so the retry picks another address.
	eventually(t, "a learns .10", func() bool { return len(listed(a, "lan")) == 1 })
	lease, err := bind(t, a, "02:00:00:00:00:01", "")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Address == netip.MustParseAddr("192.0.2.10") {
		t.Fatal("two clients own 192.0.2.10")
	}
	if obj, _ := h.leases.get(ObjectName("lan", netip.MustParseAddr("192.0.2.10"))); obj.Spec.MAC != "02:00:00:00:00:99" {
		t.Fatalf("the other allocator's lease changed: %+v", obj.Spec)
	}
}

func TestJanitorStatusAndLiveSettings(t *testing.T) {
	h := newHarness(t)
	a := h.replica("kudurru-a", "10.0.0.1")
	b := h.replica("kudurru-b", "10.0.0.2")
	h.election.grant("kudurru-a")
	eventually(t, "a serving", func() bool { return a.Leader() && len(a.Status().Scopes) == 1 })
	lease, err := bind(t, a, "02:00:00:00:00:01", "laptop")
	if err != nil {
		t.Fatal(err)
	}

	// The serving replica writes the scope status.
	a.maintain(h.ctx)
	var scope v1alpha1.DHCPScope
	if err := h.st.Get(h.ctx, store.Key{Kind: v1alpha1.KindDHCPScope, Name: "lan"}, &scope); err != nil {
		t.Fatal(err)
	}
	if scope.Status.ServingPod != "kudurru-a" || scope.Status.ActiveLeases != 1 || len(scope.Status.Conditions) != 1 || scope.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("status = %+v", scope.Status)
	}

	// A reservation edit applies live on every replica and renames the lease.
	scope.Spec.Reservations = []v1alpha1.DHCPReservation{{MAC: "02:00:00:00:00:01", IP: lease.Address.String(), Hostname: "workstation"}}
	if _, err := h.st.Apply(h.ctx, &scope, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b renames the lease", func() bool {
		s, err := b.Store("lan")
		if err != nil {
			return false
		}
		got, ok := s.DNS().Address(lease.Address)
		return ok && got.Name == "workstation.leases.test." && s.Configuration().Revision == 2
	})

	// Expired objects are deleted by the serving replica only.
	h.nowMu.Lock()
	h.now = h.now.Add(13 * time.Hour)
	h.nowMu.Unlock()
	a.maintain(h.ctx)
	eventually(t, "expired lease deleted", func() bool {
		_, ok := h.leases.get(ObjectName("lan", lease.Address))
		return !ok
	})
	eventually(t, "replicas drop the expired lease", func() bool {
		sa, _ := a.Store("lan")
		sb, _ := b.Store("lan")
		_, okA := sa.DNS().Address(lease.Address)
		_, okB := sb.DNS().Address(lease.Address)
		return !okA && !okB
	})
}

func TestInvalidAndOverlappingScopes(t *testing.T) {
	h := newHarness(t)
	overlap := testScope()
	overlap.Name = "zz-overlap"
	overlap.Spec.Subnet = "192.0.2.0/25"
	if _, err := h.st.Apply(h.ctx, overlap, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	broken := testScope()
	broken.Name = "broken"
	broken.Spec.Subnet = "198.51.100.0/24"
	broken.Spec.RangeStart = "198.51.100.20"
	broken.Spec.RangeEnd = "198.51.100.10"
	if _, err := h.st.Apply(h.ctx, broken, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	a := h.replica("kudurru-a", "10.0.0.1")
	eventually(t, "scopes reconciled", func() bool { st := a.Status(); return len(st.Scopes) == 1 && len(st.Invalid) == 2 })
	st := a.Status()
	if st.Scopes[0] != "lan" || st.Invalid["zz-overlap"] == "" || st.Invalid["broken"] == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestLeaseObjectRoundTrip(t *testing.T) {
	for _, lease := range []dhcp.ExternalLease{
		{Record: dhcp.LeaseRecord{Address: netip.MustParseAddr("192.0.2.10"), Identity: dhcp.LeaseIdentity{Kind: "mac", Value: "02:00:00:00:00:01"},
			MAC: "02:00:00:00:00:01", Hostname: "laptop", ExpiresAt: time.Unix(1_900_000_000, 0).UTC(), State: "bound"},
			Claim: dhcp.DNSClaim{Name: "laptop.leases.test.", Epoch: 2, Ticket: 77, Ordinal: 1}},
		{Record: dhcp.LeaseRecord{Address: netip.MustParseAddr("192.0.2.11"), Identity: dhcp.LeaseIdentity{Kind: "client-id", Value: "MDFhYmNk"},
			MAC: "02:00:00:00:00:02", ExpiresAt: time.Unix(1_900_000_060, 0).UTC(), State: "declined"}},
	} {
		obj := leaseObject("kudurru", "lan", lease)
		scope, got, err := leaseFromObject(obj)
		if err != nil || scope != "lan" || fmt.Sprint(got) != fmt.Sprint(lease) {
			t.Fatalf("round trip = %q %+v %v, want %+v", scope, got, err, lease)
		}
	}
	bad := leaseObject("kudurru", "lan", dhcp.ExternalLease{Record: dhcp.LeaseRecord{Address: netip.MustParseAddr("192.0.2.10"), Identity: dhcp.LeaseIdentity{Kind: "mac", Value: "02:00:00:00:00:01"}, MAC: "02:00:00:00:00:01", State: "bound"}})
	bad.Name = "lan.192.0.2.99"
	if _, _, err := leaseFromObject(bad); err == nil {
		t.Fatal("a lease whose name does not match its address was accepted")
	}
}

// The event of this replica's own write can arrive before the write
// returns. It must not be applied after a newer local write (the binding
// would revert to an offer in the working copy).
func TestOwnEventsDoNotRevertNewerWrites(t *testing.T) {
	h := newHarness(t)
	a := h.replica("kudurru-a", "10.0.0.1")
	h.election.grant("kudurru-a")
	eventually(t, "a serving", func() bool { return a.Leader() && len(a.Status().Scopes) == 1 })
	h.leases.delay.Store(int64(30 * time.Millisecond))
	for i := 1; i <= 3; i++ {
		// Offer and acknowledge in separate packets, as clients do.
		client := dhcp.Client{MAC: fmt.Sprintf("02:00:00:00:00:%02x", i)}
		var lease dhcp.Lease
		if err := a.mutate(context.Background(), "lan", func(ctx context.Context, s *dhcp.LeaseStore) error {
			var err error
			lease, err = s.Offer(ctx, client, netip.Addr{})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // the client's REQUEST follows later
		if err := a.mutate(context.Background(), "lan", func(ctx context.Context, s *dhcp.LeaseStore) error {
			_, err := s.Acknowledge(ctx, client, lease.Address, dhcp.Selecting)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // let any stale event land
		for _, l := range listed(a, "lan") {
			if l.Address == lease.Address && l.State != "bound" {
				t.Fatalf("working copy reverted %s to %s", l.Address, l.State)
			}
		}
		// Another writer's change right after is applied promptly: the own
		// event must not leave a pending entry that hides it.
		obj, err := h.leases.Get(context.Background(), ObjectName("lan", lease.Address))
		if err != nil {
			t.Fatal(err)
		}
		obj.Spec.Hostname = fmt.Sprintf("renamed-%d", i)
		if err := h.leases.Update(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the other writer's change", func() bool {
			for _, l := range listed(a, "lan") {
				if l.Address == lease.Address {
					return l.Hostname == obj.Spec.Hostname
				}
			}
			return false
		})
	}
}

// A watch event that waits for the scope's packet lock while a newer local
// write commits must be dropped, not applied over that write.
func TestStaleEventWaitingForThePacketLock(t *testing.T) {
	h := newHarness(t)
	a := h.replica("kudurru-a", "10.0.0.1")
	h.election.grant("kudurru-a")
	eventually(t, "a serving", func() bool { return a.Leader() && len(a.Status().Scopes) == 1 })
	client := dhcp.Client{MAC: "02:00:00:00:00:01", Hostname: "laptop"}
	lease, err := bind(t, a, client.MAC, client.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	name := ObjectName("lan", lease.Address)

	entered, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- a.mutate(context.Background(), "lan", func(ctx context.Context, s *dhcp.LeaseStore) error {
			close(entered)
			<-proceed
			renewed := client
			renewed.Hostname = "laptop-2"
			_, err := s.Acknowledge(ctx, renewed, lease.Address, dhcp.Renewing)
			return err
		})
	}()
	<-entered
	// Another writer changes the object; its event waits for the lock.
	obj, err := h.leases.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	obj.Spec.Hostname = "intruder"
	if err := h.leases.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event in the cache", func() bool { return a.cached(name, obj.ResourceVersion, false) })
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the queued event runs now
	current, err := h.leases.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed(a, "lan") {
		if l.Address == lease.Address && l.Hostname != current.Spec.Hostname {
			t.Fatalf("working copy hostname %q, object %q", l.Hostname, current.Spec.Hostname)
		}
	}
	if current.Spec.Hostname != "laptop-2" {
		t.Fatalf("the renewal did not write over the other writer: %q", current.Spec.Hostname)
	}
}
