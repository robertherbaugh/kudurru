package authoritative

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// This acceptance receives a real BIND-signed AXFR and checks the production
// authoritative assembly through actual UDP/TCP listeners. The validator here
// is only independent test evidence, explicitly anchored to this fixture.
func TestBINDSignedSecondaryThroughListeners(t *testing.T) {
	for _, format := range []string{"NSEC", "NSEC3", "NSEC3-optout"} {
		t.Run(format, func(t *testing.T) {
			const origin = "signed-secondary.test."
			dir := t.TempDir()
			anchor := bindSecondarySignedFile(t, dir, origin, format)
			addr := bindSignedPrimary(t, dir, origin)
			dz := dnsZone(origin, func(dz *v1alpha1.DNSZone) {
				dz.Spec.Type = v1alpha1.ZoneTypeSecondary
				dz.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{addr}, TransferProtocol: "tcp"}
			})
			client := &secondary.TransferClient{Timeout: time.Second}
			records, err := client.AXFR(t.Context(), dz, addr)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			z, err := zone.BuildSecondary(dz, records, now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			holder := new(zone.Holder)
			snap, err := zone.NewSnapshot(1, true, z)
			if err != nil {
				t.Fatal(err)
			}
			holder.Store(snap)
			stage := New(holder)
			stage.Now = func() time.Time { return now }
			anyReq := (&Case{Qname: origin, Qtype: dns.TypeANY, Proto: pipeline.ProtoUDP}).request()
			anyReq.Msg.SetEdns0(1232, true)
			fallback := &recorder{}
			anyResp, err := stage.Serve(anyReq, fallback.next)
			if err != nil || anyResp.Msg.Rcode != dns.RcodeSuccess || anyResp.Msg.AuthenticatedData || len(fallback.calls) != 0 {
				t.Fatalf("signed stage ANY failed: %+v %v", anyResp, err)
			}
			var anyType uint16
			for _, rr := range anyResp.Msg.Answer {
				if rr.Header().Rrtype != dns.TypeRRSIG {
					if anyType != 0 && anyType != rr.Header().Rrtype {
						t.Fatal("stage UDP ANY returned multiple RRsets")
					}
					anyType = rr.Header().Rrtype
				}
			}
			if anyType == 0 || !signedResponseHasRRSIG(anyResp.Msg) {
				t.Fatal("stage UDP ANY omitted its signed RRset")
			}
			for _, rr := range anyResp.Msg.Answer {
				if sig, ok := rr.(*dns.RRSIG); ok && sig.TypeCovered != anyType {
					t.Fatal("stage UDP ANY retained unrelated signatures")
				}
			}
			verifyBindSignedRRsets(t, records, anyResp.Msg.Answer)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			chain := pipeline.MustNew(&ingress.Stage{}, &postprocess.Stage{Now: func() time.Time { return now }}, stage)
			server := dnsserver.New("127.0.0.1:0", []string{"udp", "tcp"}, chain, logger)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- server.Start(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("signed listeners failed to stop")
				}
			})
			awaitSigned(t, func() bool { return len(server.Addrs()) == 2 })
			validate, err := dnssec.New(dnssec.Options{Anchors: []dns.RR{anchor}, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			fetch := func(ctx context.Context, name string, typ uint16) (*dns.Msg, error) {
				q := new(dns.Msg)
				q.SetQuestion(name, typ)
				q.SetEdns0(1232, true)
				r, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(ctx, q, server.Addrs()["tcp"])
				return r, err
			}
			cases := []struct {
				name     string
				typ      uint16
				rcode    int
				referral bool
			}{
				{"www.", dns.TypeA, dns.RcodeSuccess, false},
				{"alias.", dns.TypeA, dns.RcodeSuccess, false},
				{"child.wildcname.", dns.TypeA, dns.RcodeSuccess, false},
				{"child.negcname.", dns.TypeA, dns.RcodeNameError, false},
				{"child.wild.", dns.TypeA, dns.RcodeSuccess, false},
				{"www.", dns.TypeAAAA, dns.RcodeSuccess, false},
				{"missing.", dns.TypeA, dns.RcodeNameError, false},
				{"child.wild.", dns.TypeAAAA, dns.RcodeSuccess, false},
				{"branch.", dns.TypeA, dns.RcodeSuccess, false},
				{"ns.insecure.", dns.TypeA, dns.RcodeSuccess, true},
				{"sameglue.", dns.TypeA, dns.RcodeSuccess, true},
				{"insecure.", dns.TypeDS, dns.RcodeSuccess, false},
				{"secure.", dns.TypeDS, dns.RcodeSuccess, false},
				{"ns.secure.", dns.TypeA, dns.RcodeSuccess, true},
				{"x.dname.", dns.TypeA, dns.RcodeSuccess, false},
				{"", dns.TypeDNSKEY, dns.RcodeSuccess, false},
				{"", dns.TypeSOA, dns.RcodeSuccess, false},
			}
			if format == "NSEC3-optout" {
				cases = []struct {
					name     string
					typ      uint16
					rcode    int
					referral bool
				}{{"insecure.", dns.TypeDS, dns.RcodeSuccess, false}, {"ns.insecure.", dns.TypeA, dns.RcodeSuccess, true}, {"www.", dns.TypeA, dns.RcodeSuccess, false}}
			}
			for _, proto := range []string{"udp", "tcp"} {
				for _, tc := range cases {
					q := new(dns.Msg)
					q.SetQuestion(tc.name+origin, tc.typ)
					q.RecursionDesired = tc.name != "x.dname."
					q.SetEdns0(1232, true)
					q.AuthenticatedData = true
					reply, _, err := (&dns.Client{Net: proto, Timeout: time.Second}).ExchangeContext(t.Context(), q, server.Addrs()[proto])
					if err != nil {
						t.Fatal(err)
					}
					if reply.Rcode != tc.rcode || reply.AuthenticatedData || reply.Authoritative == tc.referral {
						t.Fatalf("%s %s/%s: %s", proto, q.Question[0].Name, dns.TypeToString[tc.typ], reply)
					}
					// The DNAME fixture ends outside configured authority; RD=0 yields a
					// signed local DNAME and synthesized CNAME without recursive forwarding.
					if tc.name == "x.dname." {
						verifyBindSignedRRsets(t, records, reply.Answer, reply.Ns)
						continue
					}
					if tc.referral {
						if tc.name == "ns.secure." && !signedSectionHasType(reply.Ns, dns.TypeDS) {
							t.Fatalf("secure referral omitted parent DS: %s", reply)
						}
						verifyBindSignedRRsets(t, records, reply.Ns)
						continue
					}
					if format == "NSEC3-optout" && tc.typ == dns.TypeDS {
						// Direct DS opt-out is authenticated delegation absence, not
						// ordinary wildcard NODATA. Compare BIND's proof selection and
						// verify every emitted signature independently.
						primary, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(t.Context(), q, addr)
						if err != nil {
							t.Fatal(err)
						}
						verifyBindOptOutDS(t, origin, q.Question[0].Name, reply, primary)
						verifyBindSignedRRsets(t, records, reply.Ns)
						verdict, err := validate.Validate(t.Context(), q.Question[0], reply, fetch)
						if err != nil || verdict.Security.State != pipeline.ValidationInsecure || reply.AuthenticatedData {
							t.Fatalf("direct opt-out DS must validate Insecure with AD clear: %s %v\n%s", verdict.Security.Reason, err, reply)
						}
						continue
					}
					verdict, err := validate.Validate(t.Context(), q.Question[0], reply, fetch)
					if err != nil || verdict.Security.State != pipeline.ValidationSecure {
						t.Fatalf("external verification %s/%s: %s %v\n%s", q.Question[0].Name, dns.TypeToString[tc.typ], verdict.Security.Reason, err, reply)
					}
				}
			}
			verifySignedListenerDiagnostics(t, server, records)
			for _, cd := range []bool{false, true} {
				for _, do := range []bool{false, true} {
					q := new(dns.Msg)
					q.SetQuestion("www."+origin, dns.TypeA)
					q.SetEdns0(1232, do)
					q.CheckingDisabled = cd
					q.AuthenticatedData = true
					r, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(t.Context(), q, server.Addrs()["tcp"])
					if err != nil || r.AuthenticatedData || r.CheckingDisabled != cd || signedResponseHasRRSIG(r) != do {
						t.Fatalf("DO/CD/AD: %v %v", r, err)
					}
				}
			}
			verifySignedSecondaryStageBoundaries(t, dz, records, now)
			// The exported transfer remains the exact original signed zone records.
			var exported []dns.RR
			for chunk := range z.AXFR(zone.DefaultChunkBytes) {
				exported = append(exported, chunk...)
			}
			if len(exported) != len(records)+1 {
				t.Fatal("signed rendering changed transfer record count")
			}
			t.Logf("BIND %s signed AXFR -> immutable zone -> ingress/postprocess/authoritative -> UDP/TCP: independent positive, wildcard, alias and denial verification; AD clear", format)
		})
	}
}

func signedResponseHasRRSIG(m *dns.Msg) bool {
	for _, section := range [][]dns.RR{m.Answer, m.Ns} {
		for _, rr := range section {
			if rr.Header().Rrtype == dns.TypeRRSIG {
				return true
			}
		}
	}
	return false
}
func awaitSigned(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("signed fixture readiness timeout")
}
func signedTool(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	bin, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s unavailable", name)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // Test fixture executable and generated temporary paths.
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}
func bindSecondarySignedFile(t *testing.T, dir, origin, format string) *dns.DS {
	t.Helper()
	key := signedTool(t, dir, "dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", "-f", "KSK", origin)
	signedTool(t, dir, "dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", origin)
	content := fmt.Sprintf(`$ORIGIN %s
$TTL 300
@ IN SOA ns.%s admin.%s 4294967295 60 10 3600 60
@ IN NS ns
ns IN A 127.0.0.1
www IN A 192.0.2.80
alias IN CNAME www
*.wildcname IN CNAME alias
*.negcname IN CNAME missing
external IN CNAME host.outside.test.
childalias IN CNAME www.child
*.wild IN A 192.0.2.81
leaf.branch IN TXT "empty nonterminal"
dname IN DNAME outside.test.
insecure IN NS ns.insecure
ns.insecure IN A 192.0.2.54
sameglue IN NS sameglue
sameglue IN A 192.0.2.55
secure IN NS ns.secure
ns.secure IN A 192.0.2.56
secure IN DS 1234 13 2 00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF
`, origin, origin, origin)
	if err := os.WriteFile(filepath.Join(dir, "zone"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-S", "-n", "1", "-o", origin, "-f", "zone.signed"}
	if format != "NSEC" {
		args = append(args, "-3", "-", "-H", "0")
	}
	if format == "NSEC3-optout" {
		args = append(args, "-A")
	}
	signedTool(t, dir, "dnssec-signzone", append(args, "zone")...)
	signedTool(t, dir, "dnssec-verify", "-o", origin, "zone.signed")
	text := signedTool(t, dir, "dnssec-dsfromkey", "-2", key+".key")
	rr, err := dns.NewRR(text)
	if err != nil {
		t.Fatal(err)
	}
	return rr.(*dns.DS)
}
func bindSignedPrimary(t *testing.T, dir, origin string) string {
	t.Helper()
	bin, err := exec.LookPath("named")
	if err != nil {
		t.Skip("BIND named unavailable")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	conf := fmt.Sprintf(`options { directory %q; listen-on port %s { 127.0.0.1; }; listen-on-v6 { none; }; recursion no; dnssec-validation no; notify no; pid-file "named.pid"; session-keyfile "session.key"; }; controls {}; zone %q { type primary; file "zone.signed"; allow-transfer { any; }; };`, dir, port, origin)
	path := filepath.Join(dir, "named.conf")
	if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "named.log")) //nolint:gosec // Path is inside this test's temporary fixture directory.
	if err != nil {
		t.Fatal(err)
	} //nolint:gosec // Test temporary directory.
	cmd := exec.Command(bin, "-g", "-4", "-n", "1", "-c", path) //nolint:gosec // Native test fixture executable.
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("named shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				if err := <-done; err == nil {
					t.Error("killed named exited successfully")
				}
			}
			_ = log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(filepath.Join(dir, "named.log")) //nolint:gosec // Path is inside this test's temporary fixture directory.
				t.Log(string(data))
			}
		})
	} //nolint:gosec // Test temporary log.
	t.Cleanup(stop)
	awaitSigned(t, func() bool {
		q := new(dns.Msg)
		q.SetQuestion(origin, dns.TypeSOA)
		r, _, err := (&dns.Client{Timeout: 100 * time.Millisecond}).ExchangeContext(t.Context(), q, addr)
		return err == nil && r.Rcode == dns.RcodeSuccess && r.Authoritative
	})
	return addr
}
func verifyBindSignedRRsets(t *testing.T, records []dns.RR, sections ...[]dns.RR) {
	t.Helper()
	var keys []*dns.DNSKEY
	for _, rr := range records {
		if key, ok := rr.(*dns.DNSKEY); ok {
			keys = append(keys, key)
		}
	}
	for _, section := range sections {
		for _, rr := range section {
			typ := rr.Header().Rrtype
			if typ == dns.TypeRRSIG || typ == dns.TypeNS || (typ == dns.TypeCNAME && signedSectionHasType(section, dns.TypeDNAME)) {
				continue
			}
			covered := false
			for _, record := range section {
				if sig, ok := record.(*dns.RRSIG); ok && sig.TypeCovered == typ && strings.EqualFold(sig.Hdr.Name, rr.Header().Name) {
					covered = true
					break
				}
			}
			if !covered {
				t.Fatalf("emitted signed RRset omitted its signature: %s", rr)
			}
		}
		for _, rr := range section {
			sig, ok := rr.(*dns.RRSIG)
			if !ok {
				continue
			}
			var set []dns.RR
			for _, record := range section {
				if dns.CanonicalName(record.Header().Name) == dns.CanonicalName(sig.Hdr.Name) && record.Header().Rrtype == sig.TypeCovered {
					set = append(set, record)
				}
			}
			valid := false
			for _, key := range keys {
				if sig.Verify(key, set) == nil {
					valid = true
					break
				}
			}
			if !valid {
				t.Fatalf("serialized BIND signature failed verification: %s", sig)
			}
		}
	}
}

func verifyBindOptOutDS(t *testing.T, origin, name string, reply, primary *dns.Msg) {
	t.Helper()
	if primary.Rcode != dns.RcodeSuccess || len(primary.Answer) != 0 || !primary.Authoritative {
		t.Fatalf("BIND did not confirm DS absence: %s", primary)
	}
	closest, covered := false, false
	count := 0
	for _, rr := range reply.Ns {
		n, ok := rr.(*dns.NSEC3)
		if !ok {
			continue
		}
		count++
		matched := false
		for _, original := range primary.Ns {
			if dns.IsDuplicate(n, original) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("opt-out proof differs from BIND: %s", n)
		}
		hash := dns.HashName(origin, n.Hash, n.Iterations, n.Salt)
		owner := strings.ToUpper(dns.SplitDomainName(n.Hdr.Name)[0])
		closest = closest || owner == hash
		query := dns.HashName(name, n.Hash, n.Iterations, n.Salt)
		next := strings.ToUpper(n.NextDomain)
		inside := (owner < next && owner < query && query < next) || (owner >= next && (query > owner || query < next))
		covered = covered || (n.Flags&1 != 0 && inside)
	}
	if count == 0 || !closest || !covered {
		t.Fatalf("missing closest-encloser or opt-out next-closer proof: %s", reply)
	}
}

func verifySignedSecondaryStageBoundaries(t *testing.T, dz *v1alpha1.DNSZone, records []dns.RR, now time.Time) {
	t.Helper()
	origin := dns.Fqdn(dz.Spec.ZoneName)
	var expiration uint32
	for _, rr := range records {
		if sig, ok := rr.(*dns.RRSIG); ok {
			expiration = max(expiration, sig.Expiration)
		}
	}
	expiredAt := time.Unix(int64(expiration), 0)
	z, err := zone.BuildSecondary(dz, records, expiredAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	childConfig := dnsZone("child."+origin, func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeSecondary })
	child, err := zone.BuildSecondaryPlaceholder(childConfig)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := zone.NewSnapshot(2, true, z, child)
	if err != nil {
		t.Fatal(err)
	}
	holder := new(zone.Holder)
	holder.Store(snapshot)
	stage := New(holder)
	stage.Now = func() time.Time { return expiredAt }
	for _, cd := range []bool{false, true} {
		for _, name := range []string{"www.", "missing.", "child.wild.", "alias."} {
			req := (&Case{Qname: name + origin, Qtype: dns.TypeA}).request()
			req.Msg.SetEdns0(1232, true)
			req.Msg.CheckingDisabled = cd
			fallback := &recorder{}
			resp, err := stage.Serve(req, fallback.next)
			if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || len(resp.Msg.Answer) != 0 || len(resp.Msg.Ns) != 0 || len(fallback.calls) != 0 || resp.Origin != pipeline.OriginFailure || resp.Msg.AuthenticatedData {
				t.Fatalf("expired signature failed open %s: %+v %v", name, resp, err)
			}
		}
	}
	for _, typ := range []uint16{dns.TypeA, dns.TypeRRSIG, dns.TypeDNSKEY} {
		for _, do := range []bool{false, true} {
			if typ == dns.TypeA && do {
				continue
			}
			name := origin
			if typ == dns.TypeA {
				name = "www." + origin
			}
			req := (&Case{Qname: name, Qtype: typ}).request()
			req.Msg.SetEdns0(1232, do)
			fallback := &recorder{}
			resp, err := stage.Serve(req, fallback.next)
			if err != nil || resp.Msg.Rcode != dns.RcodeSuccess || len(resp.Msg.Answer) == 0 || len(fallback.calls) != 0 || resp.Msg.AuthenticatedData || resp.Security.State != pipeline.ValidationUnknown {
				t.Fatalf("diagnostic/DO=0 answer unavailable: %+v %v", resp, err)
			}
		}
	}
	stage.Now = func() time.Time { return now }
	for _, noRD := range []bool{false, true} {
		req := (&Case{Qname: "childalias." + origin, Qtype: dns.TypeA, NoRD: noRD}).request()
		req.Msg.SetEdns0(1232, true)
		fallback := &recorder{}
		resp, err := stage.Serve(req, fallback.next)
		if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || len(resp.Msg.Answer) != 0 || len(fallback.calls) != 0 {
			t.Fatalf("signed alias bypassed configured unavailable child: %+v %v", resp, err)
		}
	}
	req := (&Case{Qname: "external." + origin, Qtype: dns.TypeA}).request()
	req.Msg.SetEdns0(1232, true)
	resp, err := stage.Serve(req, func(sub *pipeline.Request) (*pipeline.Response, error) {
		out := pipeline.Reply(sub, dns.RcodeSuccess, pipeline.StatusAuthoritative)
		out.Msg.Answer = []dns.RR{mustRR("host.outside.test. 30 IN A 192.0.2.90")}
		out.Msg.AuthenticatedData = true
		out.Origin = pipeline.OriginForwarded
		out.Security.State = pipeline.ValidationSecure
		return out, nil
	})
	if err != nil || resp.Origin != pipeline.OriginMixed || !resp.Modified || resp.Security.State != pipeline.ValidationUnknown || resp.Msg.AuthenticatedData || resp.Msg.Authoritative || len(resp.Msg.Answer) != 3 {
		t.Fatalf("signed external splice inherited trust or lost prefix: %+v %v", resp, err)
	}
}

func signedSectionHasType(section []dns.RR, typ uint16) bool {
	for _, rr := range section {
		if rr.Header().Rrtype == typ {
			return true
		}
	}
	return false
}

func verifySignedListenerDiagnostics(t *testing.T, server *dnsserver.Server, records []dns.RR) {
	t.Helper()
	var apex string
	selected := map[uint16]string{}
	for _, rr := range records {
		if soa, ok := rr.(*dns.SOA); ok {
			apex = soa.Hdr.Name
		}
		switch rr.Header().Rrtype {
		case dns.TypeRRSIG, dns.TypeDNSKEY, dns.TypeDS, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM:
			selected[rr.Header().Rrtype] = rr.Header().Name
		}
	}
	for typ, name := range selected {
		for _, do := range []bool{false, true} {
			q := new(dns.Msg)
			q.SetQuestion(name, typ)
			q.SetEdns0(1232, do)
			r, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(t.Context(), q, server.Addrs()["tcp"])
			if err != nil || r.Rcode != dns.RcodeSuccess || r.AuthenticatedData {
				t.Fatalf("explicit stored DNSSEC query: %s %v", r, err)
			}
			count := 0
			for _, expected := range records {
				if expected.Header().Rrtype != typ || !strings.EqualFold(expected.Header().Name, name) {
					continue
				}
				count++
				found := false
				for _, got := range r.Answer {
					if dns.IsDuplicate(expected, got) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("explicit DNSSEC query changed raw data %s: %s", expected, r)
				}
			}
			if count == 0 {
				t.Fatal("empty diagnostic fixture")
			}
		}
	}
	q := new(dns.Msg)
	q.SetQuestion(apex, dns.TypeANY)
	q.SetEdns0(1232, true)
	r, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).ExchangeContext(t.Context(), q, server.Addrs()["udp"])
	// Ingress refuses ANY before the authoritative stage (the existing policy).
	if err != nil || r.Rcode != dns.RcodeRefused || len(r.Answer) != 0 {
		t.Fatalf("ingress ANY policy changed: %s %v", r, err)
	}
}
