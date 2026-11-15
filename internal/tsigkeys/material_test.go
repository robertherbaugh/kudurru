package tsigkeys

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

func testSealer(t *testing.T, current, previous byte) *Sealer {
	t.Helper()
	var prev []byte
	if previous != 0 {
		prev = bytes.Repeat([]byte{previous}, 32)
	}
	s, err := NewSealer(bytes.Repeat([]byte{current}, 32), prev)
	if err != nil {
		t.Fatal("prepare encryption fixture")
	}
	return s
}

func testMaterial(t *testing.T, cluster string) *Material {
	t.Helper()
	m, err := NewMaterial(Owner{ClusterID: cluster, UID: "original-owner", KeyName: "updates.example.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal("prepare credential fixture")
	}
	return m
}

func TestEncryptedMaterialOwnerRotationAndReseal(t *testing.T) {
	original := testSealer(t, 0x11, 0)
	m := testMaterial(t, "cluster")
	record, err := original.Seal(m)
	if err != nil {
		t.Fatal("seal fixture")
	}
	plain := m.Key().Secret
	raw, _ := json.Marshal(record)
	for _, encoding := range []string{string(plain), base64.StdEncoding.EncodeToString(plain)} {
		if bytes.Contains(raw, []byte(encoding)) {
			t.Fatal("plaintext escaped encrypted carrier")
		}
		if strings.Contains(fmt.Sprintf("%v %#v", m, m), encoding) {
			t.Fatal("diagnostic material disclosure")
		}
	}
	redacted, _ := json.Marshal(m)
	if string(redacted) != `{"private":true}` {
		t.Fatal("material serialized")
	}
	rotated := testSealer(t, 0x22, 0x11)
	opened, previous, err := rotated.Open(record)
	if err != nil || !previous || !bytes.Equal(opened.Key().Secret, plain) {
		t.Fatal("previous encryption key unavailable")
	}
	sealed, err := rotated.Seal(opened)
	if err != nil || sealed.Spec.Sealed == record.Spec.Sealed || sealed.Spec.CredentialEpoch != record.Spec.CredentialEpoch {
		t.Fatal("reseal changed credential identity")
	}
	if _, previous, err := testSealer(t, 0x22, 0).Open(sealed); err != nil || previous {
		t.Fatal("resealed current key unavailable")
	}
	if _, _, err := testSealer(t, 0x22, 0).Open(record); err == nil {
		t.Fatal("unknown encryption key adopted")
	}
	for _, mutate := range []func(*kinds.TSIGKey){
		func(k *kinds.TSIGKey) { k.UID = "replacement" },
		func(k *kinds.TSIGKey) { k.Spec.ClusterID = "other" },
		func(k *kinds.TSIGKey) { k.Spec.Algorithm = dns.HmacSHA512 },
		func(k *kinds.TSIGKey) { k.Spec.KeyName = "other.example."; k.Name = kinds.ObjectName(k.Spec.KeyName) },
		func(k *kinds.TSIGKey) { k.Spec.CredentialEpoch = strings.Repeat("a", 32) },
		func(k *kinds.TSIGKey) { k.Spec.Sealed = k.Spec.Sealed[:len(k.Spec.Sealed)-4] + "AAAA" },
	} {
		changed := record.DeepCopy()
		mutate(changed)
		if _, _, err := original.Open(changed); err == nil {
			t.Fatal("changed owner or ciphertext accepted")
		}
	}
	// Even a holder of this master key cannot transplant another purpose's envelope.
	other, _ := session.NewSealer(session.PurposeSession, bytes.Repeat([]byte{0x11}, 32), nil)
	changed := record.DeepCopy()
	changed.Spec.Sealed = other.Seal([]byte(`{"formatVersion":1}`))
	if _, _, err := original.Open(changed); err == nil {
		t.Fatal("session-purpose envelope accepted")
	}
	// Independently creating the same key bytes is credential rotation, not resealing.
	again := testMaterial(t, "cluster")
	if again.epoch == m.epoch {
		t.Fatal("identical key bytes revived credential epoch")
	}
	input := again.Key().Secret
	input[0] ^= 1
	if !bytes.Equal(again.Key().Secret, plain) {
		t.Fatal("provider input mutated stored owner")
	}
}

func TestAuthenticatedPayloadStrictnessAndBounds(t *testing.T) {
	s := testSealer(t, 0x11, 0)
	m := testMaterial(t, "cluster")
	k, err := s.Seal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"formatVersion":2}`),
		[]byte(`{"formatVersion":1,"unknown":true}`),
		append(bytes.Repeat([]byte{' '}, maxPlaintextBytes), []byte(`{}`)...),
	} {
		bad := k.DeepCopy()
		bad.Spec.Sealed = s.seal.Seal(raw)
		if _, _, err := s.Open(bad); err == nil {
			t.Fatal("unqualified payload accepted")
		}
	}
	for _, n := range []int{0, MaxSecretBytes + 1} {
		if _, err := NewMaterial(m.owner, make([]byte, n)); err == nil {
			t.Fatal("invalid secret bound accepted")
		}
	}
	bad := k.DeepCopy()
	bad.Spec.Sealed = strings.Repeat("A", kinds.MaxSealedBytes+1)
	if _, _, err := s.Open(bad); err == nil {
		t.Fatal("oversized carrier accepted")
	}
}
