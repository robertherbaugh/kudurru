package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestHostCapabilitiesAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("first-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	envCalls, secretCalls := 0, 0
	secret := []byte("secret-test-token")
	r, err := New(Options{Files: []string{path}, Environment: []string{"PROVIDER_TOKEN"}, Namespace: "resolver", FingerprintKey: bytes.Repeat([]byte{42}, 32),
		Getenv: func(_ string) (string, bool) { envCalls++; return "env-test-token", true },
		Secret: func(_ context.Context, ns, name, key string) ([]byte, error) {
			secretCalls++
			if ns != "resolver" || name != "provider" || key != "token" {
				t.Fatal("reference changed")
			}
			return secret, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []v1alpha1.CredentialReference{{File: "/etc/shadow"}, {File: dir + "/other/../token"}, {Env: "UNGRANTED_TOKEN"}, {File: path, Env: "PROVIDER_TOKEN"}, {SecretRef: &v1alpha1.CredentialSecretRef{Name: "../provider", Key: "token"}}, {}} {
		if _, err := r.Resolve(t.Context(), ref); !errors.Is(err, ErrDenied) {
			t.Fatalf("ungranted reference: %v", err)
		}
	}
	if envCalls != 0 || secretCalls != 0 {
		t.Fatal("ungranted reference performed I/O")
	}
	first, err := r.Resolve(t.Context(), v1alpha1.CredentialReference{File: path})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := r.Resolve(t.Context(), v1alpha1.CredentialReference{File: path})
	if first.Fingerprint != again.Fingerprint {
		t.Fatal("unchanged credential fingerprint changed")
	}
	if err := os.WriteFile(path, []byte("rotated-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	rotated, err := r.Resolve(t.Context(), v1alpha1.CredentialReference{File: path})
	if err != nil || first.Fingerprint == rotated.Fingerprint {
		t.Fatal("rotation not detected")
	}
	resolved, err := r.Resolve(t.Context(), v1alpha1.CredentialReference{SecretRef: &v1alpha1.CredentialSecretRef{Name: "provider", Key: "token"}})
	if err != nil {
		t.Fatal(err)
	}
	resolved.Bytes[0] = 'X'
	if string(secret) != "secret-test-token" {
		t.Fatal("resolver exposed provider-owned mutable bytes")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Resolve(ctx, v1alpha1.CredentialReference{Env: "PROVIDER_TOKEN"}); !errors.Is(err, context.Canceled) || envCalls != 0 {
		t.Fatal("canceled resolution performed I/O")
	}
}

func TestBoundedSecretsAndErrorRedaction(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("  \n"), bytes.Repeat([]byte{'x'}, MaxBytes+1)} {
		r, _ := New(Options{Environment: []string{"TOKEN"}, Getenv: func(string) (string, bool) { return string(data), true }, FingerprintKey: bytes.Repeat([]byte{42}, 32)})
		if _, err := r.Resolve(t.Context(), v1alpha1.CredentialReference{Env: "TOKEN"}); !errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid credential accepted")
		}
	}
	cause := errors.New("upstream response contains private-test-value")
	r, _ := New(Options{Namespace: "resolver", Secret: func(context.Context, string, string, string) ([]byte, error) {
		return nil, cause
	}, FingerprintKey: bytes.Repeat([]byte{42}, 32)})
	_, err := r.Resolve(t.Context(), v1alpha1.CredentialReference{SecretRef: &v1alpha1.CredentialSecretRef{Name: "provider", Key: "token"}})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
		t.Fatal("secret provider error lost its retry classification")
	}
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, err), "private-test-value") {
			t.Fatal("secret provider error leaked")
		}
	}
}
