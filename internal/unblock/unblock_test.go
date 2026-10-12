package unblock

import (
	"context"
	"fmt"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var sharedValidator = sync.OnceValues(validator.New)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t     *testing.T
	st    store.Store
	svc   *Service
	clk   *clock
	set   config.UnblockRequests
	mode  querylog.Mode
	pseud bool
}

// newFixture opens a sqlite store (with the CRD validator, so every
// write is checked against the UnblockRequest schema) with the default
// group and policy and the kids group and policy.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	v, err := sharedValidator()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: v, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, clk: &clock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}, set: Defaults}
	nameKey, err := NameKey(testEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = New(Options{
		Store: st, Node: "node-a", Now: f.clk.Now, NameKey: nameKey,
		Settings: func() config.UnblockRequests { return f.set },
		Privacy:  func() (querylog.Mode, bool) { return f.mode, f.pseud },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx := context.Background()
	for _, o := range []store.Object{
		&v1alpha1.ClientGroup{ObjectMeta: meta("default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
		&v1alpha1.ClientGroup{ObjectMeta: meta("kids"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.20.0/24"}}}},
		&v1alpha1.Policy{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}},
		&v1alpha1.Policy{ObjectMeta: meta("kids"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, AllowOnly: true}},
	} {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func meta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }

func (f *fixture) submit(client, domain, reason string) *v1alpha1.UnblockRequest {
	f.t.Helper()
	r, err := f.svc.Submit(context.Background(), Submission{
		Client: netip.MustParseAddr(client), Domain: domain, Reason: reason, Name: "Sam",
		Groups: []string{"default/kids"}, Policies: []string{"default/kids"},
		Block: &Block{Kind: "policy", Policy: "default/kids"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) list(name string) []string {
	f.t.Helper()
	bl := &v1alpha1.BlockList{}
	if err := f.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: name}, bl); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, src := range bl.Spec.Sources {
		out = append(out, src.Inline...)
		for _, e := range src.Entries {
			out = append(out, v1alpha1.EntryRule(e.Type, e.Value))
		}
	}
	return out
}

func (f *fixture) entry(list, rule string) v1alpha1.ListEntry {
	f.t.Helper()
	bl := &v1alpha1.BlockList{}
	if err := f.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: list}, bl); err != nil {
		f.t.Fatal(err)
	}
	for _, src := range bl.Spec.Sources {
		for _, e := range src.Entries {
			if v1alpha1.EntryRule(e.Type, e.Value) == rule {
				return e
			}
		}
	}
	f.t.Fatalf("no entry %s in %s", rule, list)
	return v1alpha1.ListEntry{}
}

func (f *fixture) allowRefs(policy string) []string {
	f.t.Helper()
	p := &v1alpha1.Policy{}
	if err := f.st.Get(context.Background(), store.Key{Kind: "Policy", Namespace: "default", Name: policy}, p); err != nil {
		f.t.Fatal(err)
	}
	return p.Spec.AllowListRefs
}

// testEncryptionKey stands in for the cluster encryption key.
var testEncryptionKey = []byte("0123456789abcdef0123456789abcdef")

// TestRequestNameIsKeyed: the object name, which allow-list entries show
// to blocklists.read holders (viewers) as their request, is an HMAC under
// a key derived from the encryption key; the plain hash of client and
// domain would let a viewer recover the client address by trying the
// addresses of a network.
func TestRequestNameIsKeyed(t *testing.T) {
	f := newFixture(t)
	r := f.submit("192.168.37.201", "games.example", "")
	for a := range 256 {
		if r.Name == Name(nil, fmt.Sprintf("192.168.37.%d", a), "games.example") {
			t.Fatalf("the name %s is the unkeyed hash of the client", r.Name)
		}
	}
	key, _ := NameKey(testEncryptionKey)
	if r.Name != Name(key, "192.168.37.201", "games.example") {
		t.Fatalf("name %s is not the keyed name", r.Name)
	}
	if other, _ := NameKey([]byte("another key, another cluster....")); Name(other, "192.168.37.201", "games.example") == r.Name {
		t.Fatal("the name does not depend on the key")
	}
}

func TestSubmitCreatesAndDedupes(t *testing.T) {
	f := newFixture(t)
	r := f.submit("10.0.20.5", "games.example", "homework")
	key, _ := NameKey(testEncryptionKey)
	if r.Name != Name(key, "10.0.20.5", "games.example") || r.Namespace != "default" || r.EffectiveState() != v1alpha1.UnblockPending || r.Status.State != v1alpha1.UnblockPending {
		t.Fatalf("request %s/%s state %q", r.Namespace, r.Name, r.Status.State)
	}
	if r.Spec.Client != "10.0.20.5" || r.Spec.ClientPrivacy != v1alpha1.UnblockClientFull || r.Spec.Node != "node-a" || r.Spec.Submissions != 1 || store.ManagedBy(r) != Manager {
		t.Fatalf("spec %+v", r.Spec)
	}
	f.clk.Add(time.Minute)
	r2 := f.submit("10.0.20.5", "games.example", "homework, please")
	if r2.Name != r.Name || r2.Spec.Submissions != 2 || r2.Spec.Reason != "homework, please" || !r2.Spec.RequestedAt.After(r.Spec.RequestedAt.Time) {
		t.Fatalf("dedupe: %+v", r2.Spec)
	}
	if c := f.svc.Counts(); c.Pending != 1 {
		t.Fatalf("counts %+v", c)
	}
	if got := f.svc.Lookup(netip.MustParseAddr("10.0.20.5"), "games.example"); got == nil || got.Spec.Submissions != 2 {
		t.Fatalf("lookup %+v", got)
	}
	if f.svc.Lookup(netip.MustParseAddr("10.0.20.6"), "games.example") != nil {
		t.Fatal("another client sees the request")
	}
}

func TestSubmitCap(t *testing.T) {
	f := newFixture(t)
	f.set.MaxPending = 2
	f.submit("10.0.0.1", "a.example", "")
	f.submit("10.0.0.2", "a.example", "")
	if _, err := f.svc.Submit(context.Background(), Submission{Client: netip.MustParseAddr("10.0.0.3"), Domain: "a.example"}); !errors.Is(err, ErrPaused) {
		t.Fatalf("third request: %v, want ErrPaused", err)
	}
	// Updating a pending request is not a new one.
	f.submit("10.0.0.1", "a.example", "again")
}

func TestClientPrivacy(t *testing.T) {
	cases := []struct {
		mode      querylog.Mode
		pseudonym bool
		addr      string
		client    string
		privacy   v1alpha1.UnblockClientPrivacy
	}{
		{querylog.PrivacyFull, false, "192.0.2.77", "192.0.2.77", v1alpha1.UnblockClientFull},
		{querylog.PrivacyHideDomains, false, "192.0.2.77", "192.0.2.77", v1alpha1.UnblockClientFull},
		{querylog.PrivacyAnonymizeClient, false, "192.0.2.77", "192.0.2.0", v1alpha1.UnblockClientPrefix},
		{querylog.PrivacyAnonymizeClient, false, "2001:db8:abcd:12::1", "2001:db8:abcd::", v1alpha1.UnblockClientPrefix},
		{querylog.PrivacyAnonymizeClient, true, "192.0.2.77", "", v1alpha1.UnblockClientHidden},
		{querylog.PrivacyHideDomainsClients, false, "192.0.2.77", "", v1alpha1.UnblockClientHidden},
		{querylog.PrivacyNothing, false, "::ffff:192.0.2.77", "", v1alpha1.UnblockClientHidden},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.mode, f.pseud = c.mode, c.pseudonym
		r := f.submit(c.addr, "x.example", "")
		if r.Spec.Client != c.client || r.Spec.ClientPrivacy != c.privacy {
			t.Errorf("%v/%v %s: client %q %q, want %q %q", c.mode, c.pseudonym, c.addr, r.Spec.Client, r.Spec.ClientPrivacy, c.client, c.privacy)
		}
		if f.svc.Lookup(netip.MustParseAddr(c.addr), "x.example") == nil {
			t.Errorf("%v: the client does not find its own request", c.mode)
		}
	}
}

func TestApproveEveryoneAndExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.submit("10.0.20.5", "games.example", "")
	out, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeEveryone, Match: MatchSubdomains, Duration: time.Hour, Note: "for today", Actor: "user:admin"})
	if err != nil {
		t.Fatal(err)
	}
	a := out.Request.Status.Allow
	if out.Request.Status.State != v1alpha1.UnblockApproved || a == nil || a.List != "default/ui-allow" || a.Entry != "*.games.example" || !a.Added || a.ExpiresAt == nil {
		t.Fatalf("status %+v allow %+v", out.Request.Status, a)
	}
	if !slices.Equal(out.Attached, []string{"default/default", "default/kids"}) {
		t.Fatalf("attached %v", out.Attached)
	}
	if got := f.list(EveryoneList); !slices.Equal(got, []string{"*.games.example"}) {
		t.Fatalf("ui-allow %v", got)
	}
	if e := f.entry(EveryoneList, "*.games.example"); e.Origin != v1alpha1.OriginUnblock || e.Request != "default/"+r.Name || e.CreatedBy != "user:admin" || e.Comment != "for today" {
		t.Fatalf("entry provenance %+v", e)
	}
	if !slices.Contains(f.allowRefs("kids"), EveryoneList) || !slices.Contains(f.allowRefs("default"), EveryoneList) {
		t.Fatal("ui-allow not attached")
	}
	if _, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeEveryone}); !errors.Is(err, ErrState) {
		t.Fatalf("second approve: %v", err)
	}
	if _, err := f.svc.Delete(ctx, "default", r.Name); !errors.Is(err, ErrState) {
		t.Fatalf("delete of a time-limited approval: %v", err)
	}
	// Not due yet.
	f.clk.Add(30 * time.Minute)
	if res, err := f.svc.Sweep(ctx); err != nil || res.Ended != 0 {
		t.Fatalf("early sweep %+v %v", res, err)
	}
	f.clk.Add(31 * time.Minute)
	if res, err := f.svc.Sweep(ctx); err != nil || res.Ended != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	if got := f.list(EveryoneList); got != nil {
		t.Fatalf("ui-allow after expiry %v, want deleted", got)
	}
	if slices.Contains(f.allowRefs("kids"), EveryoneList) {
		t.Fatal("deleted list still attached")
	}
	r = f.svc.Get("default", r.Name)
	if r.Status.State != v1alpha1.UnblockExpired || r.Status.Allow.Removed == nil {
		t.Fatalf("after expiry %+v", r.Status)
	}
	// Asked again: pending, the approval in the history.
	r = f.submit("10.0.20.5", "games.example", "again")
	if r.EffectiveState() != v1alpha1.UnblockPending || len(r.Status.History) != 1 || r.Status.History[0].State != v1alpha1.UnblockExpired {
		t.Fatalf("re-request %+v", r.Status)
	}
}

// TestExpiryKeepsAManualEntry: an operator who replaces a time-limited
// approval's entry with one of their own (same rule) keeps it when the
// approval ends; only entries of origin unblock are released.
func TestExpiryKeepsAManualEntry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.submit("10.0.20.5", "games.example", "")
	if _, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeEveryone, Duration: time.Hour, Actor: "user:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.lists.RemoveRule(ctx, "default", EveryoneList, "games.example", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.lists.Add(ctx, domainlist.AddRequest{Action: domainlist.Allow, Namespace: "default", Type: v1alpha1.EntryExact,
		Values: []string{"games.example"}, Comment: "permanent, ticket 42", Origin: v1alpha1.OriginAPI, Actor: "user:operator"}); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(2 * time.Hour)
	if res, err := f.svc.Sweep(ctx); err != nil || res.Ended != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	if got := f.list(EveryoneList); !slices.Equal(got, []string{"games.example"}) {
		t.Fatalf("ui-allow after the approval ended %v, want the manual entry", got)
	}
}

// racingStore runs hook once, right before the first Approved status
// write: the requester re-posting the block page form while an approval
// is in flight.
type racingStore struct {
	store.Store
	hook func()
}

func (r *racingStore) UpdateStatus(ctx context.Context, obj store.Object) error {
	if u, ok := obj.(*v1alpha1.UnblockRequest); ok && u.Status.State == v1alpha1.UnblockApproved && r.hook != nil {
		h := r.hook
		r.hook = nil
		h()
	}
	return r.Store.UpdateStatus(ctx, obj)
}

// TestApprovalRacingAResubmission: a re-submission between the allow
// entry write and the status write (a resource version conflict) does
// not fail the approval, and the time-limited entry is still removed
// when it ends. Before, the approval failed after writing the entry, and
// its retry found the entry present (added false), so it was never
// removed: the approval became permanent.
func TestApprovalRacingAResubmission(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rs := &racingStore{Store: f.st}
	f.svc.opts.Store = rs
	f.svc.lists = domainlist.New(domainlist.Options{Store: rs, Now: f.clk.Now})
	r := f.submit("10.0.20.5", "games.example", "please")
	rs.hook = func() { f.submit("10.0.20.5", "games.example", "please!!") }
	out, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeEveryone, Duration: time.Hour, Actor: "user:admin"})
	if err != nil {
		t.Fatalf("approve racing a re-submission: %v", err)
	}
	if a := out.Request.Status.Allow; out.Request.Status.State != v1alpha1.UnblockApproved || a == nil || !a.Added || out.Request.Spec.Reason != "please!!" {
		t.Fatalf("approved %+v spec %+v", out.Request.Status, out.Request.Spec)
	}
	f.clk.Add(2 * time.Hour)
	if res, err := f.svc.Sweep(ctx); err != nil || res.Ended != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	if got := f.list(EveryoneList); got != nil {
		t.Fatalf("ui-allow after the 1h approval ended %v", got)
	}
}

// failingStatusStore fails every Approved status write.
type failingStatusStore struct{ store.Store }

func (f failingStatusStore) UpdateStatus(ctx context.Context, obj store.Object) error {
	if u, ok := obj.(*v1alpha1.UnblockRequest); ok && u.Status.State == v1alpha1.UnblockApproved {
		return errors.New("store unavailable")
	}
	return f.Store.UpdateStatus(ctx, obj)
}

// TestFailedApprovalRemovesItsEntry: an approval whose status cannot be
// written takes the entry it added off the list again.
func TestFailedApprovalRemovesItsEntry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.submit("10.0.20.5", "games.example", "")
	fs := failingStatusStore{Store: f.st}
	f.svc.opts.Store = fs
	f.svc.lists = domainlist.New(domainlist.Options{Store: fs, Now: f.clk.Now})
	if _, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeEveryone, Duration: time.Hour}); err == nil {
		t.Fatal("approve succeeded")
	}
	if got := f.list(EveryoneList); got != nil {
		t.Fatalf("ui-allow after a failed approval %v", got)
	}
}

func TestApproveKeepsExistingEntries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// An entry the Domains page added stays after the approval ends.
	if _, err := f.svc.lists.Add(ctx, domainlist.AddRequest{Action: domainlist.Allow, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"games.example"}}); err != nil {
		t.Fatal(err)
	}
	r := f.submit("10.0.20.5", "games.example", "")
	out, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeEveryone, Duration: time.Minute})
	if err != nil || out.Request.Status.Allow.Added {
		t.Fatalf("approve %+v %v", out, err)
	}
	f.clk.Add(2 * time.Minute)
	if _, err := f.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.list(EveryoneList); !slices.Equal(got, []string{"games.example"}) {
		t.Fatalf("ui-allow %v", got)
	}
}

func TestSharedEntryOutlivesFirstApproval(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.submit("10.0.20.5", "games.example", "")
	b := f.submit("10.0.20.6", "games.example", "")
	if _, err := f.svc.Approve(ctx, "default", a.Name, Decision{Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, "default", b.Name, Decision{}); err != nil { // permanent, same entry
		t.Fatal(err)
	}
	f.clk.Add(2 * time.Hour)
	if _, err := f.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.list(EveryoneList); !slices.Equal(got, []string{"games.example"}) {
		t.Fatalf("entry removed although a permanent approval needs it: %v", got)
	}
	if !f.svc.Get("default", b.Name).Status.Allow.Added {
		t.Fatal("the permanent approval did not inherit the entry")
	}
	// Denying the permanent one now removes the entry.
	if _, err := f.svc.Deny(ctx, "default", b.Name, "changed my mind", "user:admin"); err != nil {
		t.Fatal(err)
	}
	if got := f.list(EveryoneList); got != nil {
		t.Fatalf("entry after revoke %v", got)
	}
	r := f.svc.Get("default", b.Name)
	if r.Status.State != v1alpha1.UnblockDenied || r.Status.Note != "changed my mind" || len(r.Status.History) != 1 || r.Status.History[0].Allow.Removed == nil {
		t.Fatalf("denied %+v", r.Status)
	}
}

func TestApproveGroup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.submit("10.0.20.5", "games.example", "")
	out, err := f.svc.Approve(ctx, "default", r.Name, Decision{Scope: ScopeGroup})
	if err != nil {
		t.Fatal(err)
	}
	if a := out.Request.Status.Allow; a.List != "default/ui-allow-kids" || a.Group != "default/kids" || a.Entry != "games.example" {
		t.Fatalf("allow %+v", a)
	}
	if !slices.Equal(out.Attached, []string{"default/kids"}) || slices.Contains(f.allowRefs("default"), "ui-allow-kids") {
		t.Fatalf("attached %v, default refs %v", out.Attached, f.allowRefs("default"))
	}
	if got := f.list("ui-allow-kids"); !slices.Equal(got, []string{"games.example"}) {
		t.Fatalf("ui-allow-kids %v", got)
	}
	// A group no policy covers.
	cg := &v1alpha1.ClientGroup{ObjectMeta: meta("guests"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.30.0/24"}}}}
	if _, err := f.st.Apply(ctx, cg, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	r2 := f.submit("10.0.20.9", "other.example", "")
	if _, err := f.svc.Approve(ctx, "default", r2.Name, Decision{Scope: ScopeGroup, Group: "guests"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uncovered group: %v", err)
	}
	if _, err := f.svc.Approve(ctx, "default", r2.Name, Decision{Scope: "client"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("client scope: %v", err)
	}
}

func TestManagedElsewhere(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A file-managed kids policy is left alone and reported.
	p := &v1alpha1.Policy{}
	if err := f.st.Get(ctx, store.Key{Kind: "Policy", Namespace: "default", Name: "kids"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Apply(ctx, p, store.ApplyOptions{Manager: store.ManagerFile("kids.yaml"), Force: true}); err != nil {
		t.Fatal(err)
	}
	r := f.submit("10.0.20.5", "games.example", "")
	out, err := f.svc.Approve(ctx, "default", r.Name, Decision{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Skipped, []string{"default/kids"}) || slices.Contains(f.allowRefs("kids"), EveryoneList) {
		t.Fatalf("skipped %v", out.Skipped)
	}
}

// TestLegacyApprovalRelease: an approval made before structured entries
// (entry "||name^" as an inline line) is released from the migrated list.
func TestLegacyApprovalRelease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.submit("10.0.20.5", "games.example", "")
	bl := &v1alpha1.BlockList{ObjectMeta: meta(EveryoneList), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{{Name: "ui", Inline: []string{"||games.example^", "keep.example"}}}}}
	if _, err := f.st.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	exp := metav1.NewTime(f.clk.Now().Add(time.Minute))
	r.Status = v1alpha1.UnblockRequestStatus{State: v1alpha1.UnblockApproved, Allow: &v1alpha1.UnblockAllow{
		Scope: ScopeEveryone, Match: MatchSubdomains, List: "default/" + EveryoneList, Entry: "||games.example^", Added: true, ExpiresAt: &exp,
	}}
	if err := f.st.UpdateStatus(ctx, r); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.lists.Migrate(ctx); err != nil || n != 1 {
		t.Fatalf("migrate %d %v", n, err)
	}
	if e := f.entry(EveryoneList, "*.games.example"); e.Origin != v1alpha1.OriginUnblock || e.Request != "default/"+r.Name {
		t.Fatalf("migrated entry %+v", e)
	}
	f.clk.Add(2 * time.Minute)
	if res, err := f.svc.Sweep(ctx); err != nil || res.Ended != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	if got := f.list(EveryoneList); !slices.Equal(got, []string{"keep.example"}) {
		t.Fatalf("ui-allow after the release %v", got)
	}
}

func TestSweepExpiresAndPrunes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pending := f.submit("10.0.0.1", "a.example", "")
	denied := f.submit("10.0.0.2", "b.example", "")
	if _, err := f.svc.Deny(ctx, "default", denied.Name, "", "user:admin"); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(f.set.PendingExpiry.Std())
	res, err := f.svc.Sweep(ctx)
	if err != nil || res.Expired != 1 || res.Pruned != 0 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	if r := f.svc.Get("default", pending.Name); r.Status.State != v1alpha1.UnblockExpired || r.Status.DecidedBy != SystemActor {
		t.Fatalf("expired %+v", r.Status)
	}
	f.clk.Add(f.set.Retention.Std() - f.set.PendingExpiry.Std())
	if res, err := f.svc.Sweep(ctx); err != nil || res.Pruned != 1 {
		t.Fatalf("prune denied %+v %v", res, err)
	}
	f.clk.Add(f.set.PendingExpiry.Std())
	if res, err := f.svc.Sweep(ctx); err != nil || res.Pruned != 1 {
		t.Fatalf("prune expired %+v %v", res, err)
	}
	if c := f.svc.Counts(); c != (Counts{}) {
		t.Fatalf("counts %+v", c)
	}
}

func TestRunIndexesTheStore(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A request written by another node.
	other := New(Options{Store: f.st, Node: "node-b", Now: f.clk.Now})
	r, err := other.Submit(ctx, Submission{Client: netip.MustParseAddr("10.0.0.9"), Domain: "c.example"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.svc.Run(ctx) }()
	select {
	case <-f.svc.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("not synced")
	}
	if got := f.svc.Get("default", r.Name); got == nil || got.Spec.Node != "node-b" {
		t.Fatalf("indexed %+v", got)
	}
	if _, err := other.Delete(ctx, "default", r.Name); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.svc.Get("default", r.Name) != nil {
		if time.Now().After(deadline) {
			t.Fatal("deletion not indexed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
