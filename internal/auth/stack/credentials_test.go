package stack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func credentialProvider(file string) *internalv1.IdentityProvider {
	return &internalv1.IdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "corp"},
		Spec: internalv1.IdentityProviderSpec{
			Issuer: "https://sso.example", ClientID: "kudurru", ClientSecretRef: &publicv1.CredentialReference{File: file},
			RedirectURL: "https://dns.example/api/v1/auth/oidc/corp/callback",
		},
	}
}

func TestStoredProviderRequiresExactHostGrant(t *testing.T) {
	dir := t.TempDir()
	file, sibling := filepath.Join(dir, "allowed"), filepath.Join(dir, "sibling")
	for _, path := range []string{file, sibling} {
		if err := os.WriteFile(path, []byte("private-test-value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			yaml := testConfig
			if allowed {
				yaml += fmt.Sprintf("\ncredentials:\n  allowedFiles: [%q]\n", file)
			}
			e := newEnv(t, authtest.Memory(t), yaml)
			for _, path := range []string{file, sibling} {
				p, err := e.s.buildProvider(e.ctx, credentialProvider(path), false)
				want := allowed && path == file
				if (err == nil) != want {
					t.Fatalf("grant=%v, sibling=%v: success=%v", allowed, path == sibling, err == nil)
				}
				if want && p.Config().ClientSecret != "private-test-value" {
					t.Fatal("granted credential was not loaded")
				}
				if err != nil && strings.Contains(err.Error(), "private-test-value") {
					t.Fatal("credential leaked in error")
				}
			}
		})
	}
}

func TestStoredProviderEnvironmentReferenceUsesHostGrant(t *testing.T) {
	t.Setenv("IAM_TEST_CREDENTIAL", "private-test-value")
	obj := credentialProvider("")
	obj.Spec.ClientSecretRef = &publicv1.CredentialReference{Env: "IAM_TEST_CREDENTIAL"}
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			yaml := testConfig
			if allowed {
				yaml += "\ncredentials:\n  allowedEnvironment: [IAM_TEST_CREDENTIAL]\n"
			}
			e := newEnv(t, authtest.Memory(t), yaml)
			p, err := e.s.buildProvider(e.ctx, obj, false)
			if (err == nil) != allowed {
				t.Fatal("environment capability did not follow host grant")
			}
			if allowed && p.Config().ClientSecret != "private-test-value" {
				t.Fatal("granted credential not loaded")
			}
		})
	}
}

func TestConfiguredProviderKeepsNameAndSeparateFileCapability(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		file := filepath.Join(t.TempDir(), "configured-secret")
		if err := os.WriteFile(file, []byte("private-config-value"), 0o600); err != nil {
			t.Fatal(err)
		}
		obj := credentialProvider("")
		obj.Spec.Issuer = "https://different.example"
		obj.Spec.ClientSecretRef = &publicv1.CredentialReference{SecretRef: &publicv1.CredentialSecretRef{Name: "stored", Key: "secret"}}
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: "test"}); err != nil {
			t.Fatal(err)
		}
		yaml := fmt.Sprintf("auth:\n  identityProviders:\n  - {id: corp, issuer: https://sso.example, clientID: kudurru, clientSecretFile: %q, redirectURL: https://dns.example/api/v1/auth/oidc/corp/callback}\n", file)
		e := newEnv(t, st, yaml)
		p, ok := e.s.registry.Load().Provider("corp")
		if !ok || p.Config().Issuer != "https://sso.example" {
			t.Fatal("configured provider did not own name")
		}
		if _, err := e.s.buildProvider(e.ctx, credentialProvider(file), false); err == nil {
			t.Fatal("stored object inherited configured provider file capability")
		}
		// The configured provider's file disappears during reload. A same-name
		// valid stored provider must not become a replacement trust authority.
		e.s.regMu.Lock()
		e.s.secrets = func(_ context.Context, _, _ string) (string, error) { return "stored-secret", nil }
		e.s.regMu.Unlock()
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if err := e.s.rebuildRegistry(e.ctx, true); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.s.registry.Load().Provider("corp"); ok {
			t.Fatal("failed configured provider fell back to stored trust")
		}
		if _, err := e.s.buildProvider(e.ctx, obj, false); err != nil {
			t.Fatal("stored provider fixture was not independently valid", err)
		}
		for _, diagnostic := range e.s.Diagnoses() {
			if strings.Contains(diagnostic, "private-config-value") || strings.Contains(diagnostic, "stored-secret") {
				t.Fatal("diagnostic leaked credential")
			}
		}
	})
}
