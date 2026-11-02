package oidccore

import "testing"

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
