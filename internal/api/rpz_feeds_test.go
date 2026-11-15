package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// rpzFeedPrimary is a real TCP transfer primary for policy.test. Failing makes
// it answer SERVFAIL to every query.
type rpzFeedPrimary struct {
	address   string
	failing   atomic.Bool
	transfers atomic.Int64
}

func startRPZFeedPrimary(t *testing.T) *rpzFeedPrimary {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &rpzFeedPrimary{address: listener.Addr().String()}
	lines := []string{
		"policy.test. 300 IN NS ns.policy.test.",
		"nx.test.policy.test. 300 IN CNAME .",
		"*.wild.test.policy.test. 300 IN CNAME *.",
		"tcp.test.policy.test. 300 IN CNAME rpz-tcp-only.",
		"32.2.0.0.127.rpz-client-ip.policy.test. 300 IN CNAME rpz-drop.",
		"24.0.2.0.192.rpz-ip.policy.test. 300 IN CNAME *.",
		"ns.example.rpz-nsdname.policy.test. 300 IN CNAME .",
		"999.rpz-ip.policy.test. 300 IN CNAME .",
	}
	started := make(chan struct{})
	server := &dns.Server{Listener: listener, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		a := new(dns.Msg)
		a.SetReply(q)
		a.Authoritative = true
		if p.failing.Load() || len(q.Question) != 1 || q.Question[0].Name != "policy.test." {
			a.Rcode = dns.RcodeServerFailure
			_ = w.WriteMsg(a)
			return
		}
		soa := &dns.SOA{Hdr: dns.RR_Header{Name: "policy.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300}, Ns: "ns.policy.test.", Mbox: "hostmaster.policy.test.", Serial: 2026111201, Refresh: 60, Retry: 10, Expire: 600, Minttl: 5}
		a.Answer = []dns.RR{soa}
		switch q.Question[0].Qtype {
		case dns.TypeSOA:
		case dns.TypeAXFR, dns.TypeIXFR:
			p.transfers.Add(1)
			for _, line := range lines {
				rr, err := dns.NewRR(line)
				if err != nil {
					panic(err)
				}
				a.Answer = append(a.Answer, rr)
			}
			a.Answer = append(a.Answer, dns.Copy(soa))
		default:
			a.Rcode = dns.RcodeRefused
		}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("RPZ primary did not start")
	}
	return p
}

type rpzFeedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *rpzFeedClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *rpzFeedClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func rpzFeedList(name, primary, key string) *public.BlockList {
	return &public.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}, Spec: public.BlockListSpec{Mode: "block",
		Sources: []public.ListSource{{Name: "feed", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{primary}, TSIGKeyRef: key}}}}}
}

// TestRPZFeedRoutesOverRealTransfer acquires a feed from a real TCP primary
// and reads every status field through the routes, then the failed-refresh,
// expired, unreachable and misconfigured states, and the diagnoses (D391).
func TestRPZFeedRoutesOverRealTransfer(t *testing.T) {
	primary := startRPZFeedPrimary(t)
	h := newHarness(t)
	for _, list := range []*public.BlockList{
		rpzFeedList("feed", primary.address, ""),
		rpzFeedList("unreachable", unusedTCPAddress(t), ""),
		rpzFeedList("unkeyed", primary.address, "absent-key."),
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "plain"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"ads.example"}}}}},
	} {
		if _, err := h.st.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := zonecache.New(h.st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = cache.Start(ctx) }()
	<-cache.Initialized()
	db, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	clock := &rpzFeedClock{now: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	manager, err := rpz.New(rpz.Options{Cache: cache, Blobs: db, ClusterID: "cluster", Keyring: keys, Receiver: &secondary.TransferClient{Keyring: keys},
		Now: clock.Now, PollInterval: 5 * time.Millisecond, Timeout: 2 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.opts.RPZ = manager

	get := func(path, who string) []RPZFeed {
		t.Helper()
		var out List[RPZFeed]
		h.do(http.MethodGet, path, who, nil).want(http.StatusOK).json(&out)
		return out.Items
	}
	one := func(name string) RPZFeed {
		t.Helper()
		items := get("/api/v1/blocklists/default/"+name+"/rpz", "viewer")
		if len(items) != 1 {
			t.Fatalf("%s feeds = %+v", name, items)
		}
		return items[0]
	}
	read := func() []DiagnosisMessage {
		t.Helper()
		var out List[DiagnosisMessage]
		h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&out)
		var rpzOnly []DiagnosisMessage
		for _, m := range out.Items {
			if m.Source == "rpz" {
				rpzOnly = append(rpzOnly, m)
			}
		}
		return rpzOnly
	}
	// messages reads the plain path, then the path SQLite nodes mount
	// (InfoMessageReads set, so /info/messages and kudurru doctor reach the
	// feed diagnoses through infoOperationalMessages), and returns both.
	// Callers require each diagnosis on both paths (see has).
	messages := func() [2][]DiagnosisMessage {
		t.Helper()
		plain := read()
		h.srv.opts.InfoMessageReads = infoReadFunc(func(context.Context, time.Time, func(string, string, string, string, string, string) error) error {
			return nil
		})
		defer func() { h.srv.opts.InfoMessageReads = nil }()
		return [2][]DiagnosisMessage{plain, read()}
	}
	has := func(paths [2][]DiagnosisMessage, severity, reason, object, text string) bool {
		for _, ms := range paths {
			found := false
			for _, m := range ms {
				if m.Severity == severity && m.Reason == reason && m.Object == object && strings.Contains(m.Message, text) && m.Node == "node-a" {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	hasAny := func(paths [2][]DiagnosisMessage, severity, object string) bool {
		for _, ms := range paths {
			for _, m := range ms {
				if m.Severity == severity && m.Object == object {
					return true
				}
			}
		}
		return false
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Before the manager runs: pending, with no copy fields.
	if f := one("feed"); f.State != "pending" || f.Available || f.Reason != "TransferPending" || f.Serial != nil || f.Counts != nil || f.Rules != nil || f.ExpiresAt != nil || f.Ignored == nil {
		t.Fatalf("pending feed = %+v", f)
	}
	if !has(messages(), "info", "TransferPending", "BlockList/default/feed", "SERVFAIL") {
		t.Fatal("pending feed has no info diagnosis")
	}
	h.do(http.MethodGet, "/api/v1/blocklists/default/absent/rpz", "viewer", nil).want(http.StatusNotFound)
	if items := get("/api/v1/blocklists/default/plain/rpz", "viewer"); len(items) != 0 {
		t.Fatalf("a list without RPZ sources reported feeds: %+v", items)
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_rpz", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/blocklists/default/feed/rpz", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/blocklists/_rpz", "", nil).want(http.StatusUnauthorized)
	h.do(http.MethodGet, "/api/v1/blocklists/default/feed/rpz", "", nil).want(http.StatusUnauthorized)

	runCtx, stopRun := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = manager.Run(runCtx) }()
	stop := func() { stopRun(); <-done }
	defer stop()

	await("feed acquired", func() bool { return one("feed").State == "serving" })
	f := one("feed")
	start := clock.Now()
	if f.Node != "node-a" || f.Namespace != "default" || f.List != "feed" || f.Source != "feed" || f.Origin != "policy.test." || !f.Available ||
		f.Serial == nil || *f.Serial != 2026111201 || f.Primary != primary.address || f.LastConfirmedAt == nil || !f.LastConfirmedAt.Equal(start) ||
		f.ExpiresAt == nil || !f.ExpiresAt.Equal(start.Add(600*time.Second)) || f.NextAttemptAt == nil || !f.NextAttemptAt.Equal(start.Add(60*time.Second)) ||
		f.LastError != "" || f.ConfigurationError != "" || f.Reason != "" || f.Message != "" {
		t.Fatalf("serving feed = %+v", f)
	}
	if c := f.Counts; c == nil || *c != (RPZFeedCounts{Records: 9, Rules: 5, Invalid: 1, Unsupported: 1, IgnoredRecords: 2}) {
		t.Fatalf("counts = %+v", c)
	}
	if r := f.Rules; r == nil || *r != (RPZRuleCounts{QName: 3, ClientIP: 1, ResponseIP: 1, NXDOMAIN: 1, NODATA: 2, Drop: 1, TCPOnly: 1}) {
		t.Fatalf("rule counts = %+v", r)
	}
	if len(f.Ignored) != 2 || f.Ignored[0].Reason != "invalid-address-trigger" || f.Ignored[0].Unsupported || f.Ignored[0].Count != 1 || f.Ignored[0].Examples[0] != "999.rpz-ip.policy.test." ||
		f.Ignored[1].Reason != "resolver-ns-provenance" || !f.Ignored[1].Unsupported || f.Ignored[1].Count != 1 || f.Ignored[1].Examples[0] != "ns.example.rpz-nsdname.policy.test." {
		t.Fatalf("ignored = %+v", f.Ignored)
	}
	all := get("/api/v1/blocklists/_rpz", "viewer")
	if len(all) != 3 || all[0].List != "feed" || all[1].List != "unkeyed" || all[2].List != "unreachable" {
		t.Fatalf("node feeds = %+v", all)
	}
	if items := get("/api/v1/blocklists/_rpz?namespace=other", "viewer"); len(items) != 0 {
		t.Fatalf("namespace filter = %+v", items)
	}
	if u := one("unkeyed"); u.State != "unavailable" || u.Reason != "ConfigurationUnavailable" || u.ConfigurationError == "" || u.Available || u.Serial != nil {
		t.Fatalf("misconfigured feed = %+v", u)
	}
	await("unreachable feed reports its transfer error", func() bool { return one("unreachable").State == "unavailable" })
	if u := one("unreachable"); u.Reason != "RPZTransfer" || u.LastError == "" || u.Available || u.Serial != nil || !strings.Contains(u.Message, "SERVFAIL") {
		t.Fatalf("unreachable feed = %+v", u)
	}
	ms := messages()
	if !has(ms, "info", "RPZRulesIgnored", "BlockList/default/feed", "1 unsupported and 1 invalid") ||
		!has(ms, "error", "RPZTransfer", "BlockList/default/unreachable", "policy.test.") ||
		!has(ms, "error", "ConfigurationUnavailable", "BlockList/default/unkeyed", "policy.test.") ||
		hasAny(ms, "error", "BlockList/default/feed") {
		t.Fatalf("diagnoses = %+v", ms)
	}

	// A failed refresh keeps the copy serving with the error reported.
	primary.failing.Store(true)
	clock.add(61 * time.Second)
	manager.Wake()
	await("failed refresh reported", func() bool { return one("feed").LastError != "" })
	if f := one("feed"); f.State != "serving" || !f.Available || f.Reason != "RPZTransfer" || !f.LastConfirmedAt.Equal(start) {
		t.Fatalf("serving feed after a failed refresh = %+v", f)
	}
	if !has(messages(), "warning", "RPZTransfer", "BlockList/default/feed", "keeps applying") {
		t.Fatal("failed refresh has no warning diagnosis")
	}

	// Past the SOA expire the copy is expired; its counts are kept.
	stop()
	clock.add(600 * time.Second)
	if f := one("feed"); f.State != "expired" || f.Available || f.Reason != "RPZExpired" || f.Serial == nil || f.Counts == nil || !strings.Contains(f.Message, "Last refresh error") {
		t.Fatalf("expired feed = %+v", f)
	}
	if !has(messages(), "error", "RPZExpired", "BlockList/default/feed", "SERVFAIL") {
		t.Fatal("expired feed has no error diagnosis")
	}

	// A clock stepped back before the last confirmation leaves the copy
	// unusable but not expired: it is unavailable, never "expired" with an
	// expiry still in the future. The copy fields stay visible.
	clock.add(-(600 + 61 + 1) * time.Second)
	if f := one("feed"); f.State != "unavailable" || f.Available || f.Reason != "RPZTransfer" || f.Serial == nil || f.Counts == nil ||
		f.ExpiresAt == nil || !f.ExpiresAt.After(clock.Now()) || !strings.Contains(f.Message, "SERVFAIL") || strings.Contains(f.Message, "expired") {
		t.Fatalf("unusable unexpired feed = %+v", f)
	}
	if !has(messages(), "error", "RPZTransfer", "BlockList/default/feed", "no usable copy") {
		t.Fatal("unusable unexpired feed has no error diagnosis")
	}
	if primary.transfers.Load() == 0 {
		t.Fatal("no real transfer happened")
	}
}

func TestRPZFeedRoutesWithoutManager(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodGet, "/api/v1/blocklists/_rpz", "viewer", nil).want(http.StatusNotImplemented)
	h.do(http.MethodGet, "/api/v1/blocklists/default/feed/rpz", "viewer", nil).want(http.StatusNotImplemented)
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

type rpzFeedSourceFunc func(namespace, name string) ([]rpz.Status, error)

func (f rpzFeedSourceFunc) Statuses() []rpz.Status { return nil }
func (f rpzFeedSourceFunc) ListStatuses(namespace, name string) ([]rpz.Status, error) {
	return f(namespace, name)
}

// TestRPZFeedRouteBeforeConfiguration answers 503 while the node has no
// configuration snapshot, so a missing list is not reported as absent.
func TestRPZFeedRouteBeforeConfiguration(t *testing.T) {
	h := newHarness(t)
	h.srv.opts.RPZ = rpzFeedSourceFunc(func(string, string) ([]rpz.Status, error) { return nil, rpz.ErrConfigurationUnavailable })
	h.do(http.MethodGet, "/api/v1/blocklists/default/feed/rpz", "viewer", nil).want(http.StatusServiceUnavailable)
	var out List[RPZFeed]
	h.do(http.MethodGet, "/api/v1/blocklists/_rpz", "viewer", nil).want(http.StatusOK).json(&out)
	if out.Items == nil || len(out.Items) != 0 {
		t.Fatalf("feeds before configuration = %+v", out.Items)
	}
}
