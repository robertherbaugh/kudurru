package oidccore

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func testSpec() publicv1.IdentityProviderSpec {
	return publicv1.IdentityProviderSpec{Issuer: testIssuer, ClientID: "browser", CLIClientID: "cli", Audiences: []string{"extra"}, ClientSecretRef: &publicv1.CredentialReference{Env: "OIDCCORE_TEST_SECRET"}}
}

func testPolicy(t *testing.T, spec publicv1.IdentityProviderSpec) *Policy {
	t.Helper()
	p, err := Compile(spec, "corp")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func evaluateClaims(t *testing.T, p *Policy, claims map[string]any) (Identity, error) {
	t.Helper()
	token, err := testVerifier(t).Verify(t.Context(), signRSA(t, claims), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return p.Evaluate(t.Context(), token, BearerIDToken, testNow)
}

func TestScalarPolicyDefaultsDetachedIdentityAndGroups(t *testing.T) {
	spec := testSpec()
	p := testPolicy(t, spec)
	claims := testClaims()
	claims["roles"] = []any{"admin", "ops", 7, nil}
	claims["groups"] = []any{"ops", "readers", false, ""}
	id, err := evaluateClaims(t, p, claims)
	if err != nil || id.Username != "corp:alice" || !slices.Equal(id.Groups, []string{"corp:admin", "corp:ops", "corp:readers"}) {
		t.Fatal("default mapping mismatch", err)
	}
	key, err := ParseIdentityKey(id.ID())
	if err != nil || key != (IdentityKey{"corp", testIssuer, claims["sub"].(string)}) {
		t.Fatal("changed existing identity tuple", err)
	}
	id.Groups[0] = "injected"
	copySpec := p.Spec()
	*copySpec.ClaimMappings.Username.Prefix = "injected"
	copySpec.Audiences[0] = "injected"
	copySpec.ClientSecretRef.Env = "injected"
	if p.Spec().Audiences[0] != "extra" || *p.Spec().ClaimMappings.Username.Prefix != "corp:" || p.Spec().ClientSecretRef.Env != "OIDCCORE_TEST_SECRET" {
		t.Fatal("caller changed compiled policy")
	}
	id, err = evaluateClaims(t, p, claims)
	if err != nil || id.Groups[0] != "corp:admin" {
		t.Fatal("identity output aliased policy", err)
	}
	empty := ""
	spec.ClaimMappings.Username.Prefix = &empty
	spec.ClaimMappings.Groups.Prefix = &empty
	unprefixed := testPolicy(t, spec)
	id, err = evaluateClaims(t, unprefixed, claims)
	if err != nil || id.Username != "alice" || id.Groups[0] != "admin" || p.Identity() == unprefixed.Identity() {
		t.Fatal("explicit empty prefix/default identity lost", err)
	}
}

func TestScalarFallbackVerifiedEmailDomainsAndOverage(t *testing.T) {
	spec := testSpec()
	spec.ClaimMappings.Username.Claim = "login"
	p := testPolicy(t, spec)
	claims := testClaims()
	delete(claims, "preferred_username")
	claims["login"] = ""
	claims["email"] = "alice@EXAMPLE.COM"
	claims["name"] = "Alice"
	for _, flag := range []any{nil, false, "false", true, "true"} {
		if flag == nil {
			delete(claims, "email_verified")
		} else {
			claims["email_verified"] = flag
		}
		id, err := evaluateClaims(t, p, claims)
		if err != nil {
			t.Fatal(err)
		}
		if flag == true || flag == "true" {
			if id.Email != "alice@EXAMPLE.COM" || id.Username != "corp:alice@EXAMPLE.COM" {
				t.Fatal("verified email ignored")
			}
		} else {
			if id.Email != "" || id.Username != "corp:"+claims["sub"].(string) {
				t.Fatal("unverified email admitted")
			}
		}
	}
	spec.AllowedDomains = []string{"EXAMPLE.COM", "example.com"}
	domains := testPolicy(t, spec)
	if _, err := evaluateClaims(t, domains, claims); err != nil {
		t.Fatal("normalized domain rejected", err)
	}
	for _, email := range []string{"alice@sub.example.com", "alice@other.example", "alice@@example.com", "@example.com"} {
		claims["email"] = email
		if _, err := evaluateClaims(t, domains, claims); !errors.Is(err, ErrPolicy) {
			t.Fatal("domain restriction bypassed", err)
		}
	}
	claims["email"] = "alice@example.com"
	claims["email_verified"] = false
	if _, err := evaluateClaims(t, domains, claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("unverified domain admitted", err)
	}
	claims = testClaims()
	delete(claims, "groups")
	claims["_claim_names"] = map[string]any{"groups": "remote"}
	if _, err := evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("distributed group overage accepted", err)
	}
	claims["groups"] = []string{"ops"}
	if _, err := evaluateClaims(t, p, claims); err != nil {
		t.Fatal("available local groups rejected", err)
	}
	claims["groups"] = []any{}
	claims["_claim_names"] = map[string]any{"roles": "remote"}
	if _, err := evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("distributed roles ignored", err)
	}
}

func TestScalarRuleExactValuesAndEmptyDefault(t *testing.T) {
	spec := testSpec()
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Claim: "tier", RequiredValue: "3"}, {Claim: "email_verified", RequiredValue: "true"}, {Claim: "empty"}}
	p := testPolicy(t, spec)
	for _, value := range []any{3, json.Number("3"), "3", json.Number("3.0"), true, nil, []string{"3"}} {
		claims := testClaims()
		claims["tier"] = value
		claims["email_verified"] = true
		claims["empty"] = ""
		_, err := evaluateClaims(t, p, claims)
		good := value == 3 || value == json.Number("3") || value == "3"
		if (err == nil) != good {
			t.Fatal("scalar comparison changed numeric spelling/type semantics", err)
		}
	}
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Claim: "large", RequiredValue: "18446744073709551615"}}
	claims := testClaims()
	claims["large"] = json.Number("18446744073709551615")
	if _, err := evaluateClaims(t, testPolicy(t, spec), claims); err != nil {
		t.Fatal("large exact scalar rounded", err)
	}
}

func TestAudienceModesAndTimeCannotBeBypassed(t *testing.T) {
	p := testPolicy(t, testSpec())
	v := testVerifier(t)
	for _, aud := range []any{"browser", "cli", "extra", []string{"browser", "unrelated"}, "unrelated"} {
		claims := testClaims()
		claims["aud"] = aud
		claims["nonce"] = "nonce"
		if _, ok := aud.([]string); ok {
			claims["azp"] = "cli"
		}
		token, err := v.VerifyBrowser(t.Context(), signRSA(t, claims), BrowserProof{Nonce: "nonce"}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Evaluate(t.Context(), token, BearerIDToken, testNow)
		if (err == nil) != (aud != "unrelated") {
			t.Fatal("bearer audience mismatch", err)
		}
		_, err = p.Evaluate(t.Context(), token, BrowserIDToken, testNow)
		if (err == nil) != (aud == "browser") {
			t.Fatal("browser audience broadened", err)
		}
	}
	for _, azp := range []any{nil, "unrelated", "browser"} {
		claims := testClaims()
		claims["aud"] = []string{"browser", "cli"}
		if azp != nil {
			claims["azp"] = azp
		}
		token, err := v.Verify(t.Context(), signRSA(t, claims), testNow)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Evaluate(t.Context(), token, BearerIDToken, testNow)
		if (err == nil) != (azp == "browser") {
			t.Fatal("multi-audience azp bypass", err)
		}
	}
	token, err := v.Verify(t.Context(), signRSA(t, testClaims()), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Evaluate(t.Context(), token, BrowserIDToken, testNow); !errors.Is(err, ErrBrowser) {
		t.Fatal("raw verify bypassed browser nonce", err)
	}
	if _, err = p.Evaluate(t.Context(), token, BearerIDToken, testNow.Add(time.Hour)); !errors.Is(err, ErrTime) {
		t.Fatal("held facts outlived expiry", err)
	}
	if _, err = p.Evaluate(t.Context(), token, ProofMode(0), testNow); !errors.Is(err, ErrPolicy) {
		t.Fatal("unknown proof mode accepted", err)
	}
	if _, err = p.Evaluate(t.Context(), &VerifiedToken{}, BearerIDToken, testNow); !errors.Is(err, ErrPolicy) {
		t.Fatal("fabricated verified proof accepted", err)
	}
	spec := testSpec()
	spec.Issuer = "https://new-issuer.example"
	other := testPolicy(t, spec)
	if _, err = other.Evaluate(t.Context(), token, BearerIDToken, testNow); !errors.Is(err, ErrPolicy) {
		t.Fatal("policy/facts issuer mismatch accepted", err)
	}
}

func TestCELClaimsRulesAndMappings(t *testing.T) {
	spec := testSpec()
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "claims.email_verified == true && claims.tier >= 3"}}
	spec.ClaimMappings.Username.Expression = "claims.preferred_username + '-cel'"
	spec.ClaimMappings.Groups.Expression = "claims.groups.filter(g, g.startsWith('ops'))"
	spec.ClaimMappings.DisplayName.Expression = "claims['name']"
	spec.ClaimMappings.Email.Expression = "claims.email"
	p := testPolicy(t, spec)
	claims := testClaims()
	claims["email_verified"] = true
	claims["tier"] = 3
	claims["name"] = "Alice"
	claims["email"] = "alice@example.com"
	claims["roles"] = []string{"auditor"}
	id, err := evaluateClaims(t, p, claims)
	if err != nil || id.Username != "corp:alice-cel" || id.Email != "alice@example.com" || id.DisplayName != "Alice" || !slices.Equal(id.Groups, []string{"corp:auditor", "corp:ops"}) {
		t.Fatal("CEL mapping failed", err)
	}
	claims["tier"] = 2
	if _, err = evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("false CEL rule admitted", err)
	}
	claims["tier"] = 3
	delete(claims, "name")
	if _, err = evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("CEL missing mapping silently ignored", err)
	}
	// Dynamic lookup must be runtime-typed, including every group list element.
	spec = testSpec()
	spec.ClaimMappings.Groups.Expression = "claims.groups"
	p = testPolicy(t, spec)
	for _, groups := range []any{[]any{"ops", 1}, "ops", nil, []string{""}} {
		claims = testClaims()
		claims["groups"] = groups
		if _, err = evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
			t.Fatal("wrong CEL group type accepted", err)
		}
	}
	// Expression username does not fall through to a privileged scalar default.
	spec = testSpec()
	spec.ClaimMappings.Username.Expression = "''"
	p = testPolicy(t, spec)
	if _, err = evaluateClaims(t, p, testClaims()); !errors.Is(err, ErrPolicy) {
		t.Fatal("empty expression mapping fell back", err)
	}
}

func TestCELTypesCostBoundsAndCancellation(t *testing.T) {
	for _, source := range []string{"1", "'true'", "[true]", "claims.missing == true", "claims.tier"} {
		spec := testSpec()
		spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: source}}
		p, err := Compile(spec, "corp")
		if err == nil {
			claims := testClaims()
			claims["tier"] = 3
			if _, err = evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
				t.Fatal("invalid validation result admitted", err)
			}
		}
	}
	for _, source := range []string{"host.secret == 'x'", "http.get('https://example.com')", "time.now() > 0", "claims.groups.map(x, x).all(x, x == x) +"} {
		spec := testSpec()
		spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: source}}
		if _, err := Compile(spec, "corp"); !errors.Is(err, ErrPolicy) {
			t.Fatal("invalid/host expression accepted", err)
		}
	}
	spec := testSpec()
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "claims.values.all(a, claims.values.all(b, a == a && b == b))"}}
	p := testPolicy(t, spec)
	claims := testClaims()
	values := make([]int, 128)
	claims["values"] = values
	if _, err := evaluateClaims(t, p, claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("expression runtime cost was unbounded", err)
	}
	spec.ClaimValidationRules = make([]publicv1.ClaimValidationRule, 64)
	for i := range spec.ClaimValidationRules {
		spec.ClaimValidationRules[i].Expression = "claims.values.all(x, x == x)"
	}
	p = testPolicy(t, spec)
	claims["values"] = make([]int, 1024)
	if _, err := evaluateClaims(t, p, claims); !errors.Is(err, ErrBounds) {
		t.Fatal("aggregate evaluation cost unbounded", err)
	}
	spec = testSpec()
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "[" + strings.Repeat("1,", 1023) + "1].size() > 0"}}
	if _, err := Compile(spec, "corp"); !errors.Is(err, ErrBounds) {
		t.Fatal("AST node budget not enforced", err)
	}
	spec = testSpec()
	source := "true"
	for i := 0; i < 5; i++ {
		source = "claims.groups.all(g, " + source + ")"
	}
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: source}}
	if _, err := Compile(spec, "corp"); !errors.Is(err, ErrBounds) {
		t.Fatal("nested comprehension budget not enforced", err)
	}
	p = testPolicy(t, testSpec())
	token, err := testVerifier(t).Verify(t.Context(), signRSA(t, testClaims()), testNow)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = p.Evaluate(ctx, token, BearerIDToken, testNow); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored policy cancellation", err)
	}
}

func TestCELNumericModelAndUnverifiedEmail(t *testing.T) {
	spec := testSpec()
	spec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "claims.big == 18446744073709551615u && claims.fraction == 3.5"}}
	claims := testClaims()
	claims["big"] = json.Number("18446744073709551615")
	claims["fraction"] = json.Number("3.5")
	if _, err := evaluateClaims(t, testPolicy(t, spec), claims); err != nil {
		t.Fatal("typed numeric CEL failed", err)
	}
	claims["big"] = json.Number("18446744073709551616")
	if _, err := evaluateClaims(t, testPolicy(t, spec), claims); !errors.Is(err, ErrClaims) {
		t.Fatal("integer overflow coerced to double", err)
	}
	spec = testSpec()
	spec.ClaimMappings.Username.Expression = "claims.email"
	claims = testClaims()
	claims["email"] = "alice@example.com"
	if _, err := evaluateClaims(t, testPolicy(t, spec), claims); !errors.Is(err, ErrPolicy) {
		t.Fatal("unverified email username bypassed", err)
	}
	claims["email_verified"] = true
	if _, err := evaluateClaims(t, testPolicy(t, spec), claims); err != nil {
		t.Fatal("verified expression email rejected", err)
	}
	for _, source := range []string{"claims.email.startsWith('alice') ? 'alice' : 'other'", "claims['email'].startsWith('alice') ? 'alice' : 'other'", "claims[claims.which]"} {
		spec.ClaimMappings.Username.Expression = source
		claims["which"] = "email"
		delete(claims, "email_verified")
		if _, err := evaluateClaims(t, testPolicy(t, spec), claims); !errors.Is(err, ErrPolicy) {
			t.Fatal("transformed/dynamic unverified email bypassed", err)
		}
	}
}

func TestPolicyRejectsInvalidAndOversizedPublicSpec(t *testing.T) {
	for _, change := range []func(*publicv1.IdentityProviderSpec){
		func(s *publicv1.IdentityProviderSpec) { s.ClientID = "" }, func(s *publicv1.IdentityProviderSpec) { s.Issuer = "http://foreign.example" },
		func(s *publicv1.IdentityProviderSpec) { s.ClaimValidationRules = []publicv1.ClaimValidationRule{{}} },
		func(s *publicv1.IdentityProviderSpec) {
			s.ClaimValidationRules = []publicv1.ClaimValidationRule{{Claim: "tier", Expression: "true"}}
		},
		func(s *publicv1.IdentityProviderSpec) {
			s.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "true", RequiredValue: "x"}}
		},
		func(s *publicv1.IdentityProviderSpec) {
			s.ClaimMappings.Groups.Claim = "groups"
			s.ClaimMappings.Groups.Expression = "[]"
		},
		func(s *publicv1.IdentityProviderSpec) {
			s.ClaimMappings.Email.Claim = "email"
			s.ClaimMappings.Email.Expression = "''"
		},
		func(s *publicv1.IdentityProviderSpec) { s.Provisioning.DefaultRole = "admin" }, func(s *publicv1.IdentityProviderSpec) { s.Provisioning.AutoProvision = "bad" },
		func(s *publicv1.IdentityProviderSpec) { s.AllowedDomains = []string{"*.example.com"} }, func(s *publicv1.IdentityProviderSpec) { s.ClientSecretRef.File = "/unauthorized" },
		func(s *publicv1.IdentityProviderSpec) { s.Audiences = make([]string, 65) }, func(s *publicv1.IdentityProviderSpec) {
			s.ClaimMappings.Username.Expression = strings.Repeat("x", 4097)
		},
	} {
		s := testSpec()
		change(&s)
		if _, err := Compile(s, "corp"); err == nil {
			t.Fatal("invalid public policy accepted")
		}
	}
	for _, mode := range []string{"always", "never"} {
		s := testSpec()
		s.Provisioning.AutoProvision = mode
		s.Provisioning.DefaultRole = "viewer"
		if _, err := Compile(s, "corp"); err != nil {
			t.Fatal("valid detached provisioning policy rejected", err)
		}
	}
}

func TestHeldPoliciesConcurrentAndNoVerdictCache(t *testing.T) {
	v := testVerifier(t)
	claims := testClaims()
	claims["tier"] = 3
	raw := signRSA(t, claims)
	oldSpec := testSpec()
	oldSpec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "claims.tier >= 3"}}
	old := testPolicy(t, oldSpec)
	newSpec := testSpec()
	newSpec.ClaimValidationRules = []publicv1.ClaimValidationRule{{Expression: "claims.tier >= 4"}}
	newPolicy := testPolicy(t, newSpec)
	if old.Identity() == newPolicy.Identity() {
		t.Fatal("changed compiled policy identity lost")
	}
	oldSpec.ClaimValidationRules[0].Expression = "false"
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for j := 0; j < 20; j++ {
				token, err := v.Verify(t.Context(), raw, testNow)
				if err != nil {
					t.Error(err)
					return
				}
				id, err := old.Evaluate(t.Context(), token, BearerIDToken, testNow)
				if err != nil {
					t.Error("held old policy changed", err)
					return
				}
				id.Groups[0] = "injected"
				if _, err = newPolicy.Evaluate(t.Context(), token, BearerIDToken, testNow); !errors.Is(err, ErrPolicy) {
					t.Error("new policy reused old verdict", err)
					return
				}
			}
		})
	}
	wg.Wait()
}
