package dnsserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// The tests in this file run zone transfers end to end: a miekg dns.Transfer
// (the BIND-style client flow: one TCP connection, a stream of messages,
// TSIG verified on every one of them) against the real listener registry
// with the authoritative stage behind it.

var (
	xfrSecret    = []byte("0123456789abcdef0123456789abcdef")
	xfrSecret512 = []byte("fedcba9876543210fedcba9876543210fedcba9876543210")
	otherSecret  = []byte("not-the-secret-not-the-secret-!!")
)

const (
	xfrKey    = "xfr-key."
	xfrKey512 = "xfr512.example."
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func testKeyring(t testing.TB) *StaticKeyring {
	t.Helper()
	k, err := NewStaticKeyring(
		TSIGKey{Name: "XFR-Key", Algorithm: "hmac-sha256", Secret: xfrSecret},
		TSIGKey{Name: xfrKey512, Algorithm: "HMAC-SHA512.", Secret: xfrSecret512},
	)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// bigZone has enough data for a multi-message AXFR: 2,000 TXT records of
// about 100 octets plus some A records.
func bigZone(t testing.TB, name string, policy v1alpha1.ZonePolicy) *zone.Zone {
	t.Helper()
	dz := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: strings.TrimSuffix(name, "."), Generation: 1},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName: name, Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1." + name},
			DefaultTTL: 300, Policy: policy,
		},
		Status: v1alpha1.DNSZoneStatus{Serial: 2026101101},
	}
	eps := []v1alpha1.Endpoint{{DNSName: "ns1." + name, RecordType: "A", Targets: []string{"192.0.2.53"}}}
	for i := range 2000 {
		eps = append(eps, v1alpha1.Endpoint{
			DNSName: fmt.Sprintf("t%04d.%s", i, name), RecordType: "TXT",
			Targets: []string{fmt.Sprintf("v=record %04d %s", i, strings.Repeat("x", 80))},
		})
	}
	z, errs := zone.Build(dz, []*v1alpha1.DNSRecordSet{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dns", Name: "records"},
		Spec:       v1alpha1.DNSRecordSetSpec{Endpoints: eps},
	}}, zone.BuildOptions{})
	if !errs.Empty() {
		t.Fatalf("build: %v %v", errs.Zone, errs.RecordSets)
	}
	return z
}

type xfrFixture struct {
	reg      *Registry
	stage    *authoritative.Stage
	keyed    *zone.Zone // example.org.: allow + TSIG key
	open     *zone.Zone // open.example.: useNetworkACL 127.0.0.1, no key
	mu       sync.Mutex
	failures []string
}

func (f *xfrFixture) tcp() string { return f.reg.Addr(ProtoTCP).String() }
func (f *xfrFixture) udp() string { return f.reg.Addr(ProtoUDP).String() }

func (f *xfrFixture) failed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.failures)
}

func startXFR(t *testing.T) *xfrFixture {
	t.Helper()
	f := &xfrFixture{
		keyed: bigZone(t, "example.org.", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow, TransferTSIGKeyRefs: []string{"xfr-key", xfrKey512}}),
		open:  bigZone(t, "open.example.", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: []string{"127.0.0.1"}}),
	}
	snap, err := zone.NewSnapshot(1, true, f.keyed, f.open)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	f.stage = authoritative.New(h)
	f.stage.Metrics = authoritative.NewMetrics(nil)
	reg, err := NewRegistry(Options{
		Handler: &Handler{Chain: pipeline.MustNew(f.stage), Logger: quietLogger},
		Logger:  quietLogger,
		TSIG:    testKeyring(t),
		OnTSIGFailure: func(reason string) {
			f.mu.Lock()
			f.failures = append(f.failures, reason)
			f.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"}, Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; {
		if ok, _ := reg.Ready(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listeners not ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.reg = reg
	return f
}

// axfr runs a transfer with dns.Transfer and returns the envelopes' records
// per message, or the first error.
func axfr(t *testing.T, addr, zoneName string, secrets map[string]string, key, alg string) ([][]dns.RR, error) {
	t.Helper()
	m := new(dns.Msg)
	m.SetAxfr(zoneName)
	tr := &dns.Transfer{TsigSecret: secrets}
	if key != "" {
		m.SetTsig(key, alg, 300, time.Now().Unix())
	}
	ch, err := tr.In(m, addr)
	if err != nil {
		return nil, err
	}
	var out [][]dns.RR
	for env := range ch {
		if env.Error != nil {
			return out, env.Error
		}
		out = append(out, env.RR)
	}
	return out, nil
}

// wantStream is the zone in AXFR order: SOA, every RR in canonical order,
// SOA.
func wantStream(z *zone.Zone) []string {
	var out []string
	z.RRsets(func(s *zone.RRset) bool {
		for _, rr := range s.RRs {
			out = append(out, rr.String())
		}
		return true
	})
	return append(out, z.SOA().String())
}

func flatten(msgs [][]dns.RR) []string {
	var out []string
	for _, m := range msgs {
		for _, rr := range m {
			out = append(out, rr.String())
		}
	}
	return out
}

func TestAXFRWithTSIG(t *testing.T) {
	f := startXFR(t)
	for _, tc := range []struct {
		name, key, alg string
		secret         []byte
	}{
		{"hmac-sha256", xfrKey, dns.HmacSHA256, xfrSecret},
		{"hmac-sha512", xfrKey512, dns.HmacSHA512, xfrSecret512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// miekg verifies the TSIG of every message (the first with the
			// request MAC, the rest timers-only and chained), so a stream
			// that arrives at all is correctly signed throughout.
			msgs, err := axfr(t, f.tcp(), "example.org.", map[string]string{tc.key: b64(tc.secret)}, tc.key, tc.alg)
			if err != nil {
				t.Fatalf("AXFR: %v", err)
			}
			if len(msgs) < 3 {
				t.Fatalf("a ~250 KB zone came in %d messages; want several of at most 64 KiB", len(msgs))
			}
			got, want := flatten(msgs), wantStream(f.keyed)
			if !slices.Equal(got, want) {
				t.Fatalf("stream differs: got %d RRs, want %d; first %q last %q", len(got), len(want), got[0], got[len(got)-1])
			}
			if got[0] != got[len(got)-1] || !strings.Contains(got[0], "SOA") {
				t.Fatal("SOA must be first and last")
			}
		})
	}
	// The server counts a transfer after writing its last message, which
	// may be just after the client has read it.
	for deadline := time.Now().Add(2 * time.Second); testutilCount(f.stage, "example.org.", authoritative.TransferSuccess) != 2; {
		if time.Now().After(deadline) {
			t.Fatalf("success transfers counted %v, want 2", testutilCount(f.stage, "example.org.", authoritative.TransferSuccess))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAXFRMessagesFit64KiB(t *testing.T) {
	f := startXFR(t)
	conn, err := net.Dial("tcp", f.tcp())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	m := new(dns.Msg)
	m.SetAxfr("open.example.")
	co := &dns.Conn{Conn: conn}
	if err := co.WriteMsg(m); err != nil {
		t.Fatal(err)
	}
	soas := 0
	for soas < 2 {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var l uint16
		if err := binary.Read(conn, binary.BigEndian, &l); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, l)
		if _, err := ioReadFull(conn, buf); err != nil {
			t.Fatal(err)
		}
		r := new(dns.Msg)
		if err := r.Unpack(buf); err != nil {
			t.Fatal(err)
		}
		if !r.Authoritative || r.Rcode != dns.RcodeSuccess || r.Id != m.Id {
			t.Fatalf("message header %v", r.MsgHdr)
		}
		for _, rr := range r.Answer {
			if rr.Header().Rrtype == dns.TypeSOA {
				soas++
			}
		}
	}
}

func ioReadFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := c.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestAXFRPolicyOverTheWire(t *testing.T) {
	f := startXFR(t)
	// ACL-only zone: loopback is allowed without a key.
	msgs, err := axfr(t, f.tcp(), "open.example.", nil, "", "")
	if err != nil || !slices.Equal(flatten(msgs), wantStream(f.open)) {
		t.Fatalf("ACL transfer: %v (%d messages)", err, len(msgs))
	}
	// Key-protected zone without a key: REFUSED.
	c := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetAxfr("example.org.")
	r, _, err := c.Exchange(m, f.tcp())
	if err != nil || r.Rcode != dns.RcodeRefused {
		t.Fatalf("unsigned AXFR of a keyed zone: %v %v", r, err)
	}
	// AXFR over UDP: REFUSED even where TCP is allowed.
	c.Net = "udp"
	m.SetAxfr("open.example.")
	r, _, err = c.Exchange(m, f.udp())
	if err != nil || r.Rcode != dns.RcodeRefused {
		t.Fatalf("AXFR over UDP: %v %v", r, err)
	}
	// IXFR with a current serial: the single SOA, over UDP too.
	m = new(dns.Msg)
	m.SetIxfr("open.example.", f.open.Serial(), "ns1.open.example.", "hostmaster.open.example.")
	r, _, err = c.Exchange(m, f.udp())
	if err != nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.SOA).Serial != f.open.Serial() {
		t.Fatalf("IXFR up to date: %v %v", r, err)
	}
	// IXFR from an old serial over TCP falls back to the whole zone (RFC
	// 1995 §4).
	m.SetIxfr("open.example.", f.open.Serial()-10, "ns1.open.example.", "hostmaster.open.example.")
	tr := &dns.Transfer{}
	ch, err := tr.In(m, f.tcp())
	if err != nil {
		t.Fatal(err)
	}
	var got [][]dns.RR
	for env := range ch {
		if env.Error != nil {
			t.Fatal(env.Error)
		}
		got = append(got, env.RR)
	}
	if !slices.Equal(flatten(got), wantStream(f.open)) {
		t.Fatalf("IXFR fallback: %d RRs", len(flatten(got)))
	}
}

// rawSigned sends a signed query over TCP and returns the reply unpacked
// without TSIG verification, so the test can look at error replies.
func rawSigned(t *testing.T, addr string, m *dns.Msg, key string, secret []byte, timeSigned int64) *dns.Msg {
	t.Helper()
	m.SetTsig(key, dns.HmacSHA256, 300, timeSigned)
	packed, _, err := dns.TsigGenerate(m, b64(secret), "", false)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(packed))), packed...)); err != nil { //nolint:gosec // G115: a query is far below 64 KiB
		t.Fatal(err)
	}
	var l uint16
	if err := binary.Read(conn, binary.BigEndian, &l); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, l)
	if _, err := ioReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	r := new(dns.Msg)
	if err := r.Unpack(buf); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTSIGRejections(t *testing.T) {
	f := startXFR(t)
	now := time.Now().Unix()
	for _, tc := range []struct {
		name    string
		key     string
		secret  []byte
		signed  int64
		want    uint16
		reason  string
		macSize bool
	}{
		{"unknown key", "nobody.", xfrSecret, now, dns.RcodeBadKey, TSIGBadKey, false},
		{"wrong secret", xfrKey, otherSecret, now, dns.RcodeBadSig, TSIGBadSig, false},
		{"clock skew", xfrKey, xfrSecret, now - 3600, dns.RcodeBadTime, TSIGBadTime, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.failed())
			m := new(dns.Msg)
			m.SetAxfr("example.org.")
			r := rawSigned(t, f.tcp(), m, tc.key, tc.secret, tc.signed)
			if r.Rcode != dns.RcodeNotAuth {
				t.Fatalf("rcode %s, want NOTAUTH", dns.RcodeToString[r.Rcode])
			}
			ts := r.IsTsig()
			if ts == nil || ts.Error != tc.want {
				t.Fatalf("TSIG error %v, want %d", ts, tc.want)
			}
			if (ts.MACSize > 0) != tc.macSize {
				t.Fatalf("MAC size %d: BADKEY/BADSIG replies are unsigned, BADTIME is signed", ts.MACSize)
			}
			if tc.want == dns.RcodeBadTime {
				// Other Data carries the server's clock.
				b, err := hex.DecodeString(ts.OtherData)
				if err != nil || len(b) != 6 {
					t.Fatalf("other data %q", ts.OtherData)
				}
				server := int64(binary.BigEndian.Uint64(append([]byte{0, 0}, b...))) //nolint:gosec // G115: 48-bit time
				if d := server - now; d < -5 || d > 5 {
					t.Fatalf("server time %d, now %d", server, now)
				}
				if ts.TimeSigned != uint64(tc.signed) { //nolint:gosec // G115: positive test time
					t.Fatalf("BADTIME reply time signed %d, want the request's %d", ts.TimeSigned, tc.signed)
				}
				// The reply carries a full HMAC-SHA256 MAC (miekg refuses to
				// verify any NOTAUTH reply, so the size is what is checked).
				if ts.MACSize != 32 {
					t.Fatalf("BADTIME reply MAC size %d, want 32", ts.MACSize)
				}
			}
			if got := f.failed(); len(got) != before+1 || got[len(got)-1] != tc.reason {
				t.Fatalf("failure hook saw %v, want %s", got, tc.reason)
			}
		})
	}
	// The pipeline never saw the rejected requests.
	if n := testutilCount(f.stage, "example.org.", authoritative.TransferRefused); n != 0 {
		t.Fatalf("rejected requests reached the stage: %v", n)
	}
}

func TestTSIGSignedQueryOverUDP(t *testing.T) {
	f := startXFR(t)
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second, TsigSecret: map[string]string{xfrKey: b64(xfrSecret)}}
	m := new(dns.Msg)
	m.SetQuestion("ns1.example.org.", dns.TypeA)
	m.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
	// Exchange verifies the reply's TSIG and fails if it is missing or bad.
	r, _, err := c.Exchange(m, f.udp())
	if err != nil {
		t.Fatal(err)
	}
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.IsTsig() == nil {
		t.Fatalf("signed reply %v", r)
	}
	// A NOTIFY signed with a good key is verified, then NOTIMP (NOTIFY in
	// is v0.2), and the reply is still signed.
	n := new(dns.Msg)
	n.SetNotify("example.org.")
	n.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
	r, _, err = c.Exchange(n, f.udp())
	if err != nil || r.Rcode != dns.RcodeNotImplemented || r.IsTsig() == nil {
		t.Fatalf("signed NOTIFY: %v %v", r, err)
	}
}

func TestKeyringFromConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", "  "+b64(xfrSecret)+"\n")
	k, err := KeyringFromConfig([]config.TSIGKey{{Name: "Transfer.Example", Algorithm: "hmac-sha512", SecretFile: good}})
	if err != nil {
		t.Fatal(err)
	}
	secret, alg, ok := k.Lookup("transfer.example.")
	if !ok || alg != dns.HmacSHA512 || string(secret) != string(xfrSecret) {
		t.Fatalf("lookup: %q %q %v", secret, alg, ok)
	}
	if _, _, ok := k.Lookup("other."); ok {
		t.Fatal("unknown key found")
	}
	if !slices.Equal(k.Names(), []string{"transfer.example."}) {
		t.Fatalf("names %v", k.Names())
	}
	for name, keys := range map[string][]config.TSIGKey{
		"missing file":  {{Name: "a", Algorithm: "hmac-sha256", SecretFile: filepath.Join(dir, "nope")}},
		"not base64":    {{Name: "a", Algorithm: "hmac-sha256", SecretFile: write("bad", "%%%")}},
		"bad algorithm": {{Name: "a", Algorithm: "hmac-md5", SecretFile: good}},
		"duplicate":     {{Name: "a", Algorithm: "hmac-sha256", SecretFile: good}, {Name: "A.", Algorithm: "hmac-sha256", SecretFile: good}},
		"empty secret":  {{Name: "a", Algorithm: "hmac-sha256", SecretFile: write("empty", "\n")}},
	} {
		if _, err := KeyringFromConfig(keys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTSIGProviderAlgorithmMismatch(t *testing.T) {
	p := TSIGProvider(testKeyring(t))
	ts := &dns.TSIG{Hdr: dns.RR_Header{Name: xfrKey}, Algorithm: dns.HmacSHA512}
	if _, err := p.Generate([]byte("msg"), ts); !errors.Is(err, dns.ErrKeyAlg) {
		t.Fatalf("algorithm mismatch: %v", err)
	}
	if _, err := TSIGProvider(nil).Generate([]byte("msg"), ts); !errors.Is(err, dns.ErrSecret) {
		t.Fatalf("nil keyring: %v", err)
	}
}

// testutilCount reads one series of the stage's transfer counter.
func testutilCount(s *authoritative.Stage, zoneName, result string) float64 {
	return testutil.ToFloat64(s.Metrics.Transfers.WithLabelValues(zoneName, "out", result))
}
