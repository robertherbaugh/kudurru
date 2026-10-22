package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/ee/oidc"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

// authKeyOwner is separate from TSIG. All bytes are loaded and validated before
// installation; publication/provider/host guards precede its replacement gate.
// A replacement needs a new prepared provider static capture before it can serve.
type authKeyOwner struct {
	owner snapshotowner.Owner[authKeyMaterial]
}
type authKeyMaterial struct {
	current, previous []byte
	totp              *totp.Keyring
	fingerprintID     string
}

func newAuthKeyOwner(ctx context.Context, loaded keys.Keys) (*authKeyOwner, error) {
	o := new(authKeyOwner)
	if err := o.replace(ctx, loaded); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *authKeyOwner) replace(ctx context.Context, loaded keys.Keys) error {
	if o == nil || ctx == nil {
		return providerhost.ErrInvalid
	}
	factor, err := totp.NewKeyring(loaded.Current, loaded.Previous)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(loaded.Current)
	value := &authKeyMaterial{current: slices.Clone(loaded.Current), previous: slices.Clone(loaded.Previous), totp: factor, fingerprintID: hex.EncodeToString(digest[:])}
	// Deliberate replacement always installs, including A -> A and A -> B -> A.
	return o.owner.Publish(ctx, value, func(*authKeyMaterial, *authKeyMaterial) bool { return false })
}

func (o *authKeyOwner) withCurrent(ctx context.Context, fn func() error) error {
	if o == nil || ctx == nil || fn == nil {
		return providerhost.ErrInvalid
	}
	return o.owner.WithCurrent(ctx, o.owner.Capture(), fn)
}

// providerStaticOptions contains original host-only configuration and actual
// borrowed credential capabilities. Secret is nil on SQLite; no Secret namespace
// or filesystem delegation is inferred from a stored IdentityProvider object.
type providerStaticOptions struct {
	Config    *config.Config
	Keys      *authKeyOwner
	Namespace string
	Secret    func(context.Context, string, string, string) ([]byte, error)
}

// loadProviderStatic performs startup/preparation I/O before any publication or
// command gate. The caller passes the result to providerhost.PrepareStatic and
// retains LocalProviders as the complete primary config reconciliation manifest.
// No provider discovery, client-secret read or license decision occurs here.
func loadProviderStatic(ctx context.Context, o providerStaticOptions) (providerhost.StaticInput, error) {
	if ctx == nil || o.Config == nil || o.Keys == nil {
		return providerhost.StaticInput{}, providerhost.ErrInvalid
	}
	cfg := o.Config
	if len(cfg.Auth.IdentityProviders) > providerregistry.MaxActiveSources || len(cfg.RBAC.Bindings) > 1024 {
		return providerhost.StaticInput{}, providerhost.ErrBounds
	}
	// ParseConfigEntry and ParseBindings marshal their map input. Reject large,
	// cyclic or custom values before either parser can allocate/execute them.
	budget := 0
	for _, value := range []any{cfg.Auth.IdentityProviders, cfg.RBAC.Bindings, cfg.Credentials, cfg.Auth.ProviderResources} {
		if err := providerConfigBudget(reflect.ValueOf(value), 0, &budget); err != nil {
			return providerhost.StaticInput{}, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return providerhost.StaticInput{}, errors.New("provider host: invalid configuration")
	}
	trust, err := loadProviderTrust(ctx, cfg.Auth.ProviderResources)
	if err != nil {
		return providerhost.StaticInput{}, err
	}
	cut := o.Keys.owner.Capture()
	if !cut.Current() || cut.Value() == nil {
		return providerhost.StaticInput{}, providerhost.ErrStale
	}
	material := cut.Value()
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		return providerhost.StaticInput{}, err
	}
	bindings, err := rbac.ParseBindings(cfg.RBAC.Bindings)
	if err != nil {
		return providerhost.StaticInput{}, errors.New("provider host: invalid static role bindings")
	}
	if _, err := rbac.NewBindingAuthorizer(permissions, bindings); err != nil {
		return providerhost.StaticInput{}, err
	}
	grants := credentials.Options{Files: slices.Clone(cfg.Credentials.AllowedFiles), Environment: slices.Clone(cfg.Credentials.AllowedEnvironment), Namespace: o.Namespace, Secret: o.Secret, FingerprintKey: material.current}
	objects, err := credentials.New(grants)
	if err != nil {
		return providerhost.StaticInput{}, err
	}
	local := make([]providerregistry.DesiredConfig, 0, len(cfg.Auth.IdentityProviders))
	resolvers := make(map[string]*credentials.Resolver, len(cfg.Auth.IdentityProviders))
	configFiles := make(map[string]string)
	for i, input := range cfg.Auth.IdentityProviders {
		idp, err := oidc.ParseConfigEntry(input)
		if err != nil {
			// Parser errors can quote input. Do not copy operator-provided
			// credential-like values into startup diagnostics.
			return providerhost.StaticInput{}, fmt.Errorf("auth.identityProviders[%d]: invalid provider configuration", i)
		}
		if _, duplicate := resolvers[idp.Name]; duplicate {
			return providerhost.StaticInput{}, errors.New("auth.identityProviders: duplicate provider ID")
		}
		personal := grants
		personal.Files = slices.Clone(grants.Files)
		if ref := idp.Spec.ClientSecretRef; ref != nil && ref.File != "" {
			path, err := filepath.Abs(ref.File)
			if err != nil || len(path) > 4096 {
				return providerhost.StaticInput{}, errors.New("auth.identityProviders: invalid configured credential path")
			}
			ref.File = path
			personal.Files = append(personal.Files, path)
			configFiles[idp.Name] = path
		}
		resolver, err := credentials.New(personal)
		if err != nil {
			return providerhost.StaticInput{}, err
		}
		resolvers[idp.Name] = resolver
		local = append(local, providerregistry.DesiredConfig{ID: idp.Name, Spec: idp.Spec})
	}
	policy, err := json.Marshal(struct {
		Grants    config.CredentialGrants
		Namespace string
		Secrets   bool
		Files     map[string]string
	}{cfg.Credentials, o.Namespace, o.Secret != nil, configFiles})
	if err != nil {
		return providerhost.StaticInput{}, err
	}
	digest := sha256.Sum256(policy)
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	var released atomic.Bool
	input := providerhost.StaticInput{
		Identity: "provider-static/v1/" + hex.EncodeToString(nonce[:]), LocalProviders: local,
		Auth:              servingauth.Dependencies{Permissions: permissions, StaticBindings: bindings, CurrentKey: slices.Clone(material.current), PreviousKey: slices.Clone(material.previous), Hasher: users.NewHasher(0), TOTPKeys: material.totp},
		ObjectCredentials: objects, ConfigCredentials: resolvers,
		CredentialPolicy: hex.EncodeToString(digest[:]), FingerprintKeyID: material.fingerprintID, AuthKeyIdentity: cut.Identity(),
		AllowedOrigins: append([]string{}, cfg.Auth.ProviderResources.AllowedOrigins...), LocalOrigins: append([]string{}, cfg.Auth.ProviderResources.LocalOrigins...),
		Algorithms: slices.Clone(cfg.Auth.ProviderResources.Algorithms), TrustPEM: trust,
		Current: func() bool { return !released.Load() && cut.Current() }, Release: func() { released.Store(true) },
	}
	if err := ctx.Err(); err != nil {
		input.Release()
		return providerhost.StaticInput{}, err
	}
	if !input.Current() {
		return providerhost.StaticInput{}, providerhost.ErrStale
	}
	return input, nil
}

func providerConfigBudget(v reflect.Value, depth int, total *int) error {
	*total += 32
	if depth > 12 || *total > 1<<20 {
		return providerhost.ErrBounds
	}
	if !v.IsValid() {
		return nil
	}
	if v.CanInterface() {
		if _, custom := v.Interface().(json.Marshaler); custom {
			return providerhost.ErrInvalid
		}
		if _, custom := v.Interface().(encoding.TextMarshaler); custom {
			return providerhost.ErrInvalid
		}
	}
	switch v.Kind() {
	case reflect.Interface:
		if !v.IsNil() {
			return providerConfigBudget(v.Elem(), depth+1, total)
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String || v.Len() > 1024 {
			return providerhost.ErrBounds
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := providerConfigBudget(iter.Key(), depth+1, total); err != nil {
				return err
			}
			if err := providerConfigBudget(iter.Value(), depth+1, total); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Len() > 1024 {
			return providerhost.ErrBounds
		}
		for i := 0; i < v.Len(); i++ {
			if err := providerConfigBudget(v.Index(i), depth+1, total); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := providerConfigBudget(v.Field(i), depth+1, total); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.Len() > 4096 {
			return providerhost.ErrBounds
		}
		*total += 6 * v.Len()
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
	default:
		return providerhost.ErrInvalid
	}
	if *total > 1<<20 {
		return providerhost.ErrBounds
	}
	return nil
}
