package dnsdist

import (
	"testing"
	"time"
)

func TestAuthoritativeOriginalProofAndLegacyBypass(t *testing.T) {
	o := resolverFixture(time.Now())
	st := o.results[0].zones.Resolver
	for _, reason := range []string{"", "zone-runtime", "zone-unavailable", "zone-expiry", "signed-zone", "zone-client-view", "zone-client-acl", "untrusted-console-text"} {
		st.AuthoritativeAvailable = false
		st.AuthoritativeReason = reason
		key, mode := authoritativeTarget(o, "validated-original-key", "qualified", true)
		want := "bypass-authoritative-proof"
		if reason != "" && reason != "untrusted-console-text" {
			want = "bypass-" + reason
		}
		if key != "" || mode != want {
			t.Fatal("missing proof or remote reason granted authority", reason)
		}
	}
	st.AuthoritativeAvailable = true
	if key, mode := authoritativeTarget(o, "validated-original-key", "qualified", true); key != "validated-original-key" || mode != "qualified" {
		t.Fatal("explicit original proof did not qualify")
	}
	for _, mode := range []string{"bypass-stale", "bypass-invalid", "bypass-zone-client-acl", "bypass-zone-client-view", "bypass-client-scope", "bypass-unavailable", "bypass-source"} {
		if key, got := authoritativeTarget(o, "", mode, true); key != "" || got != mode {
			t.Fatal("failed original proof was repaired")
		}
	}
	if key, mode := authoritativeTarget(o, "validated-original-key", "qualified", false); key != "" || mode != "disabled" {
		t.Fatal("remote source enabled an operator-disabled pool")
	}
}
