package dnsserver

import "testing"

func TestAcquisitionFingerprintLegacyAndStoredEpochs(t *testing.T) {
	const name = "transfer.test."
	makeRing := func(epoch string) *StaticKeyring {
		t.Helper()
		keys, err := NewStaticKeyring(TSIGKey{Name: name, Algorithm: TSIGHMACSHA256, Secret: []byte{1, 2, 3}, CredentialEpoch: epoch})
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	legacy, ok := AcquisitionFingerprint(makeRing(""), name)
	// This vector is the digest already persisted by secondary/RPZ. It must
	// stay exact so file-backed retained copies remain usable after restart.
	if !ok || legacy != "21358b2a9688d2b91c174f4d7cab064e7982bd7c9c2792b3a0c224c3b0852b49" {
		t.Fatal("legacy durable acquisition fingerprint changed")
	}
	a, aOK := AcquisitionFingerprint(makeRing("epoch-a"), "TRANSFER.TEST")
	b, bOK := AcquisitionFingerprint(makeRing("epoch-b"), name)
	again, againOK := AcquisitionFingerprint(makeRing("epoch-a"), name)
	if !aOK || !bOK || !againOK || a == b || a == legacy || a != again {
		t.Fatal("stored acquisition provenance lost credential epoch or stable identity")
	}
	if _, ok := AcquisitionFingerprint(makeRing("epoch-a"), "missing.test."); ok {
		t.Fatal("missing credential supplied acquisition provenance")
	}
	if _, ok := AcquisitionFingerprint(nil, name); ok {
		t.Fatal("missing provider supplied acquisition provenance")
	}
}
