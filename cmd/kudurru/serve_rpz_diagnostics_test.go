package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestRunServeSQLiteRPZFeedDiagnostics mounts runServe on SQLite with a real
// unsigned RPZ primary and reads the feed through the real CLI, the API
// route and /metrics (D391): a serving copy with its serial, counts and
// ignored rules, then, with the primary failing, the expired copy with its
// last error while DNS answers SERVFAIL, each also on /info/messages and
// in kudurru doctor (the captured diagnosis read SQLite mounts).
func TestRunServeSQLiteRPZFeedDiagnostics(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.Blocking.Enabled = true
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	primary := rpzDiagnosticsPrimary(t)
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "feed", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block",
			Sources: []public.ListSource{{Name: "native", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{primary.address}}}}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"feed"}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	defer stop()
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeNameError, true)
	cli := mountedCLI(t, cfg, "rpz")

	feeds := func(args ...string) []api.RPZFeed {
		t.Helper()
		out, stderr, err := cli.run(append([]string{"lists", "rpz", "-o", "json"}, args...)...)
		if err != nil {
			t.Fatalf("lists rpz: %v\n%s", err, stderr)
		}
		var items []api.RPZFeed
		if err := json.Unmarshal([]byte(out), &items); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		return items
	}
	// /info/messages on SQLite is the captured read (InfoMessageReads), which
	// is also what kudurru doctor reports.
	diagnosis := func(severity, reason, text string) bool {
		t.Helper()
		out, stderr, err := cli.run("api", "/info/messages")
		if err != nil {
			t.Fatalf("info messages: %v\n%s", err, stderr)
		}
		var list struct {
			Items []api.DiagnosisMessage `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		for _, m := range list.Items {
			if m.Source == "rpz" && m.Severity == severity && m.Reason == reason && m.Object == "BlockList/default/feed" && strings.Contains(m.Message, text) && m.Node == cfg.Node.Name {
				return true
			}
		}
		return false
	}
	// doctor reads the server through the CLI context; its local config only
	// needs to parse.
	doctorConfig := filepath.Join(t.TempDir(), "doctor.yaml")
	if err := os.WriteFile(doctorConfig, []byte("node:\n  name: doctor\n  dataDir: "+t.TempDir()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doctor := func() string {
		t.Helper()
		out, _, _ := cli.run("--config", doctorConfig, "doctor", "--skip-ports")
		return out
	}
	items := feeds("feed")
	if len(items) != 1 {
		t.Fatalf("feeds = %+v", items)
	}
	f := items[0]
	if f.Node != cfg.Node.Name || f.Namespace != "default" || f.List != "feed" || f.Source != "native" || f.Origin != "policy.test." || f.State != "serving" || !f.Available ||
		f.Serial == nil || *f.Serial != 7 || f.Primary != primary.address || f.LastConfirmedAt == nil || f.ExpiresAt == nil || !f.ExpiresAt.Equal(f.LastConfirmedAt.Add(6*time.Second)) ||
		f.Counts == nil || f.Counts.Rules != 1 || f.Counts.Unsupported != 1 || f.Rules == nil || f.Rules.QName != 1 || f.Rules.NXDOMAIN != 1 ||
		len(f.Ignored) != 1 || f.Ignored[0].Reason != "resolver-ns-provenance" || f.Ignored[0].Examples[0] != "ns.example.rpz-nsdname.policy.test." {
		t.Fatalf("serving feed = %+v", f)
	}
	if all := feeds("-A"); len(all) != 1 || all[0].List != "feed" {
		t.Fatalf("node feeds = %+v", all)
	}
	table, _, err := cli.run("lists", "rpz")
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if err != nil || len(lines) != 2 || strings.Join(strings.Fields(lines[1])[:5], " ") != "feed native policy.test. serving 7" {
		t.Fatalf("table = %q, %v", table, err)
	}
	if !kubeRPZMetric(cfg, `kudurru_rpz_feed_available{list="default/feed",source="native"} 1`) ||
		!kubeRPZMetric(cfg, `kudurru_rpz_feed_ignored_rules{list="default/feed",reason="resolver-ns-provenance",source="native"} 1`) {
		t.Fatal("feed metrics missing")
	}
	if !diagnosis("info", "RPZRulesIgnored", "1 unsupported and 0 invalid") || diagnosis("error", "RPZExpired", "") {
		t.Fatal("serving feed diagnoses missing on /info/messages")
	}
	if out := doctor(); !strings.Contains(out, "  INFO RPZRulesIgnored: RPZ feed policy.test. (source native): 1 unsupported and 0 invalid policy owners are ignored (resolver-ns-provenance). [blocklist/default/feed]") {
		t.Fatalf("doctor lacks the ignored-rules diagnosis:\n%s", out)
	}

	// The primary fails: the copy expires 6 s after its last confirmation.
	primary.failing.Store(true)
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeServerFailure, false)
	deadline := time.Now().Add(30 * time.Second)
	for {
		items = feeds("feed")
		if len(items) == 1 && items[0].State == "expired" && items[0].LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed never reported expiry: %+v", items)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f := items[0]; f.Available || f.Reason != "RPZExpired" || f.Serial == nil || *f.Serial != 7 || f.Counts == nil || !strings.Contains(f.Message, "SERVFAIL") {
		t.Fatalf("expired feed = %+v", f)
	}
	if !kubeRPZMetric(cfg, `kudurru_rpz_feed_available{list="default/feed",source="native"} 0`) {
		t.Fatal("expired feed metric missing")
	}
	if !diagnosis("error", "RPZExpired", "SERVFAIL") || !diagnosis("info", "RPZRulesIgnored", "resolver-ns-provenance") {
		t.Fatal("expired feed diagnoses missing on /info/messages")
	}
	if out := doctor(); !strings.Contains(out, "  ERROR RPZExpired: RPZ feed policy.test. (source native): The last copy has expired") || !strings.Contains(out, "diagnoses: FAIL") {
		t.Fatalf("doctor lacks the expired diagnosis:\n%s", out)
	}
	if primary.transfers.Load() == 0 {
		t.Fatal("no real transfer")
	}
}

type rpzDiagnosticsFeed struct {
	address   string
	failing   atomic.Bool
	transfers atomic.Int64
}

// rpzDiagnosticsPrimary serves policy.test. (serial 7, expire 6 s) over TCP
// with one QNAME rule and one unsupported NSDNAME owner.
func rpzDiagnosticsPrimary(t *testing.T) *rpzDiagnosticsFeed {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	feed := &rpzDiagnosticsFeed{address: listener.Addr().String()}
	started := make(chan struct{})
	server := &dns.Server{Listener: listener, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		a := new(dns.Msg)
		a.SetReply(q)
		a.Authoritative = true
		if feed.failing.Load() || len(q.Question) != 1 || q.Question[0].Name != "policy.test." {
			a.Rcode = dns.RcodeServerFailure
			_ = w.WriteMsg(a)
			return
		}
		soa := &dns.SOA{Hdr: dns.RR_Header{Name: "policy.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}, Ns: "ns.policy.test.", Mbox: "hostmaster.policy.test.", Serial: 7, Refresh: 1, Retry: 1, Expire: 6, Minttl: 5}
		a.Answer = []dns.RR{soa}
		switch q.Question[0].Qtype {
		case dns.TypeSOA:
		case dns.TypeAXFR, dns.TypeIXFR:
			feed.transfers.Add(1)
			for _, line := range []string{"policy.test. 30 IN NS ns.policy.test.", "blocked.test.policy.test. 30 IN CNAME .", "ns.example.rpz-nsdname.policy.test. 30 IN CNAME ."} {
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
	case <-time.After(4 * time.Second):
		t.Fatal("RPZ primary did not start")
	}
	return feed
}
