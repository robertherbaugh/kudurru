package auth

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCredentialTimeEvidenceIsOriginalAndNotSerializable(t *testing.T) {
	issued := time.Now().UTC().Truncate(time.Second)
	now := issued
	clock := func() time.Time { return now }
	p := WithCredentialTimeBounds(Principal{Kind: KindKubernetesServiceAccount}, issued, issued, issued.Add(10*time.Minute), clock)
	held := WithPrincipal(t.Context(), p)
	if _, ok := PrincipalFrom(held); !ok {
		t.Fatal("valid original evidence rejected")
	}
	wire, err := json.Marshal(p)
	if err != nil {
		t.Fatal("serialize principal")
	}
	var restored Principal
	if json.Unmarshal(wire, &restored) != nil {
		t.Fatal("decode principal")
	}
	if _, ok := PrincipalFrom(WithPrincipal(t.Context(), restored)); ok {
		t.Fatal("serialized Kubernetes principal retained credential authority")
	}
	now = issued.Add(10 * time.Minute)
	p = WithCredentialTimeBounds(p, now, now, now.Add(10*time.Minute), clock)
	if _, ok := PrincipalFrom(WithPrincipal(t.Context(), p)); !ok {
		t.Fatal("new independently verified bounds rejected")
	}
	if _, ok := PrincipalFrom(held); ok {
		t.Fatal("replacement evidence repaired held original expiry")
	}
	now = issued.Add(-time.Second)
	if _, ok := PrincipalFrom(held); ok {
		t.Fatal("held evidence accepted clock rollback")
	}
}
