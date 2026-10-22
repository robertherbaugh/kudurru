package servingauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type externalFixture struct {
	*fixture
	key      ed25519.PrivateKey
	verifier *oidccore.Verifier
	provider *publicv1.IdentityProvider
	raw      string
	claims   map[string]any
}

func newExternalFixture(t *testing.T) *externalFixture {
	t.Helper()
	f := &externalFixture{fixture: newFixture(t)}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = key
	f.provider = apply(t, f.st, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp", CreationTimestamp: metav1.NewTime(f.now.Add(-time.Hour))}, Spec: publicv1.IdentityProviderSpec{Issuer: "https://corp.example/issuer", ClientID: "browser", CLIClientID: "cli", ClientSecretRef: &publicv1.CredentialReference{Env: "CORP_CREDENTIAL"}, Provisioning: publicv1.Provisioning{SyncGroups: true}}}).(*publicv1.IdentityProvider)
	jwks, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "test", Algorithm: "EdDSA", Use: "sig"}}})
	f.verifier, err = oidccore.NewVerifier(f.provider.Spec.Issuer, []string{"EdDSA"}, jwks)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f.st, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "corp-read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "corp:ops"}}}})
	f.deps.License.Providers = true
	f.claims = map[string]any{"iss": f.provider.Spec.Issuer, "sub": "subject /+:", "aud": "browser", "iat": f.now.Add(-time.Minute).Unix(), "exp": f.now.Add(5 * time.Minute).Unix(), "preferred_username": "alice", "groups": []string{"ops"}, "nonce": "expected-nonce"}
	f.raw = f.sign(t, f.claims)
	f.resources(t)
	return f
}
func (f *externalFixture) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.key}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": "test", "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(claims)
	object, err := s.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := object.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func (f *externalFixture) resources(t *testing.T) {
	t.Helper()
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := ProviderSources(t.Context(), cut, f.deps.ConfigProviders)
	if err != nil {
		t.Fatal(err)
	}
	f.deps.ProviderResources = []ProviderResource{}
	for _, source := range sources {
		resource := ProviderResource{Ref: source.Ref, SpecIdentity: source.SpecIdentity, PolicyIdentity: source.PolicyIdentity, Availability: ProviderUnavailable, TrustIdentity: "test-trust", UnavailableReason: "discovery"}
		if source.Ref.ID == "corp" {
			resource.Availability = ProviderReady
			resource.UnavailableReason = ""
			resource.Verifier = f.verifier
			resource.VerifierIdentity = f.verifier.Identity()
		}
		f.deps.ProviderResources = append(f.deps.ProviderResources, resource)
	}
}
func (f *externalFixture) proof(t *testing.T) ExternalLoginProof {
	t.Helper()
	info, err := f.view(t).ProviderInfo("corp")
	if err != nil {
		t.Fatal(err)
	}
	return ExternalLoginProof{Provider: info.Descriptor.Ref, SpecIdentity: info.Descriptor.SpecIdentity, ResourceIdentity: info.ResourceIdentity, RawIDToken: f.raw, Mode: oidccore.BrowserIDToken, Browser: oidccore.BrowserProof{Nonce: "expected-nonce"}}
}
func writeTxSecurity(ctx context.Context, tx *sqlite.CommandTx, state Security) error {
	rows, err := EncodeSecurity(state)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err = tx.PutSection(ctx, "security", row); err != nil {
			return err
		}
	}
	return nil
}
func (f *externalFixture) enroll(t *testing.T, rollback bool) string {
	t.Helper()
	proof := f.proof(t)
	aborted := errors.New("test rollback")
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		d, err := ResolveExternalSessionCurrent(ctx, tx, f.deps, proof, f.now)
		if err != nil {
			return err
		}
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		state, err := DecodeSecurity(rows)
		if err != nil {
			return err
		}
		verified := f.now
		identity := ExternalIdentity{FormatVersion: 1, Provider: d.Provider.Ref, Issuer: d.Identity.Issuer, Subject: d.Identity.Subject, UID: strings.Repeat("a", 32), CreatedAt: f.now, Username: d.Identity.Username, DisplayName: d.Identity.DisplayName, Email: d.Identity.Email, Groups: slices.Clone(d.Identity.Groups), GroupsVerifiedAt: &verified}
		if d.Existing != nil {
			identity = *d.Existing
		} else {
			state.ExternalIdentities = append(state.ExternalIdentities, identity)
		}
		account, _ := ExternalAccount(identity)
		keyring, err := oidccore.NewEvidenceKeyring(f.deps.CurrentKey, f.deps.PreviousKey)
		if err != nil {
			return err
		}
		evidence, err := d.SealEvidence(keyring, identity.UID, "external-session-id")
		if err != nil {
			return err
		}
		state.Epoch++
		state.ExternalSessions = append(state.ExternalSessions, ExternalSessionRecord{FormatVersion: 1, ID: "external-session-id", Account: account, Provider: identity.Provider, Issuer: identity.Issuer, Subject: identity.Subject, Epoch: state.Epoch, IssuedAt: f.now, ExpiresAt: f.now.Add(time.Hour), IdleExpiresAt: f.now.Add(30 * time.Minute), Grants: d.Grants, CSRFHash: CSRFHash(f.csrf), Evidence: evidence})
		if err = writeTxSecurity(ctx, tx, state); err != nil {
			return err
		}
		if _, err = AuthenticateCurrent(ctx, tx, f.deps, Credential{Kind: Cookie, Value: "bad"}, f.now); err == nil {
			return errors.New("invalid cookie accepted")
		}
		if rollback {
			return aborted
		}
		return nil
	})
	if rollback {
		if !errors.Is(err, aborted) {
			t.Fatal(err)
		}
		if _, err = f.view(t).SealSession("external-session-id", f.csrf); err == nil {
			t.Fatal("rolled back enrollment served")
		}
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := f.view(t).SealSession("external-session-id", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	return cookie
}
func TestExternalCurrentEnrollmentRollbackCookieExpiryAndDetachedEvidence(t *testing.T) {
	f := newExternalFixture(t)
	f.enroll(t, true)
	cookie := f.enroll(t, false)
	v := f.view(t)
	p, err := v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now)
	if err != nil || p.AccountUID != strings.Repeat("a", 32) || p.BuiltinAdmin {
		t.Fatal("JWT identity", err)
	}
	p.External.Evidence = "injected"
	p.Groups[0] = "injected"
	p.Grants[0] = "admin"
	if _, err = v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); err != nil {
		t.Fatal("aliased JWT evidence", err)
	}
	later := f.now.Add(6 * time.Minute)
	if _, err = v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, later); err == nil {
		t.Fatal("JWT expiry bypass")
	}
	sess, err := v.CookieSession(cookie, later)
	if err != nil || sess.ID != "external-session-id" || sess.CSRF != f.csrf || !slices.Equal(sess.RolesSnapshot, []string{"viewer"}) {
		t.Fatal("enrolled session expired with original JWT", err)
	}
	sess.Principal.External.SessionID = "injected"
	sess.Principal.Groups[0] = "injected"
	sess.RolesSnapshot[0] = "admin"
	if _, err = v.CookieSession(cookie, later); err != nil {
		t.Fatal("session alias", err)
	}
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openStore(t, f.path, replication.Standalone)
	if _, err = f.view(t).CookieSession(cookie, later); err != nil {
		t.Fatal("restart", err)
	}
	proof := f.proof(t)
	proof.Browser.Nonce = "wrong"
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, e := ResolveExternalSessionCurrent(ctx, tx, f.deps, proof, f.now)
		return e
	})
	if !errors.Is(err, ErrCredentials) {
		t.Fatal("browser nonce bypass", err)
	}
}
func TestExternalFrozenGrantIntersectionAndHeldResourceCuts(t *testing.T) {
	f := newExternalFixture(t)
	cookie := f.enroll(t, false)
	old := f.view(t)
	apply(t, f.st, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "corp-write", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "operator", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "corp:ops"}}}})
	current := f.view(t)
	p, err := current.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Grants, []string{"viewer"}) {
		t.Fatal("new binding broadened session")
	}
	apply(t, f.st, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "corp-read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "corp:other"}}}})
	current = f.view(t)
	p, err = current.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Grants) != 0 {
		t.Fatal("another subject/global role retained session grant")
	}
	d, err := current.Authorize(p, readAttrs(), f.now)
	if err != nil || d.Allowed {
		t.Fatal("frozen intersection bypass", err)
	}
	if _, err = old.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now); err != nil {
		t.Fatal("old cut lost resources", err)
	}
	changed := f.provider.DeepCopy()
	changed.Spec.ClientID = "new-client"
	apply(t, f.st, changed)
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, e := AuthenticateCurrent(ctx, tx, f.deps, Credential{Kind: OIDCJWT, Value: f.raw}, f.now)
		return e
	})
	if !errors.Is(err, ErrDependency) {
		t.Fatal("stale resources accepted current spec", err)
	}
	f.resources(t)
	if _, err = f.view(t).Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); err == nil {
		t.Fatal("current audience ignored")
	}
	if _, err = old.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); err != nil {
		t.Fatal("held issuer/audience changed", err)
	}
}

func (f *externalFixture) state(t *testing.T) Security {
	t.Helper()
	state, err := DecodeSecurity(f.image(t).Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func TestProviderMatchingPrecedenceUnavailableAndLicense(t *testing.T) {
	f := newExternalFixture(t)
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Dependencies){
		func(d *Dependencies) { d.ProviderResources = d.ProviderResources[:1] },
		func(d *Dependencies) { d.ProviderResources = append(d.ProviderResources, d.ProviderResources[0]) },
		func(d *Dependencies) { d.ProviderResources[0].Ref.UID = "wrong" },
		func(d *Dependencies) { d.ProviderResources[0].Ref.Origin = ProviderConfig },
		func(d *Dependencies) { d.ProviderResources[0].SpecIdentity = strings.Repeat("a", 64) },
		func(d *Dependencies) { d.ProviderResources[0].PolicyIdentity = strings.Repeat("a", 64) },
		func(d *Dependencies) { d.ProviderResources[0].VerifierIdentity = strings.Repeat("a", 64) },
		func(d *Dependencies) { d.ProviderResources[0].TrustIdentity = "" },
		func(d *Dependencies) { d.ProviderResources[0].Availability = "unknown" },
	} {
		deps := f.deps
		deps.ProviderResources = slices.Clone(deps.ProviderResources)
		change(&deps)
		if _, err = Prepare(t.Context(), cut, deps); err == nil {
			t.Fatal("resource mismatch accepted")
		}
	}
	deps := f.deps
	deps.ConfigProviders = []ProviderSource{{Ref: ProviderRef{ID: "corp", UID: "stable-config-uid", Origin: ProviderConfig}, CreatedAt: f.now.Add(-time.Hour), Spec: f.provider.Spec}}
	sources, err := ProviderSources(t.Context(), cut, deps.ConfigProviders)
	if err != nil {
		t.Fatal(err)
	}
	deps.ProviderResources = []ProviderResource{}
	for _, source := range sources {
		deps.ProviderResources = append(deps.ProviderResources, ProviderResource{Ref: source.Ref, SpecIdentity: source.SpecIdentity, PolicyIdentity: source.PolicyIdentity, Availability: ProviderUnavailable, TrustIdentity: "config-down", UnavailableReason: "discovery"})
	}
	v, err := Prepare(t.Context(), cut, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release()
	info, err := v.ProviderInfo("corp")
	if err != nil || info.Descriptor.Ref.Origin != ProviderConfig || info.Descriptor.Ref.UID != "stable-config-uid" {
		t.Fatal("config shadow lost", err)
	}
	if _, err = v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); err == nil {
		t.Fatal("stored provider fallback")
	}
	if _, err = v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err != nil {
		t.Fatal("provider outage gated local/SA", err)
	}
	deps = f.deps
	deps.License.Providers = false
	v, err = Prepare(t.Context(), cut, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release()
	if _, err = v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); err == nil {
		t.Fatal("provider entitlement bypass")
	}
	if _, err = v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err != nil {
		t.Fatal("external license gated local/SA", err)
	}
	info.Descriptor.Spec.Scopes[0] = "injected"
	original, _ := f.view(t).ProviderInfo("corp")
	if original.Descriptor.Spec.Scopes[0] == "injected" {
		t.Fatal("descriptor alias")
	}
}
func TestExternalFloorsRestartInstallPromotionAndCurrentStagedRollback(t *testing.T) {
	f := newExternalFixture(t)
	cookie := f.enroll(t, false)
	old := f.view(t)
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, d, e := ReauthorizeCurrent(ctx, tx, f.deps, Credential{Kind: Cookie, Value: cookie}, readAttrs(), f.now)
		if e != nil || !d.Allowed {
			return e
		}
		rows, e := tx.ListSections(ctx, "security")
		if e != nil {
			return e
		}
		state, e := DecodeSecurity(rows)
		if e != nil {
			return e
		}
		state.Epoch++
		state.GrantFloors = append(state.GrantFloors, GrantFloor{FormatVersion: 1, Grant: "viewer", ValidAfter: f.now})
		if e = writeTxSecurity(ctx, tx, state); e != nil {
			return e
		}
		_, _, e = ReauthorizeCurrent(ctx, tx, f.deps, Credential{Kind: Cookie, Value: cookie}, readAttrs(), f.now)
		if !errors.Is(e, ErrCredentials) {
			t.Fatal("staged floor ignored", e)
		}
		return errors.New("abort floor")
	})
	if err == nil {
		t.Fatal("rollback not exercised")
	}
	if _, err = f.view(t).CookieSession(cookie, f.now); err != nil {
		t.Fatal("rolled-back floor leaked", err)
	}
	state := f.state(t)
	state.Epoch++
	state.ProviderFloors = append(state.ProviderFloors, ProviderFloor{FormatVersion: 1, Provider: "corp", UID: string(f.provider.UID), ValidAfter: f.now.Add(-time.Minute)})
	f.security = state
	f.writeSecurity(t)
	if _, err = f.view(t).Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("JWT floor equality", err)
	}
	if _, err = f.view(t).CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("revoked original JWT recycled via newer cookie", err)
	}
	if _, err = old.CookieSession(cookie, f.now); err != nil {
		t.Fatal("old cut floor changed", err)
	}
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openStore(t, f.path, replication.Standalone)
	if _, err = f.view(t).CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("restart lost floor", err)
	}
	source := f.image(t)
	follower := openStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err = follower.Enroll(t.Context(), source.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *source)
	if err != nil {
		t.Fatal(err)
	}
	authority := follower.Authority()
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: authority.Term, ExpectedInstalled: authority.Installed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: source.Term, ExpectedInstalled: installed, RequestID: "external-promotion"}); err != nil {
		t.Fatal(err)
	}
	image, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut, err := CaptureImage(t.Context(), image, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := Prepare(t.Context(), cut, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer promoted.Release()
	if _, err = promoted.CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("promotion lost floor", err)
	}
}
func TestExternalAPITokenExactUIDScopesCurrentProfileAndNoLiveStore(t *testing.T) {
	f := newExternalFixture(t)
	cookie := f.enroll(t, false)
	state := f.state(t)
	i := state.ExternalIdentities[0]
	wire, id := tokens.Generate()
	token := apply(t, f.st, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(f.now.Add(time.Second))}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: "User", Name: i.Username, ID: (oidccore.IdentityKey{Provider: i.Provider.ID, Issuer: i.Issuer, Subject: i.Subject}).String(), Provider: i.Provider.ID, UID: i.UID}, SecretHash: tokens.Hash(wire), Grants: []string{"viewer"}, Scopes: []string{"viewer"}}}).(*internalv1.APIToken)
	v := f.view(t)
	now := f.now.Add(2 * time.Second)
	p, err := v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: wire}, now)
	if err != nil || p.AccountUID != i.UID || p.AuthMethod != auth.MethodToken || !slices.Equal(p.Scopes, []string{"viewer"}) {
		t.Fatal("external personal token", err)
	}
	p.BuiltinAdmin = true
	p.Grants = []string{"admin"}
	p.Scopes = []string{"admin"}
	p.Name = "local-admin"
	refreshed, err := v.LoadPrincipal(p, now)
	if err != nil || refreshed.BuiltinAdmin || refreshed.Name == "local-admin" || !slices.Equal(refreshed.Scopes, []string{"viewer"}) {
		t.Fatal("forged token projection", err)
	}
	// A sealed old token proof cannot bind a replacement incarnation by name.
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	changed := token.DeepCopy()
	changed.UID = "replacement-token-uid"
	cut.objects[store.Key{Kind: "APIToken", Name: id}] = changed
	replacement, err := Prepare(t.Context(), cut, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Release()
	if _, err = replacement.LoadPrincipal(refreshed, now); err == nil {
		t.Fatal("lost token UID receipt")
	}
	state.ExternalIdentities[0].Groups = []string{}
	when := f.now.Add(time.Second)
	state.ExternalIdentities[0].GroupsVerifiedAt = &when
	state.Epoch++
	f.security = state
	f.writeSecurity(t)
	current := f.view(t)
	p, err = current.Authenticate(t.Context(), Credential{Kind: APIToken, Value: wire}, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := current.Authorize(p, readAttrs(), now)
	if err != nil || d.Allowed {
		t.Fatal("durable group narrowing ignored", err)
	}
	if _, err = current.CookieSession(cookie, now); err != nil {
		t.Fatal(err)
	}
	// Prepared Views continue with only their detached captured inputs after close.
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: wire}, now); err != nil {
		t.Fatal("store read after Prepare", err)
	}
	if _, err = v.CookieSession(cookie, now); err != nil {
		t.Fatal("cookie store read", err)
	}
}
func TestExternalProviderAndSecurityPreallocationBounds(t *testing.T) {
	f := newExternalFixture(t)
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ProviderSources(t.Context(), cut, make([]ProviderSource, MaxProviders+1)); !errors.Is(err, ErrBounds) {
		t.Fatal(err)
	}
	if _, err = Prepare(t.Context(), cut, Dependencies{}); err == nil {
		t.Fatal("implicit dependencies")
	}
	source := ProviderSource{Ref: ProviderRef{ID: "config", UID: "stable", Origin: ProviderConfig}, CreatedAt: f.now, Spec: f.provider.Spec}
	source.Spec.ClaimValidationRules = make([]publicv1.ClaimValidationRule, 65)
	if _, err = ProviderSources(t.Context(), cut, []ProviderSource{source}); !errors.Is(err, ErrBounds) {
		t.Fatal("rules allocation cap", err)
	}
	f.enroll(t, false)
	state := f.state(t)
	state.ExternalSessions[0].Evidence = strings.Repeat("x", MaxExternalEvidenceBytes+1)
	if _, err = EncodeSecurity(state); err == nil {
		t.Fatal("evidence cap")
	}
	state = f.state(t)
	rows, err := EncodeSecurity(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"disabled", "groupsVerifiedAt", "revoked"} {
		changed := slices.Clone(rows)
		found := false
		for j, row := range changed {
			var object map[string]json.RawMessage
			_ = json.Unmarshal(row.Data, &object)
			if _, ok := object[field]; !ok {
				continue
			}
			delete(object, field)
			changed[j].Data, _ = json.Marshal(object)
			found = true
			break
		}
		if !found {
			t.Fatal("field fixture")
		}
		if _, err = DecodeSecurity(changed); err == nil {
			t.Fatal("missing security field", field)
		}
	}
}

func TestExternalExistingDecisionUIDStagedDisableAndSealedEvidenceTamper(t *testing.T) {
	f := newExternalFixture(t)
	cookie := f.enroll(t, false)
	proof := f.proof(t)
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		d, e := ResolveExternalSessionCurrent(ctx, tx, f.deps, proof, f.now)
		if e != nil || d.Existing == nil {
			t.Fatal("existing", e)
		}
		if e = d.ValidAt(f.now); e != nil {
			t.Fatal("live decision", e)
		}
		if e = d.ValidAt(f.now.Add(-time.Nanosecond)); e == nil {
			t.Fatal("issuance clock moved backward")
		}
		if e = d.ValidAt(f.now.Add(5 * time.Minute)); e == nil {
			t.Fatal("expired decision allowed issuance")
		}
		var zero ExternalSessionDecision
		if e = zero.ValidAt(f.now); e == nil {
			t.Fatal("zero decision")
		}
		keys, e := oidccore.NewEvidenceKeyring(f.deps.CurrentKey, nil)
		if e != nil {
			return e
		}
		if _, e = d.SealEvidence(keys, strings.Repeat("b", 32), "another-session-id"); e == nil {
			t.Fatal("existing UID replaced")
		}
		d.Provider.Ref.UID = "forged"
		d.Identity.Subject = "forged"
		d.Existing.Groups[0] = "forged"
		sealed, e := d.SealEvidence(keys, strings.Repeat("a", 32), "another-session-id")
		if e != nil {
			return e
		}
		b := oidccore.EvidenceBinding{Kind: oidccore.SessionEvidence, Provider: proof.Provider.ID, ProviderUID: proof.Provider.UID, Origin: string(proof.Provider.Origin), Issuer: f.provider.Spec.Issuer, Subject: f.claims["sub"].(string), IdentityUID: strings.Repeat("a", 32), RecordID: "another-session-id"}
		if _, e = keys.Open(b, sealed); e != nil {
			t.Fatal("public projection replaced private source", e)
		}
		rows, e := tx.ListSections(ctx, "security")
		if e != nil {
			return e
		}
		state, e := DecodeSecurity(rows)
		if e != nil {
			return e
		}
		state.ExternalIdentities[0].Disabled = true
		state.Epoch++
		if e = writeTxSecurity(ctx, tx, state); e != nil {
			return e
		}
		if _, e = ResolveExternalSessionCurrent(ctx, tx, f.deps, proof, f.now); !errors.Is(e, ErrCredentials) {
			t.Fatal("staged disabled identity", e)
		}
		if _, e = AuthenticateCurrent(ctx, tx, f.deps, Credential{Kind: Cookie, Value: cookie}, f.now); !errors.Is(e, ErrCredentials) {
			t.Fatal("staged disabled cookie", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.view(t).CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("disabled persisted", err)
	}
}
func TestExternalCookieRejectsWrongEvidenceUIDSessionCSRFAndEpoch(t *testing.T) {
	f := newExternalFixture(t)
	cookie := f.enroll(t, false)
	v := f.view(t)
	raw, _, err := v.sealer.Open(cookie)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CookiePayload){func(p *CookiePayload) { p.CSRF = strings.Repeat("z", 32) }, func(p *CookiePayload) { p.Epoch++ }, func(p *CookiePayload) { p.Account.UID = strings.Repeat("b", 32) }, func(p *CookiePayload) { p.FormatVersion = 2 }, func(p *CookiePayload) { p.SessionID = "different-session-id" }} {
		var payload CookiePayload
		_ = json.Unmarshal(raw, &payload)
		mutate(&payload)
		changed, _ := json.Marshal(payload)
		if _, err = v.CookieSession(v.sealer.Seal(changed), f.now); err == nil {
			t.Fatal("cookie row mismatch")
		}
	}
	original := f.state(t)
	for _, mutate := range []func(*Security){
		func(s *Security) { s.ExternalSessions[0].Evidence = v.sealer.Seal([]byte(`{}`)) },
		func(s *Security) {
			s.ExternalSessions[0].Evidence = s.ExternalSessions[0].Evidence[:len(s.ExternalSessions[0].Evidence)-1] + "!"
		},
		func(s *Security) {
			s.ExternalIdentities[0].UID = strings.Repeat("b", 32)
			s.ExternalSessions[0].Account.UID = strings.Repeat("b", 32)
		},
		func(s *Security) { s.ExternalSessions[0].Revoked = true },
	} {
		state := original
		state.ExternalIdentities = slices.Clone(original.ExternalIdentities)
		state.ExternalSessions = slices.Clone(original.ExternalSessions)
		mutate(&state)
		state.Epoch++
		cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
		if err != nil {
			t.Fatal(err)
		}
		cut.security = state
		prepared, err := Prepare(t.Context(), cut, f.deps)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = prepared.CookieSession(cookie, f.now); err == nil {
			t.Fatal("bad durable evidence accepted")
		}
		prepared.Release()
	}
}
func TestExternalNeverAlwaysGrantFloorsAndDefaultSemantics(t *testing.T) {
	f := newExternalFixture(t)
	p, err := f.view(t).Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now)
	if err != nil || p.AccountUID != "" {
		t.Fatal("read provisioned fake UID", err)
	}
	changed := f.provider.DeepCopy()
	changed.Spec.Provisioning.AutoProvision = "never"
	apply(t, f.st, changed)
	f.resources(t)
	if _, err = f.view(t).Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); err == nil {
		t.Fatal("never provisioned absent identity")
	}
	changed.Spec.Provisioning.AutoProvision = "always"
	changed.Spec.Provisioning.DefaultRole = "viewer"
	apply(t, f.st, changed)
	f.resources(t)
	apply(t, f.st, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "corp-read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "corp:others"}}}})
	v := f.view(t)
	p, err = v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now)
	if err != nil || !slices.Equal(p.Grants, []string{"viewer"}) {
		t.Fatal("per-provider default", err)
	}
	cookie := f.enroll(t, false)
	state := f.state(t)
	state.Epoch++
	state.GrantFloors = []GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: f.now.Add(-time.Minute)}}
	f.security = state
	f.writeSecurity(t)
	if _, err = f.view(t).Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("source JWT grant-floor equality", err)
	}
	// A floor later than the session blocks it even if that grant is added again.
	state.GrantFloors[0].ValidAfter = f.now
	state.Epoch++
	f.security = state
	f.writeSecurity(t)
	apply(t, f.st, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "corp-read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "corp:ops"}}}})
	if _, err = f.view(t).CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("narrow/regrant revival", err)
	}
}

func TestMissingExternalIdentityFloorIsNotEmptyProvisioningState(t *testing.T) {
	f := newExternalFixture(t)
	f.enroll(t, false)
	proof := f.proof(t)
	state := f.state(t)
	a, _ := ExternalAccount(state.ExternalIdentities[0])
	identityKey, _ := ExternalIdentityKey(state.ExternalIdentities[0].Provider, state.ExternalIdentities[0].Issuer, state.ExternalIdentities[0].Subject)
	sessionKey := ExternalSessionKey(state.ExternalSessions[0].ID)
	state.AccountFloors = append(state.AccountFloors, AccountFloor{FormatVersion: 1, Account: a, ValidAfter: f.now})
	state.ExternalSessions = []ExternalSessionRecord{}
	state.ExternalIdentities = []ExternalIdentity{}
	state.Epoch++
	if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.DeleteSection(ctx, "security", identityKey); err != nil {
			return err
		}
		if err := tx.DeleteSection(ctx, "security", sessionKey); err != nil {
			return err
		}
		return writeTxSecurity(ctx, tx, state)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.view(t).Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("missing account erased durable revocation", err)
	}
	if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := ResolveExternalSessionCurrent(ctx, tx, f.deps, proof, f.now)
		return err
	}); !errors.Is(err, ErrCredentials) {
		t.Fatal("current resolver reprovisioned revoked tuple", err)
	}
}
