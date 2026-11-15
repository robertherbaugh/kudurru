package oidccore

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestCLIExactAudienceDoesNotBroadenBearerOrBrowser(t *testing.T) {
	for _, tc := range []struct {
		name string
		aud  any
		azp  any
		ok   bool
	}{{"cli", "cli", nil, true}, {"web", "browser", nil, false}, {"extra", "extra", nil, false}, {"wrong authorized party", "cli", "browser", false}, {"multiple missing", []string{"cli", "extra"}, nil, false}, {"multiple valid", []string{"cli", "extra"}, "cli", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClaims()
			c["aud"] = tc.aud
			if tc.azp != nil {
				c["azp"] = tc.azp
			}
			v, e := testVerifier(t).Verify(t.Context(), signRSA(t, c), testNow)
			if e != nil {
				t.Fatal("signature")
			}
			p := testPolicy(t, testSpec())
			_, e = p.EvaluateCLI(t.Context(), v, testNow)
			if (e == nil) != tc.ok {
				t.Fatal("strict selected client mismatch")
			}
			if _, e = p.Evaluate(t.Context(), v, BrowserIDToken, testNow); e == nil {
				t.Fatal("CLI became browser enrollment proof")
			}
		})
	}
	s := testSpec()
	s.CLIClientID = ""
	p := testPolicy(t, s)
	v, e := testVerifier(t).Verify(t.Context(), signRSA(t, testClaims()), testNow)
	if e != nil {
		t.Fatal("signature")
	}
	if _, e = p.EvaluateCLI(t.Context(), v, testNow); e == nil {
		t.Fatal("borrowed confidential web client")
	}
	if _, e = p.Evaluate(t.Context(), v, BearerIDToken, testNow); e != nil {
		t.Fatal("changed ordinary bearer policy")
	}
}

func TestCLIDeviceHashWithoutBrowserEnrollmentMarker(t *testing.T) {
	claims := testClaims()
	claims["aud"] = "cli"
	delete(claims, "nonce")
	access := "device-test-access"
	hash := sha256.Sum256([]byte(access))
	claims["at_hash"] = base64.RawURLEncoding.EncodeToString(hash[:16])
	v := testVerifier(t)
	raw := signRSA(t, claims)
	proof, e := v.VerifyDevice(t.Context(), raw, access, testNow)
	if e != nil {
		t.Fatal("device hash")
	}
	p := testPolicy(t, testSpec())
	if _, e := p.EvaluateCLI(t.Context(), proof, testNow); e != nil {
		t.Fatal("strict device CLI audience")
	}
	if _, e := p.Evaluate(t.Context(), proof, BrowserIDToken, testNow); e == nil {
		t.Fatal("device proof became browser enrollment")
	}
	if _, e := v.VerifyDevice(t.Context(), raw, "wrong", testNow); e == nil {
		t.Fatal("device at_hash bypass")
	}
	delete(claims, "at_hash")
	if _, e := v.VerifyDevice(t.Context(), signRSA(t, claims), "", testNow); e != nil {
		t.Fatal("optional nonce/hash invented")
	}
}
