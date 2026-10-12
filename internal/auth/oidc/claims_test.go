package oidc

import (
	"reflect"
	"strings"
	"testing"
)

func mustClaims(t *testing.T, js string) Claims {
	t.Helper()
	c, err := DecodeClaims([]byte(js))
	if err != nil {
		t.Fatalf("decode %s: %v", js, err)
	}
	return c
}

func validConfig() ProviderConfig {
	return ProviderConfig{
		ID:           "corp",
		Issuer:       "https://idp.example",
		ClientID:     "kudurru",
		ClientSecret: "s3cret",
		RedirectURL:  "https://dns.example/api/v1/auth/oidc/corp/callback",
	}
}

func TestEvaluateUsernameFallback(t *testing.T) {
	cases := []struct {
		name      string
		claim     string // configured username claim
		prefix    string
		claims    string
		want      string
		wantClaim string
	}{
		{"preferred_username first", "", "", `{"preferred_username":"alice","upn":"a@corp","email":"a@x","email_verified":true}`, "alice", "preferred_username"},
		{"upn when no preferred_username", "", "", `{"upn":"alice@corp.example","email":"a@x","email_verified":true}`, "alice@corp.example", "upn"},
		{"verified email next", "", "", `{"email":"alice@x.example","email_verified":true}`, "alice@x.example", "email"},
		{"verified email as string", "", "", `{"email":"alice@x.example","email_verified":"true"}`, "alice@x.example", "email"},
		{"unverified email skipped for sub", "", "", `{"email":"alice@x.example","email_verified":false}`, "sub-123", "sub"},
		{"missing email_verified skipped for sub", "", "", `{"email":"alice@x.example"}`, "sub-123", "sub"},
		{"sub last", "", "", `{}`, "sub-123", "sub"},
		{"configured claim wins", "nickname", "", `{"nickname":"ally","preferred_username":"alice"}`, "ally", "nickname"},
		{"configured claim missing falls back", "nickname", "", `{"preferred_username":"alice"}`, "alice", "preferred_username"},
		{"non-string claim skipped", "", "", `{"preferred_username":42,"upn":"alice@corp"}`, "alice@corp", "upn"},
		{"control characters skipped", "", "", `{"preferred_username":"ali\nce"}`, "sub-123", "sub"},
		{"prefix applied", "", "corp:", `{"preferred_username":"alice"}`, "corp:alice", "preferred_username"},
		{"configured email still needs verification", "email", "", `{"email":"alice@x.example","email_verified":false,"preferred_username":"alice"}`, "alice", "preferred_username"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ClaimMappings.Username = PrefixedClaim{Claim: c.claim, Prefix: c.prefix}
			if err := cfg.Validate(nil); err != nil {
				t.Fatal(err)
			}
			id, lerr := cfg.Evaluate("https://idp.example", "sub-123", mustClaims(t, c.claims))
			if lerr != nil {
				t.Fatalf("Evaluate: %v", lerr)
			}
			if id.Username != c.want || id.UsernameClaim != c.wantClaim {
				t.Errorf("username = %q from %q, want %q from %q", id.Username, id.UsernameClaim, c.want, c.wantClaim)
			}
		})
	}
}

func TestEvaluateGroupsShapes(t *testing.T) {
	cases := []struct {
		name   string
		claim  string
		prefix string
		claims string
		want   []string
	}{
		{"array", "", "", `{"groups":["b","a"]}`, []string{"a", "b"}},
		{"single string", "", "", `{"groups":"dns-admins"}`, []string{"dns-admins"}},
		{"roles merged", "", "", `{"groups":["a"],"roles":["r1","a"]}`, []string{"a", "r1"}},
		{"roles as string", "", "", `{"roles":"Kudurru.Admin"}`, []string{"Kudurru.Admin"}},
		{"non-strings ignored", "", "", `{"groups":["a",1,null,{"x":1},"", true]}`, []string{"a"}},
		{"object ignored", "", "", `{"groups":{"a":true}}`, nil},
		{"absent", "", "", `{}`, nil},
		{"prefix on both claims", "", "corp:", `{"groups":["/dns-admins"],"roles":["ops"]}`, []string{"corp:/dns-admins", "corp:ops"}},
		{"custom claim", "memberOf", "", `{"memberOf":["x"],"groups":["ignored"]}`, []string{"x"}},
		{"custom claim is roles", "roles", "", `{"roles":["x"]}`, []string{"x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ClaimMappings.Groups = PrefixedClaim{Claim: c.claim, Prefix: c.prefix}
			if err := cfg.Validate(nil); err != nil {
				t.Fatal(err)
			}
			id, lerr := cfg.Evaluate("https://idp.example", "s", mustClaims(t, c.claims))
			if lerr != nil {
				t.Fatal(lerr)
			}
			if !reflect.DeepEqual(id.Groups, c.want) {
				t.Errorf("groups = %#v, want %#v", id.Groups, c.want)
			}
		})
	}
}

func TestEvaluateClaimValidationRules(t *testing.T) {
	rules := []ClaimValidationRule{
		{Claim: "hd", RequiredValue: "example.com"},
		{Claim: "email_verified", RequiredValue: "true"},
		{Claim: "tier", RequiredValue: "3"},
	}
	cases := []struct {
		name   string
		claims string
		ok     bool
	}{
		{"all hold", `{"hd":"example.com","email_verified":true,"tier":3}`, true},
		{"bool as string", `{"hd":"example.com","email_verified":"true","tier":3}`, true},
		{"number as string", `{"hd":"example.com","email_verified":true,"tier":"3"}`, true},
		{"wrong value", `{"hd":"evil.com","email_verified":true,"tier":3}`, false},
		{"missing claim", `{"email_verified":true,"tier":3}`, false},
		{"array never equals", `{"hd":["example.com"],"email_verified":true,"tier":3}`, false},
		{"false bool", `{"hd":"example.com","email_verified":false,"tier":3}`, false},
		{"float differs", `{"hd":"example.com","email_verified":true,"tier":3.0}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ClaimValidationRules = rules
			_ = cfg.Validate(nil)
			_, lerr := cfg.Evaluate("https://idp.example", "s", mustClaims(t, c.claims))
			if (lerr == nil) != c.ok {
				t.Fatalf("Evaluate err = %v, want ok=%v", lerr, c.ok)
			}
			if lerr != nil && (lerr.Code != CodeClaimRule || strings.Contains(lerr.Message, "evil")) {
				t.Errorf("error leaks or wrong code: %+v", lerr)
			}
		})
	}
}

func TestEvaluateEmailAndDisplayName(t *testing.T) {
	cfg := validConfig()
	_ = cfg.Validate(nil)
	id, _ := cfg.Evaluate("https://idp.example", "s", mustClaims(t, `{"email":"a@x","email_verified":true,"name":"Alice A"}`))
	if id.Email != "a@x" || id.DisplayName != "Alice A" {
		t.Errorf("verified: %+v", id)
	}
	id, _ = cfg.Evaluate("https://idp.example", "s", mustClaims(t, `{"email":"a@x","name":"Alice A"}`))
	if id.Email != "" {
		t.Errorf("unverified email kept: %q", id.Email)
	}
	if id.Key != (IdentityKey{Provider: "corp", Issuer: "https://idp.example", Subject: "s"}) {
		t.Errorf("key = %+v", id.Key)
	}
}

func TestEvaluateDistributedGroups(t *testing.T) {
	cfg := validConfig()
	_ = cfg.Validate(nil)
	overage := `{"_claim_names":{"groups":"src1"},"_claim_sources":{"src1":{"endpoint":"https://graph.example/getMemberObjects"}}}`
	_, lerr := cfg.Evaluate("https://idp.example", "s", mustClaims(t, overage))
	if lerr == nil || lerr.Code != CodeDistributedClaim {
		t.Fatalf("overage: %v", lerr)
	}
	// Groups present alongside _claim_names: use them.
	both := `{"groups":["a"],"_claim_names":{"groups":"src1"}}`
	if id, lerr := cfg.Evaluate("https://idp.example", "s", mustClaims(t, both)); lerr != nil || len(id.Groups) != 1 {
		t.Fatalf("groups + _claim_names: %v %v", id.Groups, lerr)
	}
}

func TestEvaluateNoUsername(t *testing.T) {
	cfg := validConfig()
	_ = cfg.Validate(nil)
	if _, lerr := cfg.Evaluate("https://idp.example", "", mustClaims(t, `{}`)); lerr == nil || lerr.Code != CodeClaims {
		t.Fatalf("no username: %v", lerr)
	}
}

func TestCheckAudience(t *testing.T) {
	cases := []struct {
		name   string
		aud    []string
		claims string
		ok     bool
	}{
		{"single aud no azp", []string{"kudurru"}, `{}`, true},
		{"single aud matching azp", []string{"kudurru"}, `{"azp":"kudurru"}`, true},
		{"single aud foreign azp", []string{"kudurru"}, `{"azp":"other"}`, false},
		{"multi aud no azp", []string{"kudurru", "other"}, `{}`, false},
		{"multi aud azp ours", []string{"kudurru", "other"}, `{"azp":"kudurru"}`, true},
		{"multi aud azp theirs", []string{"kudurru", "other"}, `{"azp":"other"}`, false},
	}
	for _, c := range cases {
		if lerr := checkAudience(c.aud, mustClaims(t, c.claims), "kudurru"); (lerr == nil) != c.ok {
			t.Errorf("%s: %v", c.name, lerr)
		}
	}
}

func TestSanitizeIDPError(t *testing.T) {
	if got := sanitizeIDPError("access_denied"); got != "access_denied" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeIDPError("<script>\n"); strings.ContainsAny(got, "<>\n") {
		t.Errorf("got %q", got)
	}
	if got := sanitizeIDPError(strings.Repeat("a", 200)); len(got) != 64 {
		t.Errorf("len %d", len(got))
	}
}
