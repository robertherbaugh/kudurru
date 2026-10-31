package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/bootstrap"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func licenseManager(t *testing.T, nodes int) *license.Manager {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := license.New(license.Options{Keyring: license.Keyring{"t": pub}})
	if err != nil {
		t.Fatal(err)
	}
	if nodes == 0 {
		return m
	}
	key, err := license.Sign(priv, "t", license.Claims{
		ID: "lic_t", Licensee: "tests", Plan: license.PlanPersonal, Nodes: nodes,
		IssuedAt: time.Now().Add(-time.Hour).Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set(key, "test"); err != nil {
		t.Fatal(err)
	}
	return m
}

func peers(n int) staticMembership {
	p := []cluster.Peer{{Name: "node-a", Self: true}}
	for i := 1; i < n; i++ {
		p = append(p, cluster.Peer{Name: "node-" + string(rune('a'+i)), Address: "10.0.0.2:8082"})
	}
	return staticMembership{peers: p}
}

func TestLicenseGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   int // nodes in the key; 0 is no key
		cluster int
		blocked bool
	}{
		{"community, one node", 0, 1, false},
		{"community, two nodes", 0, 2, true},
		{"personal key, three nodes", 3, 3, false},
		{"personal key, four nodes", 3, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.License = licenseManager(t, tc.nodes)
				o.Membership = peers(tc.cluster)
			})
			// A configuration write is refused only over the limit.
			r := h.do(http.MethodDelete, "/api/v1/zones/nonexistent", "admin", nil)
			if tc.blocked {
				r.want(http.StatusPaymentRequired)
				if r.errKey() != KeyLicense {
					t.Errorf("error key = %q, want %q", r.errKey(), KeyLicense)
				}
			} else if r.Code == http.StatusPaymentRequired {
				t.Fatalf("write refused: %s", r.Body.String())
			}
			// Reads, the sign-in and the operational controls are never refused.
			h.do(http.MethodGet, "/api/v1/zones", "admin", nil).want(http.StatusOK)
			for _, p := range []string{"/api/v1/auth/login", "/api/v1/dns/blocking", "/api/v1/cache/flush", "/api/v1/dns/query"} {
				if r := h.do(http.MethodPost, p, "admin", map[string]any{}); r.Code == http.StatusPaymentRequired {
					t.Errorf("POST %s refused over the limit: %s", p, r.Body.String())
				}
			}
			// The state is always readable.
			var info LicenseInfo
			h.do(http.MethodGet, "/api/v1/license", "viewer", nil).want(http.StatusOK).json(&info)
			if info.NodesInUse != tc.cluster || info.OverLimit != tc.blocked {
				t.Errorf("license info = %+v", info)
			}
		})
	}
}

func TestLicenseInfoStates(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.License = licenseManager(t, 0) })
	var info LicenseInfo
	h.do(http.MethodGet, "/api/v1/license", "viewer", nil).want(http.StatusOK).json(&info)
	if info.State != license.StateCommunity || info.Nodes != license.CommunityNodes || len(info.Features) != 0 {
		t.Errorf("community info = %+v", info)
	}
	h.do(http.MethodGet, "/api/v1/license", "", nil).want(http.StatusUnauthorized)

	h = newHarness(t, func(o *Options) { o.License = licenseManager(t, 3) })
	h.do(http.MethodGet, "/api/v1/license", "viewer", nil).want(http.StatusOK).json(&info)
	if info.State != license.StateActive || info.Plan != license.PlanPersonal || info.Nodes != 3 || len(info.Features) != len(license.Features())-1 || info.Allows(license.FeatureBranding) {
		t.Errorf("personal info = %+v", info)
	}
}

func TestLicenseGateFailsOpenWithoutMembership(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.License = licenseManager(t, 0)
		o.Membership = failingMembership{}
	})
	if r := h.do(http.MethodDelete, "/api/v1/zones/nonexistent", "admin", nil); r.Code == http.StatusPaymentRequired {
		t.Fatalf("a membership outage blocked a write: %s", r.Body.String())
	}
}

type failingMembership struct{}

func (failingMembership) Leader(context.Context) (cluster.Peer, error) {
	return cluster.Peer{}, cluster.ErrNoLeader
}
func (failingMembership) Peers(context.Context) ([]cluster.Peer, error) {
	return nil, errors.New("membership down")
}

func policyDoc(name string) map[string]any {
	return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "Policy", "metadata": map[string]any{"name": name}, "spec": map[string]any{}}
}

// Features without a key answer 402; with a key they do not. The checks run
// after authorization, so an unauthenticated caller still gets 401.
func TestLicensedFeaturesAnswer402(t *testing.T) {
	routes := []struct{ name, method, path string }{
		{"audit trail", http.MethodGet, "/api/v1/audit"},
		{"audit export", http.MethodGet, "/api/v1/audit/export"},
		{"audit verification", http.MethodGet, "/api/v1/audit/verify"},
		{"long-term statistics", http.MethodGet, "/api/v1/stats/summary?window=7d"},
		{"long-term range", http.MethodGet, "/api/v1/stats/timeseries?from=2026-09-01T00:00:00Z&to=2026-09-02T00:00:00Z"},
		{"unblock requests", http.MethodGet, "/api/v1/unblockrequests"},
	}
	w, tail := querylogFixture(t)
	free := newHarness(t, func(o *Options) { o.License = licenseManager(t, 0); o.Querylog = w.Reader(); o.Tail = tail })
	keyed := newHarness(t, func(o *Options) { o.License = licenseManager(t, 3); o.Querylog = w.Reader(); o.Tail = tail })
	for _, rt := range routes {
		r := free.do(rt.method, rt.path, "admin", nil)
		if r.Code != http.StatusPaymentRequired || r.errKey() != KeyLicense {
			t.Errorf("%s without a key: status %d key %q: %s", rt.name, r.Code, r.errKey(), r.Body.String())
		}
		if r := keyed.do(rt.method, rt.path, "admin", nil); r.Code == http.StatusPaymentRequired {
			t.Errorf("%s with a key: %s", rt.name, r.Body.String())
		}
		free.do(rt.method, rt.path, "", nil).want(http.StatusUnauthorized)
	}
	// The short windows stay free.
	for _, p := range []string{"/api/v1/stats/summary", "/api/v1/stats/summary?window=1h", "/api/v1/stats/summary?window=24h"} {
		if r := free.do(http.MethodGet, p, "admin", nil); r.Code == http.StatusPaymentRequired {
			t.Errorf("GET %s refused without a key: %s", p, r.Body.String())
		}
	}
	// Without a License option nothing is gated (tests, partial wirings).
	open := newHarness(t)
	if r := open.do(http.MethodGet, "/api/v1/audit", "admin", nil); r.Code == http.StatusPaymentRequired {
		t.Errorf("an ungated server answered 402")
	}
}

// seedDefaults writes the first-boot objects, Policy "default" among them, the
// way a node's first start does: straight to the store, past the API's gate.
func seedDefaults(t *testing.T, st store.Store) {
	t.Helper()
	res, err := bootstrap.Seed(context.Background(), st, bootstrap.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil || !res.FirstBoot {
		t.Fatalf("seed: %+v, %v", res, err)
	}
}

func createPolicy(h *harness, name string) response {
	return h.do(http.MethodPut, "/api/v1/objects/policies/default/"+name, "admin", policyDoc(name))
}

// Without a key the seeded Policy "default" is the one policy there may be:
// the API refuses every other create, and the default stays editable.
func TestPolicyLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.License = licenseManager(t, 0) })
	seedDefaults(t, h.st)
	r := createPolicy(h, "kids")
	r.want(http.StatusPaymentRequired)
	if body := r.Body.String(); r.errKey() != KeyLicense || !strings.Contains(body, "only the policy default/default can be created") || !strings.Contains(body, "only the default policy") {
		t.Errorf("refusal of another policy: %s", body)
	}
	// The default is not a create: it stays editable.
	createPolicy(h, "default").want(http.StatusOK)
	// Without a key only the default policy can be created, never another
	// name: with the default deleted, "kids" is still refused, the default
	// can be made again, and then nothing else.
	h.do(http.MethodDelete, "/api/v1/objects/policies/default/default", "admin", nil).want(http.StatusNoContent)
	createPolicy(h, "kids").want(http.StatusPaymentRequired)
	createPolicy(h, "default").want(http.StatusCreated)
	createPolicy(h, "guests").want(http.StatusPaymentRequired)
	// The name alone is not enough: default in another namespace is refused.
	other := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "Policy",
		"metadata": map[string]any{"name": "default", "namespace": "other"}, "spec": map[string]any{"clientGroupRefs": []any{"default"}}}
	h.do(http.MethodPut, "/api/v1/objects/policies/other/default", "admin", other).want(http.StatusPaymentRequired)
	// A key lifts the limit, whatever the names.
	k := newHarness(t, func(o *Options) { o.License = licenseManager(t, 3) })
	seedDefaults(t, k.st)
	for _, n := range []string{"a", "b", "c"} {
		createPolicy(k, n).want(http.StatusCreated)
	}
}

// countingMembership counts Peers calls, answers slowly, and can fail.
type countingMembership struct {
	calls atomic.Int32
	delay time.Duration
	err   error
	peers []cluster.Peer
}

func (m *countingMembership) Leader(context.Context) (cluster.Peer, error) {
	return cluster.Peer{}, cluster.ErrNoLeader
}

func (m *countingMembership) Peers(context.Context) ([]cluster.Peer, error) {
	m.calls.Add(1)
	time.Sleep(m.delay)
	return m.peers, m.err
}

// Concurrent writes share one membership call instead of queueing behind one
// each, and a failed lookup is remembered, so a dead membership source costs
// the writes behind it nothing.
func TestNodeCountSharesTheLookupAndCachesFailure(t *testing.T) {
	m := &countingMembership{delay: 150 * time.Millisecond, peers: peers(2).peers}
	h := newHarness(t, func(o *Options) { o.License = licenseManager(t, 0); o.Membership = m })

	var wg sync.WaitGroup
	start := time.Now()
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n := h.srv.nodeCount(context.Background()); n != 2 {
				t.Errorf("nodeCount = %d, want 2", n)
			}
		}()
	}
	wg.Wait()
	if got := m.calls.Load(); got != 1 {
		t.Errorf("20 concurrent lookups made %d membership calls, want 1", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("20 concurrent lookups took %v: they queued", d)
	}

	// A failure is cached: the gate fails open without asking again.
	f := &countingMembership{delay: 50 * time.Millisecond, err: errors.New("membership down")}
	h = newHarness(t, func(o *Options) { o.License = licenseManager(t, 0); o.Membership = f })
	for range 5 {
		if n := h.srv.nodeCount(context.Background()); n != 0 {
			t.Fatalf("nodeCount on a failing membership = %d, want 0 (fail open)", n)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("5 lookups against a dead membership made %d calls, want 1", got)
	}

	// The lookup does not depend on the first caller: a canceled context
	// there does not fail the callers that share its call.
	m2 := &countingMembership{delay: 100 * time.Millisecond, peers: peers(3).peers}
	h = newHarness(t, func(o *Options) { o.License = licenseManager(t, 0); o.Membership = m2 })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if n := h.srv.nodeCount(ctx); n != 3 {
		t.Errorf("nodeCount with a canceled caller = %d, want 3", n)
	}
}

// slowList returns the policy count late, so that a count taken while another
// create is under way is stale by the time it is used.
type slowList struct{ store.Store }

func (s slowList) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	l, err := s.Store.List(ctx, kind, opts)
	time.Sleep(20 * time.Millisecond)
	return l, err
}

// Creates that race at the limit cannot both pass: the check and the create
// are one step. With the seeded default in place every racer is refused;
// with it deleted exactly one wins the free slot.
func TestPolicyLimitHoldsUnderConcurrentCreates(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.License = licenseManager(t, 0); o.Store = slowList{o.Store} })
	seedDefaults(t, h.st)

	race := func(round string) (created, refused int32) {
		var c, r atomic.Int32
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch resp := createPolicy(h, fmt.Sprintf("%s-%d", round, i)); resp.Code {
				case http.StatusCreated:
					c.Add(1)
				case http.StatusPaymentRequired:
					r.Add(1)
				default:
					t.Errorf("PUT %s-%d: %d %s", round, i, resp.Code, resp.Body.String())
				}
			}()
		}
		wg.Wait()
		return c.Load(), r.Load()
	}
	policies := func() []any {
		list := h.do(http.MethodGet, "/api/v1/objects/policies", "admin", nil).want(http.StatusOK).m()
		items, _ := list["items"].([]any)
		return items
	}

	if c, r := race("seeded"); c != 0 || r != 12 {
		t.Errorf("12 racing creates beside the seeded default: %d created, %d refused; want 0 and 12", c, r)
	}
	if n := len(policies()); n != 1 {
		t.Errorf("%d policies exist, want only the seeded default", n)
	}

	// With the default deleted only the default may be created again, so
	// racers with other names are all refused: the name rule, not the race,
	// keeps the store at one policy.
	h.do(http.MethodDelete, "/api/v1/objects/policies/default/default", "admin", nil).want(http.StatusNoContent)
	if c, r := race("racer"); c != 0 || r != 12 {
		t.Errorf("12 racing creates of other names with no policy: %d created, %d refused; want 0 and 12", c, r)
	}
	if n := len(policies()); n != 0 {
		t.Errorf("%d policies exist, want none", n)
	}
}

// In Kubernetes the seeded defaults live in the release namespace (#40), so
// that is where the one policy without a key is: "default" in the server's
// default namespace, and not "default/default".
func TestPolicyLimitFollowsTheDefaultNamespace(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.License = licenseManager(t, 0); o.DefaultNamespace = "dns" })
	if res, err := bootstrap.Seed(context.Background(), h.st, bootstrap.Options{Namespace: "dns", Logger: slog.New(slog.DiscardHandler)}); err != nil || !res.FirstBoot {
		t.Fatalf("seed: %+v, %v", res, err)
	}
	put := func(ns, name string) response {
		return h.do(http.MethodPut, "/api/v1/objects/policies/"+ns+"/"+name, "admin", policyDoc(name))
	}
	r := put("dns", "kids")
	r.want(http.StatusPaymentRequired)
	if !strings.Contains(r.Body.String(), "only the policy dns/default can be created") {
		t.Errorf("refusal names the wrong default: %s", r.Body.String())
	}
	put("dns", "default").want(http.StatusOK) // the seeded default stays editable
	h.do(http.MethodDelete, "/api/v1/objects/policies/dns/default", "admin", nil).want(http.StatusNoContent)
	put("default", "default").want(http.StatusPaymentRequired) // the old default namespace is not the default now
	put("dns", "kids").want(http.StatusPaymentRequired)
	put("dns", "default").want(http.StatusCreated) // the default can be made again, under its own name
}
