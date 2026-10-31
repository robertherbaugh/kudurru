package blocking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// TestAdGuardBlackBox compares manually authored public-grammar cases with
// an external official AGH executable. Nothing links or copies its GPL code.
// Set KUDURRU_ADGUARD_BINARY to run; an unconfigured machine skips explicitly.
func TestAdGuardBlackBox(t *testing.T) {
	binary := os.Getenv("KUDURRU_ADGUARD_BINARY")
	if binary == "" {
		t.Skip("set KUDURRU_ADGUARD_BINARY to an external official AdGuard Home binary")
	}
	binary, e := filepath.Abs(binary)
	if e != nil {
		t.Fatal(e)
	}
	pc, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := comparatorReply(q)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = up.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = up.Shutdown() })
	// Every fixture rule is first-party test data; domains are intentionally
	// distinct so tests can identify interactions instead of another rule.
	rules := []string{
		"||important.test^", "@@||important.test^", "||important.test^$important",
		"||important-except.test^$important", "@@||important-except.test^$important",
		"||cancel.test^", "||cancel.test^$badfilter",
		"||late-cancel.test^$badfilter", "||late-cancel.test^",
		"||type.test^$dnstype=AAAA", "||neg-type.test^$dnstype=~A|~CNAME",
		"||mixed-type.test^$dnstype=~AAAA|A",
		"||client.test^$client=127.0.0.0/8", "||excluded-client.test^$client=~127.0.0.1",
		"||named.test^$client='Fixture PC'", "||tagged.test^$ctag=device_pc",
		"||negative-tag.test^$ctag=~device_pc",
		"||ads*.wild.test^", "||denyallow.test^$denyallow=good.denyallow.test",
		"||unknown.test^$unsupported-modifier",
		"||rewrite.test^$dnsrewrite=192.0.2.9", "||rewrite.test^",
		"||multi.test^$dnsrewrite=192.0.2.9", "||multi.test^$dnsrewrite=192.0.2.10",
		"||exception-rewrite.test^$dnsrewrite=192.0.2.9", "@@||exception-rewrite.test^$dnsrewrite",
		"||nxdomain-rewrite.test^$dnsrewrite=NXDOMAIN",
		"||wire-target.test^$dnstype=AAAA,client=127.0.0.1",
		"||192.0.2.66^$dnstype=A,ctag=device_pc",
		"||wire-txt.test^$dnsrewrite=NOERROR;TXT;hello\\,world",
		"||combined-client.test^$client=127.0.0.0/8|~127.0.0.1",

		"||keyword-rewrite.test^$dnsrewrite=192.0.2.9", "||keyword-rewrite.test^$dnsrewrite=NOERROR",
		"||keyword-refused.test^$dnsrewrite=192.0.2.9", "||keyword-refused.test^$dnsrewrite=REFUSED",
		"||important-keyword.test^$dnsrewrite=192.0.2.9,important", "||important-keyword.test^$dnsrewrite=REFUSED",
		"||important-multi.test^$dnsrewrite=192.0.2.9,important", "||important-multi.test^$dnsrewrite=192.0.2.10",

		"||important-rewrite.test^$dnsrewrite=192.0.2.9,important", "@@||important-rewrite.test^$dnsrewrite",
		"||important-rewrite-except.test^$dnsrewrite=192.0.2.9,important", "@@||important-rewrite-except.test^$dnsrewrite,important",
		"||rewrite-plain-except.test^$dnsrewrite=192.0.2.9", "@@||rewrite-plain-except.test^$important",
		"||target-priority.test^$dnsrewrite=192.0.2.9", "||target-priority.test^$dnsrewrite=target.example",
		"||txt-rewrite.test^$dnsrewrite=NOERROR;TXT;hello_world",
		"||plain-type.test^$dnstype=A", "plain-domain.test$dnstype=A",
		"0.0.0.0 hosts-cancel.test", "hosts-cancel.test$badfilter",
	}
	sel := selectionFor(t, rules...)
	port := unusedPort(t, "udp")
	httpPort := unusedPort(t, "tcp")
	dir := t.TempDir()
	config := map[string]any{"schema_version": 32, "http": map[string]any{"address": "127.0.0.1:" + strconv.Itoa(httpPort)}, "dns": map[string]any{"bind_hosts": []string{"127.0.0.1"}, "port": port, "upstream_dns": []string{pc.LocalAddr().String()}, "bootstrap_dns": []string{}, "cache_size": 65536, "cache_enabled": false, "ratelimit": 0, "use_private_ptr_resolvers": false}, "filtering": map[string]any{"protection_enabled": true, "filtering_enabled": true, "blocking_mode": "null_ip", "blocked_response_ttl": 60}, "filters": []any{}, "whitelist_filters": []any{}, "user_rules": rules, "users": []any{}, "clients": map[string]any{"persistent": []any{map[string]any{"name": "Fixture PC", "ids": []string{"127.0.0.1"}, "tags": []string{"device_pc"}, "use_global_settings": true, "filtering_enabled": true, "use_global_blocked_services": true}}}}
	data, _ := json.MarshalIndent(config, "", "  ")
	path := filepath.Join(dir, "AdGuardHome.yaml")
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "-c", path, "-w", dir, "--no-check-update", "--no-permcheck") //nolint:gosec // explicitly opt-in external comparator, never request input
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	// Read process logs only after Wait; the process writes asynchronously.
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	client := &dns.Client{Net: "udp", Timeout: 100 * time.Millisecond}
	server := "127.0.0.1:" + strconv.Itoa(port)
	ready := false
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
		q := new(dns.Msg)
		q.SetQuestion("ready.fixture.test.", dns.TypeA)
		if _, _, e = client.Exchange(q, server); e == nil {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("external comparator did not start")
	}
	client.Timeout = time.Second
	cases := []struct {
		name string
		typ  uint16
	}{
		{"important.test.", dns.TypeA}, {"important-except.test.", dns.TypeA}, {"cancel.test.", dns.TypeA}, {"late-cancel.test.", dns.TypeA},
		{"type.test.", dns.TypeA}, {"type.test.", dns.TypeAAAA}, {"neg-type.test.", dns.TypeA}, {"neg-type.test.", dns.TypeAAAA}, {"mixed-type.test.", dns.TypeA}, {"mixed-type.test.", dns.TypeAAAA},
		{"client.test.", dns.TypeA}, {"excluded-client.test.", dns.TypeA}, {"named.test.", dns.TypeA}, {"tagged.test.", dns.TypeA}, {"negative-tag.test.", dns.TypeA},
		{"ads123.wild.test.", dns.TypeA}, {"notads.wild.test.", dns.TypeA}, {"denyallow.test.", dns.TypeA}, {"good.denyallow.test.", dns.TypeA},
		{"unknown.test.", dns.TypeA}, {"rewrite.test.", dns.TypeA}, {"rewrite.test.", dns.TypeAAAA}, {"multi.test.", dns.TypeA}, {"exception-rewrite.test.", dns.TypeA}, {"nxdomain-rewrite.test.", dns.TypeA},
		{"keyword-rewrite.test.", dns.TypeA}, {"keyword-refused.test.", dns.TypeA}, {"important-keyword.test.", dns.TypeA}, {"important-multi.test.", dns.TypeA}, {"important-rewrite.test.", dns.TypeA}, {"important-rewrite-except.test.", dns.TypeA},
		{"rewrite-plain-except.test.", dns.TypeA}, {"target-priority.test.", dns.TypeCNAME}, {"txt-rewrite.test.", dns.TypeTXT},
		{"plain-type.test.", dns.TypeA}, {"plain-domain.test.", dns.TypeA}, {"sub.plain-domain.test.", dns.TypeA}, {"hosts-cancel.test.", dns.TypeA},
		{"wire-alias.test.", dns.TypeA}, {"wire-alias.test.", dns.TypeAAAA}, {"wire-ip.test.", dns.TypeA}, {"wire-txt.test.", dns.TypeTXT}, {"combined-client.test.", dns.TypeA},
		{"wire-target.test.", dns.TypeA}, {"wire-target.test.", dns.TypeAAAA},
	}
	for _, tc := range cases {
		t.Run(tc.name+dns.TypeToString[tc.typ], func(t *testing.T) {
			q := new(dns.Msg)
			q.SetQuestion(tc.name, tc.typ)
			reply, _, err := client.Exchange(q, server)
			if err != nil {
				t.Fatal(err)
			}
			ctx := Context{Name: tc.name, Type: tc.typ, Class: 1, ClientIP: netip.MustParseAddr("127.0.0.1"), ClientNames: []string{"Fixture PC"}, Tags: []string{"device_pc"}}
			decision := sel.EvaluateContext(ctx)
			raw := comparatorReply(q)
			if decision.Action == ActionPass || decision.Reason == ReasonException {
				var names []string
				var addresses []netip.Addr
				for _, rr := range raw.Answer {
					switch r := rr.(type) {
					case *dns.CNAME:
						names = append(names, r.Target)
					case *dns.A:
						if a, ok := netip.AddrFromSlice(r.A); ok {
							addresses = append(addresses, a.Unmap())
						}
					case *dns.AAAA:
						if a, ok := netip.AddrFromSlice(r.AAAA); ok {
							addresses = append(addresses, a.Unmap())
						}
					}
				}
				if d := sel.EvaluateResponseContext(ctx, names, addresses); d.Action == ActionBlock {
					decision = d
				}
			}
			expectedCode := dns.RcodeSuccess
			var expected []dns.RR
			switch decision.Action {
			case ActionBlock:
				switch tc.typ {
				case dns.TypeA:
					expected = []dns.RR{&dns.A{A: net.IPv4zero}}
				case dns.TypeAAAA:
					expected = []dns.RR{&dns.AAAA{AAAA: net.IPv6zero}}
				}
			case ActionRewrite:
				expectedCode = decision.Rewrite.Rcode()
				expected = decision.Rewrite.Records(tc.name, tc.typ, 1, 60)
			default:
				expected = raw.Answer
			}
			// v0.107.79 differs from the public specification: bare NOERROR
			// does not replace a simultaneously matched address rewrite. Our
			// keyword-first behavior follows the documented rule instead.
			if tc.name == "keyword-rewrite.test." {
				if len(expected) != 0 || expectedCode != dns.RcodeSuccess {
					t.Fatal("our keyword-first contract changed")
				}
				t.Log("documented v0.107.79 deviation: bare NOERROR loses to an address rewrite")
				expected = []dns.RR{&dns.A{A: net.IPv4(192, 0, 2, 9)}}
			}
			if tc.name == "wire-alias.test." && tc.typ == dns.TypeAAAA {
				if decision.Action != ActionBlock || !decision.Response {
					t.Fatal("pinned response selector contract changed")
				}
				t.Log("documented v0.107.79 deviation: combined client/type selector skips returned CNAME; our answer inspection retains query context")
				expected = raw.Answer
			}
			if reply.Rcode != expectedCode || len(reply.Answer) != len(expected) {
				t.Fatalf("AGH rcode=%d records=%v; ours=%s rcode=%d records=%v", reply.Rcode, reply.Answer, decision.Action.String(), expectedCode, expected)
			}
			addresses := map[string]bool{}
			for _, rr := range reply.Answer {
				addresses[rrAddress(rr)] = true
			}
			for _, rr := range expected {
				if !addresses[rrAddress(rr)] {
					t.Fatalf("AGH %v != ours %v", reply.Answer, expected)
				}
			}
		})
	}
}
func rrAddress(rr dns.RR) string {
	switch r := rr.(type) {
	case *dns.A:
		return r.A.String()
	case *dns.AAAA:
		return r.AAAA.String()
	}
	rrCopy := dns.Copy(rr)
	rrCopy.Header().Name, rrCopy.Header().Ttl, rrCopy.Header().Class = ".", 0, dns.ClassINET
	return fmt.Sprint(rrCopy)
}
func unusedPort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		pc, e := net.ListenPacket("udp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = pc.Close() }()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// comparatorReply is our first-party deterministic raw upstream, shared by
// the external executable and the expected unfiltered response fixture.
func comparatorReply(q *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(q)
	m.RecursionAvailable = true
	h := dns.RR_Header{Name: q.Question[0].Name, Rrtype: q.Question[0].Qtype, Class: 1, Ttl: 60}
	address := "192.0.2.77"
	if h.Name == "wire-ip.test." {
		address = "192.0.2.66"
	}
	if h.Name == "wire-alias.test." {
		m.Answer = append(m.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: h.Name, Rrtype: dns.TypeCNAME, Class: 1, Ttl: 60}, Target: "wire-target.test."})
		h.Name = "wire-target.test."
	}
	switch h.Rrtype {
	case dns.TypeA:
		m.Answer = append(m.Answer, &dns.A{Hdr: h, A: net.ParseIP(address)})
	case dns.TypeAAAA:
		m.Answer = append(m.Answer, &dns.AAAA{Hdr: h, AAAA: net.ParseIP("2001:db8::77")})
	}
	return m
}
