package config

import (
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

func TestDNSSECSettings(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DNSSEC.Enabled || !cfg.DNSSEC.UseBuiltinAnchors {
		t.Fatalf("validation must be opt-in with shipped trust anchors available: %+v", cfg.DNSSEC)
	}
	env, err := Load("", []string{"KUDURRU_DNSSEC__ENABLED=true"})
	if err != nil || !env.DNSSEC.Enabled {
		t.Fatalf("environment override: %v", err)
	}
	if _, err := Load("", []string{"KUDURRU_DNSSEC__USEBUILTINANCHORS=false"}); err == nil {
		t.Fatal("removing all trust anchors must be rejected, including when globally disabled")
	}
}

func TestDNSSECTrustAndExceptionValidation(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	nta := NegativeTrustAnchor{Domain: "broken.example.", Reason: "upstream rollover incident", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	for _, tc := range []struct {
		name string
		cfg  DNSSEC
		want string
	}{
		{"built-in", DNSSEC{UseBuiltinAnchors: true}, ""},
		{"private", DNSSEC{TrustAnchors: []string{dnssec.RootAnchors()[0].String()}}, ""},
		{"empty", DNSSEC{}, "trust anchors required"},
		{"wrong type", DNSSEC{TrustAnchors: []string{"example. 60 IN A 192.0.2.1"}}, "DS or DNSKEY"},
		{"malformed", DNSSEC{TrustAnchors: []string{"not a record"}}, "trustAnchors[0]"},
		{"exception", DNSSEC{UseBuiltinAnchors: true, NegativeTrustAnchors: []NegativeTrustAnchor{nta}}, ""},
		{"missing reason", DNSSEC{UseBuiltinAnchors: true, NegativeTrustAnchors: []NegativeTrustAnchor{{Domain: nta.Domain, Reason: "  ", ExpiresAt: nta.ExpiresAt}}}, "reason"},
		{"relative expiry", DNSSEC{UseBuiltinAnchors: true, NegativeTrustAnchors: []NegativeTrustAnchor{{Domain: nta.Domain, Reason: nta.Reason, ExpiresAt: "1h"}}}, "RFC3339"},
		{"too long", DNSSEC{UseBuiltinAnchors: true, NegativeTrustAnchors: []NegativeTrustAnchor{{Domain: nta.Domain, Reason: nta.Reason, ExpiresAt: now.Add(8 * 24 * time.Hour).Format(time.RFC3339)}}}, "7 days"},
		{"duplicate", DNSSEC{UseBuiltinAnchors: true, NegativeTrustAnchors: []NegativeTrustAnchor{nta, {Domain: "BROKEN.EXAMPLE", Reason: nta.Reason, ExpiresAt: nta.ExpiresAt}}}, "duplicate"},
		{"expired", DNSSEC{UseBuiltinAnchors: true, NegativeTrustAnchors: []NegativeTrustAnchor{{Domain: nta.Domain, Reason: nta.Reason, ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339)}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Validator(clock)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("Validator() = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestDNSSECSettingsRoundTripDoesNotRenewException(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(time.Hour).Format(time.RFC3339)
	flat := Defaults()
	flat["dnssec.enabled"] = true
	flat["dnssec.negativeTrustAnchors"] = []map[string]any{{"domain": "example.", "reason": "repair pending", "expiresAt": expiry}}
	cfg, err := FromFlat(flat)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromFlat(Flatten(cfg))
	if err != nil || len(back.DNSSEC.NegativeTrustAnchors) != 1 || back.DNSSEC.NegativeTrustAnchors[0].ExpiresAt != expiry {
		t.Fatalf("round trip lost the absolute expiry: %v", err)
	}
	v, err := back.DNSSEC.Validator(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	before := v.ActiveContextID()
	now = now.Add(2 * time.Hour)
	if before == v.ActiveContextID() {
		t.Fatal("expired exception must retire the bypass cache context without reload")
	}
}
