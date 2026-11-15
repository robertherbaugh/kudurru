package config

import (
	"strings"
	"testing"
	"time"
)

func TestRecursiveNTATypedSettingsAndRestore(t *testing.T) {
	const path = "upstreams.recursion.negativeTrustAnchors"
	expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	for _, tc := range []struct {
		name, domain, reason, expiry string
		valid                        bool
	}{
		{"active", "broken.example.", "repair", expiry.Format(time.RFC3339), true},
		{"expired", "broken.example.", "repair", expiry.Add(-2 * time.Hour).Format(time.RFC3339), true},
		{"relative", "broken.example.", "repair", "1h", false},
		{"zero", "broken.example.", "repair", "0001-01-01T00:00:00Z", false},
		{"too far", "broken.example.", "repair", expiry.Add(8 * 24 * time.Hour).Format(time.RFC3339), false},
		{"domain", "bad domain", "repair", expiry.Format(time.RFC3339), false},
		{"reason", "broken.example.", " ", expiry.Format(time.RFC3339), false},
		{"oversized reason", "broken.example.", strings.Repeat("x", 257), expiry.Format(time.RFC3339), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Defaults()
			flat[path] = []map[string]any{{"domain": tc.domain, "reason": tc.reason, "expiresAt": tc.expiry}}
			cfg, err := FromFlat(flat)
			if (err == nil) != tc.valid {
				t.Fatalf("settings admission mismatch: accepted=%t", err == nil)
			}
			if err != nil {
				return
			}
			restored, err := FromFlat(Flatten(cfg))
			if err != nil || len(restored.Upstreams.Recursion.NegativeTrustAnchors) != 1 {
				t.Fatal("typed exception did not survive settings restore")
			}
			got := restored.Upstreams.Recursion.NegativeTrustAnchors[0].ExpiresAt
			want, _ := time.Parse(time.RFC3339, tc.expiry)
			if !got.Equal(want) || len(restored.DNSSEC.NegativeTrustAnchors) != 0 {
				t.Fatal("restore renewed expiry or changed forwarded exceptions")
			}
		})
	}
	for _, count := range []int{2, 129} {
		flat := Defaults()
		entries := make([]map[string]any, count)
		for i := range entries {
			entries[i] = map[string]any{"domain": "broken.example.", "reason": "repair", "expiresAt": expiry.Format(time.RFC3339)}
		}
		flat[path] = entries
		if _, err := FromFlat(flat); err == nil {
			t.Fatal("duplicate or oversized exception set was accepted")
		}
	}
}
