package proxyproto

import (
	"bytes"
	"testing"
)

func TestClientIdentityRequiresPeerSecretAndOneValidTLV(t *testing.T) {
	policy, err := NewPolicy([]string{dnsdistA.Addr().String()}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id                       string
		duplicate, noSecret, untrusted bool
	}{
		{name: "valid", id: "Phone"}, {name: "empty"}, {name: "extra label", id: "x.phone"},
		{name: "duplicate", id: "phone", duplicate: true}, {name: "no secret", id: "phone", noSecret: true}, {name: "untrusted", id: "phone", untrusted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tlv := secretTLV(testSecret)
			if tc.noSecret {
				tlv = nil
			}
			tlv = AppendTLV(tlv, TLVTypeClientID, []byte(tc.id))
			if tc.duplicate {
				tlv = AppendTLV(tlv, TLVTypeClientID, []byte(tc.id))
			}
			wire := header(t, CommandProxy, tlv)
			peer := dnsdistA
			if tc.untrusted {
				peer = neighbour
			}
			for _, stream := range []bool{false, true} {
				id := ""
				capture := func(v string) { id = v }
				if stream {
					_, _, _, err = policy.Stream(peer, bytes.NewReader(wire), capture)
				} else {
					_, _, _, err = policy.Datagram(peer, wire, capture)
				}
				if tc.name == "valid" {
					if err != nil || id != "phone" {
						t.Fatalf("ID %q err %v", id, err)
					}
				} else if err == nil || id != "" {
					t.Fatalf("accepted identity %q: %v", id, err)
				}
			}
		})
	}
	loopback, err := NewPolicy([]string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := header(t, CommandProxy, AppendTLV(nil, TLVTypeClientID, []byte("phone")))
	if _, _, _, err := loopback.Datagram(loopbackV4, wire); err == nil {
		t.Fatal("loopback address exception granted unauthenticated ID propagation")
	}
}
