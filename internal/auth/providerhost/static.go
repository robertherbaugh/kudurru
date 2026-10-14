package providerhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

const maxStaticBytes = 1 << 20

func text(s string, n int) bool {
	if s == "" || len(s) > n || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

// budget checks typed policy/map bounds before JSON cloning, including slices
// and map lengths. Only finite plain configuration values enter this helper.
func budget(v reflect.Value, depth int, n *int) error {
	*n += 32
	if depth > 12 || *n > maxStaticBytes {
		return ErrBounds
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return budget(v.Elem(), depth+1, n)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := budget(v.Field(i), depth+1, n); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Len() > 1024 {
			return ErrBounds
		}
		for i := 0; i < v.Len(); i++ {
			if err := budget(v.Index(i), depth+1, n); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.Len() > 1024 {
			return ErrBounds
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := budget(iter.Key(), depth+1, n); err != nil {
				return err
			}
			if err := budget(iter.Value(), depth+1, n); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.Len() > 4096 {
			return ErrBounds
		}
		*n += 6 * v.Len()
	}
	if *n > maxStaticBytes {
		return ErrBounds
	}
	return nil
}
func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func cloneAuth(in servingauth.Dependencies) servingauth.Dependencies {
	out := in
	out.Permissions = clone(in.Permissions)
	out.StaticBindings = clone(in.StaticBindings)
	out.CurrentKey = slices.Clone(in.CurrentKey)
	out.PreviousKey = slices.Clone(in.PreviousKey)
	out.ConfigProviders = []servingauth.ProviderSource{}
	out.ProviderResources = []servingauth.ProviderResource{}
	return out
}

// PrepareStatic validates/detaches already loaded inputs before root guards.
// Caller releases its reference after New/ReplaceStatic succeeds or on failure.
// Root must load system/operator CA PEM and actual keys before invoking it.
func PrepareStatic(ctx context.Context, in StaticInput) (*StaticResources, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, s := range []string{in.Identity, in.AuthKeyIdentity, in.CredentialPolicy, in.FingerprintKeyID} {
		if !text(s, 512) {
			return nil, ErrInvalid
		}
	}
	if in.Current == nil || in.Release == nil || !in.Current() || in.Auth.Permissions == nil || nilHandle(in.Auth.Hasher) || in.Auth.TOTPKeys == nil || in.Auth.TOTPKeys.Identity() == "" {
		return nil, ErrInvalid
	}
	if in.LocalProviders == nil || len(in.LocalProviders) > 64 || len(in.ConfigCredentials) > 64 || in.AllowedOrigins == nil || in.LocalOrigins == nil || len(in.AllowedOrigins) > 128 || len(in.LocalOrigins) > 128 || len(in.Algorithms) > 10 || len(in.TrustPEM) > providerresources.MaxTrustBytes {
		return nil, ErrBounds
	}
	// Provider resources/config and license decisions belong to the current
	// registry/license, not a prepopulated mutable auth dependency fallback.
	if len(in.Auth.ConfigProviders) != 0 || len(in.Auth.ProviderResources) != 0 {
		return nil, ErrInvalid
	}
	if _, err := session.NewSealer("providerhost-static-check", in.Auth.CurrentKey, in.Auth.PreviousKey); err != nil {
		return nil, ErrInvalid
	}
	n := 0
	if err := budget(reflect.ValueOf(in.Auth.Permissions), 0, &n); err != nil {
		return nil, err
	}
	if err := budget(reflect.ValueOf(in.Auth.StaticBindings), 0, &n); err != nil {
		return nil, err
	}
	if _, err := rbac.NewBindingAuthorizer(in.Auth.Permissions, in.Auth.StaticBindings); err != nil {
		return nil, ErrInvalid
	}
	ids := map[string]bool{}
	for _, local := range in.LocalProviders {
		if ids[local.ID] {
			return nil, ErrInvalid
		}
		ids[local.ID] = true
		if _, err := oidccore.Compile(local.Spec, local.ID); err != nil {
			return nil, ErrInvalid
		}
		if err := budget(reflect.ValueOf(local), 0, &n); err != nil {
			return nil, err
		}
	}
	for id, resolver := range in.ConfigCredentials {
		if !ids[id] || resolver == nil {
			return nil, ErrInvalid
		}
	}
	// NewHost validates concrete trust, algorithm and endpoint policy. No config
	// UID is fabricated: canonical config attachment is deferred to Host(registry).
	if _, err := providerresources.NewHost(providerresources.HostOptions{
		Identity: in.Identity, CredentialPolicy: in.CredentialPolicy, EntitlementIdentity: "provider-denied/v1", FingerprintKeyID: in.FingerprintKeyID,
		ConfigSources: []servingauth.ProviderSource{}, ObjectCredentials: in.ObjectCredentials, ConfigCredentials: in.ConfigCredentials,
		AllowedOrigins: in.AllowedOrigins, LocalOrigins: in.LocalOrigins, Algorithms: in.Algorithms, TrustPEM: in.TrustPEM,
	}); err != nil {
		return nil, err
	}
	out := in
	out.Auth = cloneAuth(in.Auth)
	out.LocalProviders = clone(in.LocalProviders)
	out.ConfigCredentials = make(map[string]*credentials.Resolver, len(in.ConfigCredentials))
	for k, v := range in.ConfigCredentials {
		out.ConfigCredentials[k] = v
	}
	out.AllowedOrigins = slices.Clone(in.AllowedOrigins)
	out.LocalOrigins = slices.Clone(in.LocalOrigins)
	out.Algorithms = slices.Clone(in.Algorithms)
	out.TrustPEM = slices.Clone(in.TrustPEM)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &StaticResources{input: out}
	s.refs.Store(1)
	return s, nil
}

func nilHandle(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func (s *StaticResources) retain() bool {
	if s == nil {
		return false
	}
	for {
		old := s.refs.Load()
		if old <= 0 || old == int64(^uint64(0)>>1) {
			return false
		}
		if s.refs.CompareAndSwap(old, old+1) {
			return true
		}
	}
}
func (s *StaticResources) release() {
	if s.refs.Add(-1) == 0 {
		s.input.Release()
	}
}

// Release relinquishes only the preparer's reference, exactly once.
func (s *StaticResources) Release() {
	if s != nil {
		s.releaseOnce.Do(s.release)
	}
}

// Current checks immutable root resource validity without locks or I/O.
func (s *StaticResources) Current() bool { return s != nil && s.refs.Load() > 0 && s.input.Current() }
