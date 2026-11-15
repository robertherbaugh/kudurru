package secondary

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

func TestZoneCredentialCaptureKeepsOriginalPolicyAndEpoch(t *testing.T) {
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: transferKey, Algorithm: "hmac-sha256", Secret: transferSecret, CredentialEpoch: "stored-original"})
	if err != nil {
		t.Fatal(err)
	}
	var owner snapshotowner.Owner[dnsserver.StaticKeyring]
	if err := owner.Publish(t.Context(), ring, func(a, b *dnsserver.StaticKeyring) bool { return a == b }); err != nil {
		t.Fatal(err)
	}
	ready := false
	m := &Manager{opts: Options{CaptureZoneKeys: func(input *v1alpha1.DNSZone) dnsserver.AcquisitionKeys {
		signed := input.Spec.Secondary.TSIGKeyRef != ""
		input.Spec.Secondary.TSIGKeyRef = "changed-only-detached-input"
		if signed && !ready {
			return nil
		}
		return acquisitionTestKeys{owner: &owner, cut: owner.Capture()}
	}}}
	signed := transferZone(true)
	signed.Spec.Secondary.Primaries = []string{"127.0.0.1:53"}
	originalName := signed.Spec.Secondary.TSIGKeyRef
	if _, _, err := m.captureEntry(signed); err == nil {
		t.Fatal("unknown source admitted signed entry")
	}
	if signed.Spec.Secondary.TSIGKeyRef != originalName {
		t.Fatal("selector mutated original zone policy")
	}
	unsigned := transferZone(false)
	unsigned.Spec.Secondary.Primaries = []string{"127.0.0.1:53"}
	if _, fp, err := m.captureEntry(unsigned); err != nil || fp != "unsigned" {
		t.Fatal("unrelated unsigned acquisition unavailable", err)
	}
	ready = true
	cut, fp, err := m.captureEntry(signed)
	want, ok := dnsserver.AcquisitionFingerprint(ring, transferKey)
	if err != nil || !ok || fp != want || cut.keys.Keys() != ring {
		t.Fatal("stored acquisition lost original epoch")
	}
	if cut.receiver != nil {
		t.Fatal("capture built a receiver before entry admission")
	}
}
