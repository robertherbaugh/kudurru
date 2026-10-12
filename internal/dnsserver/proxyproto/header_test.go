package proxyproto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// goldenINET is a PROXY/INET/DGRAM header from 192.0.2.10:53000 to
// 10.0.0.11:5353 with TLV 0xE0 = "abcd", assembled by hand from the
// specification (section 2.2).
const goldenINET = "0d0a0d0a000d0a515549540a" + // signature
	"21" + // version 2, command PROXY
	"12" + // AF_INET, DGRAM
	"0013" + // 12 address bytes + 7 TLV bytes
	"c000020a" + "0a00000b" + // 192.0.2.10, 10.0.0.11
	"cf08" + "14e9" + // 53000, 5353
	"e0000461626364" // TLV 0xE0 len 4 "abcd"

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenINET(t *testing.T) {
	want := mustHex(t, goldenINET)
	h := Header{
		Command:     CommandProxy,
		Family:      FamilyINET,
		Transport:   TransportDatagram,
		Source:      netip.MustParseAddrPort("192.0.2.10:53000"),
		Destination: netip.MustParseAddrPort("10.0.0.11:5353"),
		RawTLVs:     AppendTLV(nil, TLVTypeSecret, []byte("abcd")),
	}
	got, err := h.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Append mismatch\n got %x\nwant %x", got, want)
	}

	payload := []byte("dns-payload")
	parsed, n, err := Parse(append(want, payload...))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) {
		t.Fatalf("consumed %d bytes, want %d", n, len(want))
	}
	if parsed.Command != CommandProxy || parsed.Family != FamilyINET || parsed.Transport != TransportDatagram {
		t.Fatalf("wrong header fields: %+v", parsed)
	}
	if parsed.Source != h.Source || parsed.Destination != h.Destination {
		t.Fatalf("wrong addresses: %s -> %s", parsed.Source, parsed.Destination)
	}
	if v, ok := parsed.TLV(TLVTypeSecret); !ok || string(v) != "abcd" {
		t.Fatalf("TLV 0xE0 = %q, %v", v, ok)
	}
	if _, ok := parsed.TLV(TLVTypeALPN); ok {
		t.Fatal("unexpected ALPN TLV")
	}
}

func TestRoundTrip(t *testing.T) {
	tlvs := AppendTLV(AppendTLV(nil, TLVTypeALPN, []byte("dot")), TLVTypeSecret, bytes.Repeat([]byte{'k'}, 64))
	for _, h := range []Header{
		{Command: CommandProxy, Family: FamilyINET, Transport: TransportStream, Source: netip.MustParseAddrPort("198.51.100.7:1"), Destination: netip.MustParseAddrPort("10.0.0.1:53")},
		{Command: CommandProxy, Family: FamilyINET6, Transport: TransportDatagram, Source: netip.MustParseAddrPort("[2001:db8::7]:4242"), Destination: netip.MustParseAddrPort("[fd00::53]:5353"), RawTLVs: tlvs},
		{Command: CommandLocal, Family: FamilyUnspec},
		{Command: CommandLocal, Family: FamilyINET, Source: netip.MustParseAddrPort("127.0.0.1:1"), Destination: netip.MustParseAddrPort("127.0.0.1:2")},
		{Command: CommandProxy, Family: FamilyUNIX, Transport: TransportStream, RawTLVs: tlvs},
	} {
		b, err := h.Append(nil)
		if err != nil {
			t.Fatalf("%+v: %v", h, err)
		}
		got, n, err := Parse(b)
		if err != nil {
			t.Fatalf("%+v: %v", h, err)
		}
		if n != len(b) {
			t.Fatalf("consumed %d of %d", n, len(b))
		}
		if got.Command != h.Command || got.Family != h.Family || got.Transport != h.Transport ||
			got.Source != h.Source || got.Destination != h.Destination || !bytes.Equal(got.RawTLVs, h.RawTLVs) {
			t.Fatalf("round trip changed the header\n got %+v\nwant %+v", got, h)
		}
	}
}

func TestParseErrors(t *testing.T) {
	valid := mustHex(t, goldenINET)
	mutate := func(f func(b []byte) []byte) []byte {
		return f(bytes.Clone(valid))
	}
	tests := []struct {
		name string
		in   []byte
		want error
		msg  string
	}{
		{"empty", nil, ErrTruncated, ""},
		{"dns message", mustHex(t, "abcd01000001000000000000076578616d706c6503636f6d0000010001"), ErrNoHeader, ""},
		{"tls client hello", []byte{0x16, 0x03, 0x01, 0x02, 0x00}, ErrNoHeader, ""},
		{"partial signature", valid[:7], ErrTruncated, ""},
		{"signature only", valid[:12], ErrTruncated, ""},
		{"v1 text header", []byte("PROXY TCP4 192.0.2.1 192.0.2.2 1 2\r\n"), ErrNoHeader, ""},
		{"version 1 nibble", mutate(func(b []byte) []byte { b[12] = 0x11; return b }), ErrMalformed, "version"},
		{"version 3 nibble", mutate(func(b []byte) []byte { b[12] = 0x31; return b }), ErrMalformed, "version"},
		{"command 2", mutate(func(b []byte) []byte { b[12] = 0x22; return b }), ErrMalformed, "command"},
		{"family 4", mutate(func(b []byte) []byte { b[13] = 0x42; return b }), ErrMalformed, "family"},
		{"transport 3", mutate(func(b []byte) []byte { b[13] = 0x13; return b }), ErrMalformed, "transport"},
		{"declared longer than data", mutate(func(b []byte) []byte { b[15]++; return b }), ErrTruncated, ""},
		{"too long", mutate(func(b []byte) []byte { b[14], b[15] = 0xFF, 0xFF; return b }), ErrMalformed, "exceeds"},
		{"address block short", mutate(func(b []byte) []byte { b[14], b[15] = 0, 8; return b[:16+8] }), ErrMalformed, "address block"},
		{"inet6 block short", mutate(func(b []byte) []byte { b[13] = 0x22; return b }), ErrMalformed, "address block"},
		{"tlv overruns", mutate(func(b []byte) []byte { b[len(b)-5] = 0x09; return b }), ErrMalformed, "TLV"},
		{"trailing bytes", mutate(func(b []byte) []byte { b[15] -= 5; return b[:len(b)-5] }), ErrMalformed, "trailing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Parse(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Parse error = %v, want %v", err, tt.want)
			}
			if tt.msg != "" && !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("error %q should mention %q", err, tt.msg)
			}
		})
	}
}

func TestAppendRejects(t *testing.T) {
	v4 := netip.MustParseAddrPort("192.0.2.1:1")
	v6 := netip.MustParseAddrPort("[2001:db8::1]:1")
	for name, h := range map[string]Header{
		"bad command":    {Command: 7},
		"bad transport":  {Command: CommandProxy, Transport: 9},
		"bad family":     {Command: CommandProxy, Family: 9},
		"inet with v6":   {Command: CommandProxy, Family: FamilyINET, Source: v6, Destination: v6},
		"inet6 with v4":  {Command: CommandProxy, Family: FamilyINET6, Source: v4, Destination: v4},
		"tlvs too large": {Command: CommandProxy, RawTLVs: make([]byte, MaxHeaderLen)},
	} {
		if _, err := h.Append(nil); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: Append error = %v, want ErrMalformed", name, err)
		}
	}
}

func TestHasSignature(t *testing.T) {
	valid := mustHex(t, goldenINET)
	if !HasSignature(valid) || HasSignature(valid[:11]) || HasSignature([]byte("hello world!")) {
		t.Fatal("HasSignature is wrong")
	}
}

func BenchmarkParse(b *testing.B) {
	in := append(mustHex(b, goldenINET), make([]byte, 40)...)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := Parse(in); err != nil {
			b.Fatal(err)
		}
	}
}
