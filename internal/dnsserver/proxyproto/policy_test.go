package proxyproto

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// testSecret is 64 printable bytes, the `openssl rand -hex 32` shape.
var testSecret = []byte(strings.Repeat("0123456789abcdef", 4))

var (
	realClient = netip.MustParseAddrPort("203.0.113.7:5555")
	listenAddr = netip.MustParseAddrPort("10.0.0.53:5353")
	dnsdistA   = netip.MustParseAddrPort("10.0.0.11:40000")
	neighbour  = netip.MustParseAddrPort("10.0.0.99:40000")
	loopbackV4 = netip.MustParseAddrPort("127.0.0.1:40000")
	loopbackV6 = netip.MustParseAddrPort("[::1]:40000")
	loopbackB  = netip.MustParseAddrPort("127.0.0.2:40000")
)

// header builds a PROXY header for realClient with the given TLVs.
func header(t testing.TB, cmd Command, tlvs []byte) []byte {
	t.Helper()
	h := Header{Command: cmd, Family: FamilyINET, Transport: TransportDatagram, Source: realClient, Destination: listenAddr, RawTLVs: tlvs}
	b, err := h.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func secretTLV(v []byte) []byte { return AppendTLV(nil, TLVTypeSecret, v) }

func TestNewPolicy(t *testing.T) {
	tests := []struct {
		name    string
		from    []string
		secret  []byte
		wantErr string
		trusted []netip.Addr
	}{
		{name: "disabled", from: nil},
		{name: "loopback without secret", from: []string{"127.0.0.1/32", "::1/128"}, trusted: []netip.Addr{loopbackV4.Addr(), loopbackV6.Addr()}},
		{name: "bare addresses", from: []string{"10.0.0.11", " 10.0.0.12 "}, secret: testSecret, trusted: []netip.Addr{dnsdistA.Addr(), netip.MustParseAddr("10.0.0.12")}},
		{name: "mapped address is unmapped", from: []string{"::ffff:10.0.0.11"}, secret: testSecret, trusted: []netip.Addr{dnsdistA.Addr()}},
		{name: "LAN /24 refused", from: []string{"10.0.0.0/24"}, secret: testSecret, wantErr: "only exact hosts"},
		{name: "pod CIDR refused", from: []string{"fd00::/64"}, secret: testSecret, wantErr: "only exact hosts"},
		{name: "non-loopback needs secret", from: []string{"10.0.0.11/32"}, wantErr: "require a shared secret"},
		{name: "127.0.0.2 is not exempt", from: []string{"127.0.0.2/32"}, wantErr: "require a shared secret"},
		{name: "short secret", from: []string{"10.0.0.11"}, secret: []byte("short"), wantErr: "need at least 32"},
		{name: "garbage entry", from: []string{"dnsdist"}, secret: testSecret, wantErr: "proxyProtocolFrom entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewPolicy(tt.from, tt.secret)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewPolicy error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Enabled() != (len(tt.from) > 0) {
				t.Fatalf("Enabled() = %v", p.Enabled())
			}
			for _, a := range tt.trusted {
				if !p.Trusted(a) {
					t.Errorf("%s should be trusted", a)
				}
			}
			if p.Trusted(neighbour.Addr()) {
				t.Error("neighbour must not be trusted")
			}
		})
	}
}

// TestDatagramMatrix is the forgery matrix from the spike brief: only an
// allowed peer with the right TLV (or loopback without a secret) is
// honoured; everything else is dropped and counted under its reason.
func TestDatagramMatrix(t *testing.T) {
	withSecret, err := NewPolicy([]string{"10.0.0.11/32", "127.0.0.1/32", "127.0.0.2/32"}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	loopbackOnly, err := NewPolicy([]string{"127.0.0.1/32", "::1/128"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("\x12\x34query")
	wrong := bytes.Clone(testSecret)
	wrong[len(wrong)-1] ^= 1
	malformed := header(t, CommandProxy, secretTLV(testSecret))
	malformed[12] = 0x31 // version 3

	tests := []struct {
		name        string
		policy      *Policy
		peer        netip.AddrPort
		in          []byte
		wantDrop    Reason
		wantClient  netip.AddrPort
		wantProxied bool
	}{
		{name: "allowed peer with right TLV is honoured", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, secretTLV(testSecret)), wantClient: realClient, wantProxied: true},
		{name: "secret among other TLVs", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, append(AppendTLV(nil, TLVTypeALPN, []byte("h2")), secretTLV(testSecret)...)), wantClient: realClient, wantProxied: true},
		{name: "wrong TLV dropped", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, secretTLV(wrong)), wantDrop: ReasonUnauthenticated},
		{name: "truncated TLV value dropped", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, secretTLV(testSecret[:32])), wantDrop: ReasonUnauthenticated},
		{name: "secret in another TLV type dropped", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, AppendTLV(nil, 0xE1, testSecret)), wantDrop: ReasonUnauthenticated},
		{name: "missing TLV dropped", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, nil), wantDrop: ReasonUnauthenticated},
		{name: "disallowed peer with right TLV dropped", policy: withSecret, peer: neighbour, in: header(t, CommandProxy, secretTLV(testSecret)), wantDrop: ReasonUntrustedPeer},
		{name: "malformed header dropped", policy: withSecret, peer: dnsdistA, in: malformed, wantDrop: ReasonMalformed},
		{name: "truncated header dropped", policy: withSecret, peer: dnsdistA, in: header(t, CommandProxy, secretTLV(testSecret))[:30], wantDrop: ReasonMalformed},
		{name: "loopback needs TLV once a secret is set", policy: withSecret, peer: loopbackV4, in: header(t, CommandProxy, nil), wantDrop: ReasonUnauthenticated},
		{name: "loopback with TLV and secret", policy: withSecret, peer: loopbackV4, in: header(t, CommandProxy, secretTLV(testSecret)), wantClient: realClient, wantProxied: true},
		{name: "127.0.0.2 with right TLV", policy: withSecret, peer: loopbackB, in: header(t, CommandProxy, secretTLV(testSecret)), wantClient: realClient, wantProxied: true},
		{name: "loopback without secret accepted", policy: loopbackOnly, peer: loopbackV4, in: header(t, CommandProxy, nil), wantClient: realClient, wantProxied: true},
		{name: "::1 without secret accepted", policy: loopbackOnly, peer: loopbackV6, in: header(t, CommandProxy, nil), wantClient: realClient, wantProxied: true},
		{name: "unlisted loopback alias dropped", policy: loopbackOnly, peer: loopbackB, in: header(t, CommandProxy, nil), wantDrop: ReasonUntrustedPeer},
		{name: "LOCAL keeps the peer", policy: withSecret, peer: dnsdistA, in: header(t, CommandLocal, secretTLV(testSecret)), wantClient: dnsdistA},
		{name: "LOCAL still needs the TLV", policy: withSecret, peer: dnsdistA, in: header(t, CommandLocal, nil), wantDrop: ReasonUnauthenticated},
		{name: "no header from allowed peer is plain", policy: withSecret, peer: dnsdistA, in: nil, wantClient: dnsdistA},
		{name: "no header from anyone is plain", policy: withSecret, peer: neighbour, in: nil, wantClient: neighbour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := map[Reason]uint64{}
			for _, r := range Reasons {
				before[r] = tt.policy.Dropped(r)
			}
			in := append(bytes.Clone(tt.in), payload...)
			got, client, proxied, err := tt.policy.Datagram(tt.peer, in)
			if tt.wantDrop != "" {
				var de *DropError
				if !errors.As(err, &de) || de.Reason != tt.wantDrop {
					t.Fatalf("err = %v, want drop %s", err, tt.wantDrop)
				}
				if got != nil {
					t.Fatal("dropped datagram must not return a payload")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected drop: %v", err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("payload = %q, want %q", got, payload)
				}
				if client != tt.wantClient || proxied != tt.wantProxied {
					t.Fatalf("client = %s proxied=%v, want %s proxied=%v", client, proxied, tt.wantClient, tt.wantProxied)
				}
			}
			for _, r := range Reasons {
				want := before[r]
				if r == tt.wantDrop {
					want++
				}
				if got := tt.policy.Dropped(r); got != want {
					t.Errorf("Dropped(%s) = %d, want %d", r, got, want)
				}
			}
		})
	}
}

func TestHookAndTotals(t *testing.T) {
	p, err := NewPolicy([]string{"10.0.0.11"}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var last atomic.Value
	p.SetDropHook(func(r Reason) { calls.Add(1); last.Store(r) })
	_, _, _, _ = p.Datagram(neighbour, header(t, CommandProxy, nil))
	_, _, _, _ = p.Datagram(dnsdistA, header(t, CommandProxy, nil))
	if calls.Load() != 2 || last.Load() != ReasonUnauthenticated || p.DroppedTotal() != 2 {
		t.Fatalf("hook calls=%d last=%v total=%d", calls.Load(), last.Load(), p.DroppedTotal())
	}
	p.SetDropHook(nil)
	_, _, _, _ = p.Datagram(neighbour, header(t, CommandProxy, nil))
	if calls.Load() != 2 || p.DroppedTotal() != 3 {
		t.Fatal("hook should be removed but counting must continue")
	}

	var nilPolicy *Policy
	nilPolicy.SetDropHook(func(Reason) {})
	if nilPolicy.Enabled() || nilPolicy.Trusted(loopbackV4.Addr()) || nilPolicy.DroppedTotal() != 0 {
		t.Fatal("nil policy must be disabled")
	}
	in := header(t, CommandProxy, nil)
	if got, client, _, err := nilPolicy.Datagram(neighbour, in); err != nil || !bytes.Equal(got, in) || client != neighbour {
		t.Fatal("disabled policy must not inspect datagrams")
	}
}

func TestStream(t *testing.T) {
	p, err := NewPolicy([]string{"10.0.0.11"}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	dnsStream := []byte("\x00\x1d\xab\xcd\x01\x00\x00\x01rest-of-query")

	t.Run("plain stream is replayed", func(t *testing.T) {
		r := io.MultiReader(bytes.NewReader(dnsStream[:1]), bytes.NewReader(dnsStream[1:]))
		consumed, client, proxied, err := p.Stream(neighbour, r)
		if err != nil || proxied || client != neighbour {
			t.Fatalf("err=%v proxied=%v client=%s", err, proxied, client)
		}
		rest, _ := io.ReadAll(r)
		if got := append(consumed, rest...); !bytes.Equal(got, dnsStream) {
			t.Fatalf("stream bytes lost: %q", got)
		}
	})
	t.Run("authenticated header in small pieces", func(t *testing.T) {
		in := append(header(t, CommandProxy, secretTLV(testSecret)), dnsStream...)
		r := &chunkReader{b: in, n: 3}
		consumed, client, proxied, err := p.Stream(dnsdistA, r)
		if err != nil || !proxied || client != realClient || len(consumed) != 0 {
			t.Fatalf("err=%v proxied=%v client=%s consumed=%q", err, proxied, client, consumed)
		}
		rest, _ := io.ReadAll(r)
		if !bytes.Equal(rest, dnsStream) {
			t.Fatalf("payload after header = %q", rest)
		}
	})
	for name, tc := range map[string]struct {
		peer netip.AddrPort
		in   []byte
		want Reason
	}{
		"untrusted peer":    {neighbour, header(t, CommandProxy, secretTLV(testSecret)), ReasonUntrustedPeer},
		"missing TLV":       {dnsdistA, header(t, CommandProxy, nil), ReasonUnauthenticated},
		"partial signature": {dnsdistA, header(t, CommandProxy, nil)[:6], ReasonMalformed},
		"truncated body":    {dnsdistA, header(t, CommandProxy, secretTLV(testSecret))[:20], ReasonMalformed},
		"bad version":       {dnsdistA, func() []byte { b := header(t, CommandProxy, nil); b[12] = 0x11; return b }(), ReasonMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := p.Stream(tc.peer, bytes.NewReader(tc.in))
			var de *DropError
			if !errors.As(err, &de) || de.Reason != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
	t.Run("empty stream returns EOF", func(t *testing.T) {
		if _, _, _, err := p.Stream(dnsdistA, bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v", err)
		}
	})
}

// chunkReader returns at most n bytes per Read.
type chunkReader struct {
	b []byte
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	k := min(len(p), c.n, len(c.b))
	copy(p, c.b[:k])
	c.b = c.b[k:]
	return k, nil
}

func TestLoadSecretFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, append(bytes.Clone(testSecret), '\r', '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSecretFile(good)
	if err != nil || !bytes.Equal(got, testSecret) {
		t.Fatalf("LoadSecretFile = %q, %v", got, err)
	}
	short := filepath.Join(dir, "short")
	_ = os.WriteFile(short, []byte("tooshort\n"), 0o600)
	if _, err := LoadSecretFile(short); err == nil {
		t.Fatal("short secret must be rejected")
	}
	if _, err := LoadSecretFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file must be an error")
	}
	if b, err := LoadSecretFile(""); b != nil || err != nil {
		t.Fatal("empty path means no secret")
	}
}

func BenchmarkDatagram(b *testing.B) {
	p, err := NewPolicy([]string{"10.0.0.11"}, testSecret)
	if err != nil {
		b.Fatal(err)
	}
	in := append(header(b, CommandProxy, secretTLV(testSecret)), make([]byte, 40)...)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, _, err := p.Datagram(dnsdistA, in); err != nil {
			b.Fatal(err)
		}
	}
}
