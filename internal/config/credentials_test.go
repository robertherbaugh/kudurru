package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestCredentialGrantsStayOnHost(t *testing.T) {
	host, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	host.Credentials = CredentialGrants{AllowedFiles: []string{"/run/secrets/oidc"}, AllowedEnvironment: []string{"PROVIDER_TOKEN"}}
	if _, exists := ClusterSeed(host)["credentials"]; exists {
		t.Fatal("host capabilities escaped into cluster seed")
	}
	for _, key := range []string{"allowedFiles", "allowedEnvironment"} {
		if _, _, err := ApplyClusterDocument(host, map[string]any{"credentials": map[string]any{key: []any{}}}); err == nil {
			t.Fatalf("cluster document changed %s", key)
		}
	}
	next, _, err := ApplyClusterDocument(host, map[string]any{"response": map[string]any{"filterAAAA": true}})
	if err != nil || !reflect.DeepEqual(next.Credentials, host.Credentials) {
		t.Fatal("cluster replacement lost local grants", err)
	}
	next.Credentials.AllowedFiles[0] = "/another"
	if host.Credentials.AllowedFiles[0] != "/run/secrets/oidc" {
		t.Fatal("prepared config aliases host grants")
	}
}

func TestCredentialGrantValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants CredentialGrants
	}{
		{"relative", CredentialGrants{AllowedFiles: []string{"secret"}}},
		{"unclean", CredentialGrants{AllowedFiles: []string{"/run/secrets/../secret"}}},
		{"longPath", CredentialGrants{AllowedFiles: []string{"/" + strings.Repeat("a", 4096)}}},
		{"manyFiles", CredentialGrants{AllowedFiles: make([]string, 257)}},
		{"manyEnvironment", CredentialGrants{AllowedEnvironment: make([]string, 257)}},
		{"emptyEnvironment", CredentialGrants{AllowedEnvironment: []string{""}}},
		{"numericEnvironment", CredentialGrants{AllowedEnvironment: []string{"1TOKEN"}}},
		{"dottedEnvironment", CredentialGrants{AllowedEnvironment: []string{"TOKEN.VALUE"}}},
		{"longEnvironment", CredentialGrants{AllowedEnvironment: []string{strings.Repeat("A", 129)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Credentials = tc.grants
			if cfg.Validate() == nil {
				t.Fatal("invalid host grant accepted")
			}
		})
	}
	if errs := (CredentialGrants{AllowedFiles: []string{"/run/secrets/oidc"}, AllowedEnvironment: []string{"_TOKEN_2"}}).validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
}
