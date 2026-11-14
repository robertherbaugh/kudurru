package providerhost

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestColdDeniedCompletePublisherAndDetachedStatic(t *testing.T) {
	in := staticInput(t)
	var releases atomic.Int64
	in.Release = func() { releases.Add(1) }
	x := newFixture(t, in)
	// Caller mutation after preparation cannot alter owner policy or keys.
	in.Auth.CurrentKey[0] = 'z'
	in.Auth.Permissions.Roles["viewer"][0] = "*.*"
	h := capture(t, x)
	if h.Auth.License.Providers || h.Auth.ConfigProviders == nil || h.Auth.ProviderResources == nil || !h.Current() {
		t.Fatal("cold host not a complete denying dependency")
	}
	if h.Auth.CurrentKey[0] != 'k' || h.Auth.Permissions.Roles["viewer"][0] == "*.*" {
		t.Fatal("caller aliases static resources")
	}
	h.Auth.CurrentKey[0] = 'x'
	h.Auth.Permissions.Roles["viewer"][0] = "*.*"
	next := capture(t, x)
	if next.Auth.CurrentKey[0] != 'k' || next.Auth.Permissions.Roles["viewer"][0] == "*.*" {
		t.Fatal("captured outputs alias owner")
	}
	next.Release()
	lease, err := x.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v, ok := lease.Bundle().Component("auth")
	if !ok {
		t.Fatal("missing auth owner")
	}
	if _, err = v.(*servingauth.View).Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: x.wire}, x.f.now()); err != nil {
		t.Fatal("provider denial gated actual local SA token", err)
	}
	lease.Release()
	// Host holds its own reference beyond owner shutdown. Exactly-once release.
	x.stop()
	if err = x.host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	x.publisher.Close()
	if releases.Load() != 0 {
		t.Fatal("held capture did not retain static input")
	}
	h.Release()
	h.Release()
	if releases.Load() != 1 || h.Current() {
		t.Fatal("static release not exactly once / released cut current")
	}
}

func TestStableMemoAcrossRegistryChurnCanonicalAttachmentAndSameE(t *testing.T) {
	in := staticInput(t)
	in.LocalProviders = []providerregistry.DesiredConfig{{ID: "corp", Spec: spec()}}
	x := newFixture(t, in)
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.ReconcileProviderConfigs(ctx, tx, in.LocalProviders, x.f.now())
	})
	e := reset(t, x)
	if e == "" {
		t.Fatal("missing pending activation")
	}
	first := capture(t, x)
	defer first.Release()
	accept(t, x, e)
	second := capture(t, x)
	defer second.Release()
	if first.Identity != second.Identity || first.Providers != second.Providers {
		t.Fatal("first same-E receipt rebuilt host lineage")
	}
	addObject(t, x, "second", spec())
	accept(t, x, e)
	third := capture(t, x)
	defer third.Release()
	if first.Identity != third.Identity || first.Providers != third.Providers {
		t.Fatal("unrelated object/additional same-E receipt rebuilt host")
	}
	canonical := registry(t, x.s).ConfigSources()
	if len(third.Auth.ConfigProviders) != 1 || !reflect.DeepEqual(canonical, third.Auth.ConfigProviders) {
		t.Fatal("config cursor omitted canonical registry source")
	}
	// Local mismatching spec is never overwritten to look canonical. Config
	// source still shadows its object; no local/caller UID is manufactured.
	changed := in.LocalProviders[0]
	changed.Spec.ClientID = "new-canonical-client"
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.ReconcileProviderConfigs(ctx, tx, []providerregistry.DesiredConfig{changed}, x.f.now())
	})
	fourth := capture(t, x)
	defer fourth.Release()
	if fourth.Auth.ConfigProviders[0].Spec.ClientID != "new-canonical-client" {
		t.Fatal("canonical precedence lost")
	}
	if x.host.value.Load().static.input.LocalProviders[0].Spec.ClientID != "browser" {
		t.Fatal("local spec overwritten by registry")
	}
	if fourth.Auth.ConfigProviders[0].Ref.UID != canonical[0].Ref.UID || !fourth.Auth.ConfigProviders[0].CreatedAt.Equal(canonical[0].CreatedAt) {
		t.Fatal("same config slot lost UID/time")
	}
	// Changed receipt eligibility can deny pending temporarily, but no local
	// spec fallback or fabricated adoption can repair it.
	await(t, func() bool {
		lease, err := x.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		return lease.Bundle().Identity().Base.Same(image(t, x.s).Generation())
	})
}
func TestStaticCapsNilHandlesAndRedaction(t *testing.T) {
	for name, change := range map[string]func(*StaticInput){
		"missing-origins":        func(in *StaticInput) { in.AllowedOrigins = nil },
		"local-cap":              func(in *StaticInput) { in.LocalProviders = make([]providerregistry.DesiredConfig, 65) },
		"trust-cap":              func(in *StaticInput) { in.TrustPEM = make([]byte, (2<<20)+1) },
		"missing-hasher":         func(in *StaticInput) { in.Auth.Hasher = (*users.Hasher)(nil) },
		"zero-totp":              func(in *StaticInput) { in.Auth.TOTPKeys = new(totp.Keyring) },
		"live-provider-fallback": func(in *StaticInput) { in.Auth.ConfigProviders = []servingauth.ProviderSource{{}} },
		"permissions-cap":        func(in *StaticInput) { in.Auth.Permissions.Roles["viewer"] = make([]string, 1025) },
		"unknown-role": func(in *StaticInput) {
			in.Auth.StaticBindings = append(in.Auth.StaticBindings, rbac.Binding{Role: "unknown"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := staticInput(t)
			change(&in)
			if _, err := PrepareStatic(t.Context(), in); err == nil {
				t.Fatal("invalid or excessive static preparation accepted")
			}
		})
	}
	s, err := PrepareStatic(t.Context(), staticInput(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.MarshalJSON()
	if err != nil || strings.Contains(string(raw), "kkkk") {
		t.Fatal("resource diagnostics disclose material")
	}
	s.Release()
	if s.Current() {
		t.Fatal("released static resources usable")
	}
}

func TestHostIdentityBindsIndependentLocalInstallation(t *testing.T) {
	x := newFixture(t, staticInput(t))
	other := start(t, x.s, x.f, staticInput(t))
	first, second := capture(t, x), capture(t, other)
	defer first.Release()
	defer second.Release()
	if first.Identity == second.Identity {
		t.Fatal("independent local license installations shared auth identity")
	}
}

func TestReplaceStaticRetainsOldAndNewCapturesUntilRelease(t *testing.T) {
	oldInput := staticInput(t)
	var oldReleased, newReleased atomic.Int64
	oldInput.Release = func() { oldReleased.Add(1) }
	x := newFixture(t, oldInput)
	old := capture(t, x)
	newInput := staticInput(t)
	newInput.Identity = "replacement-static"
	newInput.AuthKeyIdentity = "replacement-keys"
	newInput.Release = func() { newReleased.Add(1) }
	s, err := PrepareStatic(t.Context(), newInput)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	if err = x.host.ReplaceStatic(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.Release()
	if old.Current() || !s.Current() || oldReleased.Load() != 0 || newReleased.Load() != 0 {
		t.Fatal("replacement failed exact reference/lifetime separation")
	}
	newCut := capture(t, x)
	if newCut.Auth.License.Providers || newCut.Identity == old.Identity {
		t.Fatal("replacement self-admitted or reused host generation")
	}
	x.stop()
	x.publisher.Close()
	old.Release()
	old.Release()
	if oldReleased.Load() != 1 {
		t.Fatal("old static input not released exactly once")
	}
	if err := x.host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if newReleased.Load() != 0 {
		t.Fatal("owner close released held replacement input")
	}
	newCut.Release()
	newCut.Release()
	if newReleased.Load() != 1 {
		t.Fatal("new static input not released exactly once")
	}
}

func TestIdenticalPollingGraceDeadlineAndReplacementFences(t *testing.T) {
	x := newFixture(t, staticInput(t))
	e := reset(t, x)
	old := capture(t, x)
	defer old.Release()
	if err := x.license.Set(x.f.raw, "test"); err != nil {
		t.Fatal(err)
	}
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	poll := capture(t, x)
	defer poll.Release()
	if poll.Identity != old.Identity || !old.Current() {
		t.Fatal("identical polling changed installation")
	}
	x.f.clock.Store(time.Unix(x.f.claims.ExpiresAt, 0).UnixNano())
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	grace := capture(t, x)
	defer grace.Release()
	if grace.Identity != old.Identity || x.host.value.Load().entitlement != e {
		t.Fatal("grace transition reset equivalent management policy")
	}
	if err := x.license.Set("", ""); err != nil {
		t.Fatal(err)
	}
	if old.Current() {
		t.Fatal("held license cut survived actual removal")
	}
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := x.license.Set(x.f.raw, "test"); err != nil {
		t.Fatal(err)
	}
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	returned := capture(t, x)
	defer returned.Release()
	if returned.Auth.License.Providers || returned.Identity == old.Identity {
		t.Fatal("identical-byte license return self-admitted")
	}
	fresh := reset(t, x)
	if fresh == e {
		t.Fatal("explicit reactivation reused E")
	}
	x.f.clock.Store(time.Unix(x.f.claims.ExpiresAt, 0).Add(license.Grace).UnixNano())
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	denied := capture(t, x)
	defer denied.Release()
	if denied.Auth.License.Providers || !denied.Current() {
		t.Fatal("expired policy not a current complete denying cut")
	}
	if err := x.host.CheckAdmission(t.Context(), nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil admission did not deny")
	}
}
