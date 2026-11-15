package validator

import (
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestNotifyOptionsSchema(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name, key, source string
		valid             bool
	}{
		{"inherit", "", "", true},
		{"different-key-ipv4", "Notify_Key.Example.", "127.0.0.2", true},
		{"ipv6", "notify", "::1", true},
		{"private-ipv6", "notify", "fd00::1", true},
		{"bad-key", "a..b", "", false},
		{"maximum-key", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + ".", "", true},
		{"long-key", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62), "", false},
		{"long-label", strings.Repeat("k", 64), "", false},
		{"root-key", ".", "", false},
		{"key-space", " notify.", "", false},
		{"hostname", "", "localhost", false},
		{"port", "", "127.0.0.1:53", false},
		{"scope", "", "fe80::1%lo", false},
		{"mapped", "", "::ffff:127.0.0.1", false},
		{"unspecified-v4", "", "0.0.0.0", false},
		{"unspecified-v6", "", "::", false},
		{"multicast-v4", "", "224.0.0.1", false},
		{"multicast-v6", "", "ff02::1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := typedSample(t, "DNSZone").(*public.DNSZone)
			z.Spec.Policy.NotifyTSIGKeyRef = tc.key
			z.Spec.Policy.NotifySource = tc.source
			err := v.Validate(ctx(), z, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, validation error=%v", tc.valid, err)
			}
			if tc.valid && (z.Spec.Policy.NotifyTSIGKeyRef != tc.key || z.Spec.Policy.NotifySource != tc.source) {
				t.Fatal("new NOTIFY policy was pruned or rewritten")
			}
		})
	}
}
