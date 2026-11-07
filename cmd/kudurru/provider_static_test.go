package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func providerStaticFixture(t *testing.T) providerStaticOptions {
	t.Helper()
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(trust, providerTestCA(t, 1, true), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.ProviderResources.SystemTrust = false
	cfg.Auth.ProviderResources.CAFiles = []string{trust}
	cfg.Auth.ProviderResources.AllowedOrigins = []string{"https://issuer.example"}
	loaded := keys.Keys{Current: bytes.Repeat([]byte{1}, 32), Previous: bytes.Repeat([]byte{2}, 32)}
	owner, err := newAuthKeyOwner(t.Context(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	// Loaded slices do not remain a mutable keyring capability.
	loaded.Current[0], loaded.Previous[0] = 9, 9
	return providerStaticOptions{Config: cfg, Keys: owner, Namespace: "default"}
}

func TestProviderStaticLoadsOnlyPreparedTrustAndOriginScopedCapabilities(t *testing.T) {
	o := providerStaticFixture(t)
	secret := filepath.Join(t.TempDir(), "client-secret") // deliberately absent
	o.Config.Auth.IdentityProviders = []map[string]any{{
		"id": "corp", "issuer": "https://issuer.example", "clientID": "dns", "redirectURL": "https://dns.example/api/v1/auth/oidc/corp/callback", "clientSecretFile": secret,
		"claimMappings": map[string]any{"username": map[string]any{"claim": "sub"}},
	}}
	o.Config.RBAC.Bindings = []map[string]any{{"role": "viewer", "subjects": []any{map[string]any{"kind": "Group", "name": "operators"}}}}
	input, err := loadProviderStatic(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Release()
	if len(input.LocalProviders) != 1 || input.LocalProviders[0].ID != "corp" || input.Auth.CurrentKey[0] != 1 || input.Auth.PreviousKey[0] != 2 || input.Auth.Hasher == nil || input.Auth.TOTPKeys == nil || input.Auth.Permissions == nil {
		t.Fatal("actual immutable auth capabilities were not prepared")
	}
	if input.Auth.License.Providers || input.Auth.License.RBAC || len(input.Auth.ProviderResources) != 0 || len(input.Auth.ConfigProviders) != 0 {
		t.Fatal("static preparation fabricated license, registry or provider authority")
	}
	prepared, err := providerhost.PrepareStatic(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Release()
	if !prepared.Current() {
		t.Fatal("prepared static owner is not current")
	}
	ref := public.CredentialReference{File: secret}
	if _, err := input.ObjectCredentials.Resolve(t.Context(), ref); !errors.Is(err, credentials.ErrDenied) {
		t.Fatal("stored provider inherited config-only file grant", err)
	}
	if _, err := input.ConfigCredentials["corp"].Resolve(t.Context(), ref); !errors.Is(err, credentials.ErrUnavailable) {
		t.Fatal("explicit config grant did not defer missing secret acquisition", err)
	}
	if err := os.WriteFile(secret, []byte("test-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := input.ConfigCredentials["corp"].Resolve(t.Context(), ref); err != nil || value.Fingerprint == "" || len(value.Bytes) == 0 {
		t.Fatal("config-only credential could not be acquired", err)
	}
	o.Config.Auth.IdentityProviders[0]["issuer"] = "https://changed.example"
	o.Config.Auth.ProviderResources.AllowedOrigins[0] = "https://changed.example"
	o.Config.RBAC.Bindings[0]["role"] = "admin"
	if input.LocalProviders[0].Spec.Issuer != "https://issuer.example" || input.AllowedOrigins[0] != "https://issuer.example" || input.Auth.StaticBindings[0].Role != "viewer" {
		t.Fatal("mutable host config changed prepared static capabilities")
	}
	if err := os.WriteFile(o.Config.Auth.ProviderResources.CAFiles[0], []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !prepared.Current() || !bytes.Contains(input.TrustPEM, []byte("BEGIN CERTIFICATE")) {
		t.Fatal("held static trust reentered its file")
	}
}

func TestAuthKeyPinExcludesRotationThroughActualCommand(t *testing.T) {
	o := providerStaticFixture(t)
	input, err := loadProviderStatic(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Release()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-keys.db"), sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- o.Keys.withCurrent(t.Context(), func() error {
			return st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test/auth-key", Data: []byte(`{}`)}); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(context.Context) error {
					close(entered)
					<-release
					if !input.Current() {
						return providerhost.ErrStale
					}
					return nil
				})
			})
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("command did not reach final check")
	}
	next := keys.Keys{Current: bytes.Repeat([]byte{3}, 32)}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := o.Keys.replace(ctx, next); !errors.Is(err, context.DeadlineExceeded) || !input.Current() {
		t.Fatal("auth keys replaced before actual commit", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := o.Keys.replace(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if input.Current() {
		t.Fatal("old static keys survived replacement")
	}
	if err := o.Keys.replace(t.Context(), keys.Keys{Current: bytes.Repeat([]byte{1}, 32), Previous: bytes.Repeat([]byte{2}, 32)}); err != nil {
		t.Fatal(err)
	}
	if input.Current() {
		t.Fatal("A to B to A revived old static resources")
	}
}

type providerMarshalTrap struct{}

func (providerMarshalTrap) MarshalJSON() ([]byte, error) { panic("unbounded custom marshaler ran") }

func TestProviderStaticBoundsBeforeMarshalAndRejectsInvalidInputs(t *testing.T) {
	for _, mutate := range []func(*providerStaticOptions){
		func(o *providerStaticOptions) { o.Config.Auth.IdentityProviders = make([]map[string]any, 65) },
		func(o *providerStaticOptions) {
			o.Config.Auth.IdentityProviders = []map[string]any{{"id": strings.Repeat("a", 4097)}}
		},
		func(o *providerStaticOptions) {
			o.Config.Auth.IdentityProviders = []map[string]any{{"id": providerMarshalTrap{}}}
		},
		func(o *providerStaticOptions) {
			m := map[string]any{}
			m["loop"] = m
			o.Config.Auth.IdentityProviders = []map[string]any{m}
		},
		func(o *providerStaticOptions) {
			o.Config.Auth.ProviderResources.AllowedOrigins = []string{"http://remote.example"}
		},
	} {
		o := providerStaticFixture(t)
		mutate(&o)
		if _, err := loadProviderStatic(t.Context(), o); err == nil {
			t.Fatal("malformed or unbounded input accepted")
		}
	}
	o := providerStaticFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := loadProviderStatic(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preparation accepted", err)
	}
	o.Config.Auth.IdentityProviders = []map[string]any{{"id": "corp", "clientSecret": "private-input-do-not-echo"}}
	if _, err := loadProviderStatic(t.Context(), o); err == nil || strings.Contains(err.Error(), "private-input-do-not-echo") {
		t.Fatal("invalid credential field accepted or echoed")
	}
}
