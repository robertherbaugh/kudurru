package oidc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
)

func TestCredentialFilesNeedDelegationAndErrorsAreRedacted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("private-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	idp := &internalv1.IdentityProvider{Spec: internalv1.IdentityProviderSpec{ClientSecretFile: file}}
	idp.Name = "corp"
	if _, err := FromIdentityProvider(t.Context(), idp, nil); err == nil {
		t.Fatal("object acquired filesystem access without delegated resolver")
	}
	pc, err := FromIdentityProvider(t.Context(), idp, nil, func(_ context.Context, path string) (string, error) {
		if path != file {
			t.Fatal("wrong capability requested")
		}
		return "\n delegated-value \n", nil
	})
	if err != nil || pc.ClientSecret != "delegated-value" {
		t.Fatal("delegated file failed", err)
	}
	leaking := errors.New("private-value")
	_, err = FromIdentityProvider(t.Context(), idp, nil, func(context.Context, string) (string, error) { return "private-value", leaking })
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatal("file resolver error not redacted")
	}
	idp.Spec.ClientSecretFile = ""
	idp.Spec.ClientSecretRef = &internalv1.SecretKeyRef{Name: "client", Key: "secret"}
	_, err = FromIdentityProvider(t.Context(), idp, func(context.Context, string, string) (string, error) { return "private-value", leaking })
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatal("Secret resolver error not redacted")
	}
}
