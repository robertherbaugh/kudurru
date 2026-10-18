package proxyproto

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
)

func seedCorpus(f *testing.F) {
	f.Add(mustHex(f, goldenINET))
	f.Add(header(f, CommandProxy, secretTLV(testSecret)))
	f.Add(header(f, CommandLocal, nil))
	v6 := Header{Command: CommandProxy, Family: FamilyINET6, Transport: TransportStream,
		Source: netip.MustParseAddrPort("[2001:db8::1]:1"), Destination: netip.MustParseAddrPort("[2001:db8::2]:2"),
		RawTLVs: AppendTLV(secretTLV(testSecret), TLVTypeNoop, nil)}
	b, _ := v6.Append(nil)
	f.Add(b)
	unix := Header{Command: CommandProxy, Family: FamilyUNIX}
	b, _ = unix.Append(nil)
	f.Add(b)
	f.Add([]byte{})
	f.Add(signature[:])
	f.Add([]byte("\x00\x1d\xab\xcd\x01\x00"))
}

// FuzzParse checks that Parse never panics, never reports more bytes than it
// was given, and that every header it accepts survives Append+Parse
// unchanged.
func FuzzParse(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, in []byte) {
		h, n, err := Parse(in)
		if err != nil {
			if !errors.Is(err, ErrNoHeader) && !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrMalformed) {
				t.Fatalf("unexpected error class: %v", err)
			}
			return
		}
		if n < fixedLen || n > len(in) || n > MaxHeaderLen {
			t.Fatalf("consumed %d of %d", n, len(in))
		}
		out, err := h.Append(nil)
		if err != nil {
			t.Fatalf("accepted header does not re-encode: %v (%+v)", err, h)
		}
		h2, n2, err := Parse(out)
		if err != nil || n2 != len(out) {
			t.Fatalf("re-encoded header does not parse: %v", err)
		}
		if h2.Command != h.Command || h2.Family != h.Family || h2.Transport != h.Transport ||
			h2.Source != h.Source || h2.Destination != h.Destination || !bytes.Equal(h2.RawTLVs, h.RawTLVs) {
			t.Fatalf("round trip changed the header:\n%+v\n%+v", h, h2)
		}
	})
}

// FuzzDatagram drives the policy with arbitrary datagrams from an allowed
// and an unlisted peer: it must never panic, and a datagram it accepts as
// proxied must have carried the exact secret.
func FuzzDatagram(f *testing.F) {
	seedCorpus(f)
	p, err := NewPolicy([]string{"10.0.0.11"}, testSecret)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, peer := range []netip.AddrPort{dnsdistA, neighbour} {
			payload, client, proxied, err := p.Datagram(peer, bytes.Clone(in))
			if err != nil {
				continue
			}
			if !HasSignature(in) {
				if !bytes.Equal(payload, in) || client != peer || proxied {
					t.Fatal("datagram without a header must pass through untouched")
				}
				continue
			}
			if peer != dnsdistA {
				t.Fatal("header from an unlisted peer was accepted")
			}
			h, _, perr := Parse(in)
			if perr != nil {
				t.Fatalf("accepted an unparsable header: %v", perr)
			}
			if h.Command == CommandLocal {
				if client != peer || proxied {
					t.Fatal("LOCAL must retain the transport identity")
				}
				continue
			}
			if v, ok := h.TLV(TLVTypeSecret); !ok || !bytes.Equal(v, testSecret) {
				t.Fatal("accepted a header without the secret")
			}
		}
	})
}

// FuzzStream feeds arbitrary connection prefixes through Stream in 1-byte
// reads; plain streams must be replayed exactly.
func FuzzStream(f *testing.F) {
	seedCorpus(f)
	p, err := NewPolicy([]string{"10.0.0.11"}, testSecret)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		r := &chunkReader{b: bytes.Clone(in), n: 1}
		consumed, _, proxied, err := p.Stream(dnsdistA, r)
		if err != nil {
			return
		}
		if !proxied && len(consumed) > 0 {
			replayed := append(consumed, r.b...)
			if !bytes.Equal(replayed, in) && !HasSignature(in) {
				t.Fatalf("plain stream not replayed: %q vs %q", replayed, in)
			}
		}
	})
}
