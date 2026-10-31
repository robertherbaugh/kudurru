package security

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type externalIssueFixture struct {
	*sessionFixture
	deps     servingauth.Dependencies
	registry servingauth.Schema
	key      ed25519.PrivateKey
	provider *public.IdentityProvider
}

func newExternalIssueFixture(t *testing.T, syncGroups bool) *externalIssueFixture {
	t.Helper()
	f := &externalIssueFixture{sessionFixture: newSessionFixture(t)}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = private
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		object, err := tx.Apply(ctx, &public.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp", CreationTimestamp: metav1.NewTime(f.now.Add(-time.Hour))}, Spec: public.IdentityProviderSpec{Issuer: "https://corp.example/issuer", ClientID: "browser", CLIClientID: "cli", ClientSecretRef: &public.CredentialReference{Env: "TEST_CORP_SECRET"}, Provisioning: public.Provisioning{SyncGroups: syncGroups}}}, store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			return err
		}
		f.provider = object.(*public.IdentityProvider)
		if _, err := tx.Apply(ctx, &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "external-read", Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "viewer", Subjects: []public.BindingSubject{{Kind: "Group", Name: "corp:ops"}, {Kind: "Group", Name: "corp:keep"}}}}, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		settings.Spec.Settings = map[string]extensions.JSON{"auth": {Raw: []byte(`{"localLogin":"disabled","session":{"idleTimeout":"8m","lifetime":"10m"}}`)}}
		_, err = tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	f.registry = servingauth.Schema{Scheme: scheme, SchemaVersion: store.SchemaVersion}
	known := scheme.KnownTypes(public.GroupVersion)
	for name, typ := range known {
		if _, list := known[name+"List"]; !list {
			continue
		}
		if _, ok := reflect.New(typ).Interface().(store.Object); ok {
			f.registry.Kinds = append(f.registry.Kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
		}
	}
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	f.deps = servingauth.Dependencies{Identity: "external-test", ExpectedIdentity: "external-test", License: servingauth.LicensePolicy{RBAC: true, Providers: true}, Permissions: permissions, CurrentKey: bytes.Repeat([]byte{5}, 32), ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}}
	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "test", Algorithm: "EdDSA", Use: "sig"}}})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := oidccore.NewVerifier(f.provider.Spec.Issuer, []string{"EdDSA"}, jwks)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := servingauth.ProviderSources(t.Context(), f.cut(t), f.deps.ConfigProviders)
	if err != nil || len(sources) != 1 {
		t.Fatal("provider source", err)
	}
	source := sources[0]
	f.deps.ProviderResources = []servingauth.ProviderResource{{Ref: source.Ref, SpecIdentity: source.SpecIdentity, PolicyIdentity: source.PolicyIdentity, Availability: servingauth.ProviderReady, Verifier: verifier, VerifierIdentity: verifier.Identity(), TrustIdentity: "test-key-set"}}
	return f
}

func (f *externalIssueFixture) cut(t *testing.T) *servingauth.Cut {
	t.Helper()
	image, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut, err := servingauth.CaptureImage(t.Context(), image, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	return cut
}

func (f *externalIssueFixture) view(t *testing.T) *servingauth.View {
	t.Helper()
	v, err := servingauth.Prepare(t.Context(), f.cut(t), f.deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Release)
	return v
}

func (f *externalIssueFixture) proof(t *testing.T, at time.Time, username string, groups ...string) servingauth.ExternalLoginProof {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.key}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": "test", "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"iss": f.provider.Spec.Issuer, "sub": "independent-subject", "aud": "cli", "iat": at.Unix(), "exp": at.Add(5 * time.Minute).Unix(), "preferred_username": username, "name": "Corporate User", "email": "user@example.test", "groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := token.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.view(t).ProviderInfo("corp")
	if err != nil {
		t.Fatal(err)
	}
	return servingauth.ExternalLoginProof{Provider: info.Descriptor.Ref, SpecIdentity: info.Descriptor.SpecIdentity, ResourceIdentity: info.ResourceIdentity, RawIDToken: wire, Mode: oidccore.BearerIDToken}
}

func issuanceClock(first, second time.Time) func() time.Time {
	read := false
	return func() time.Time {
		if read {
			return second
		}
		read = true
		return first
	}
}

func (f *externalIssueFixture) issue(t *testing.T, proof servingauth.ExternalLoginProof, now time.Time) Enrollment {
	t.Helper()
	var enrolled Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		enrolled, err = IssueExternal(ctx, tx, f.deps, proof, issuanceClock(now, now.Add(time.Millisecond)))
		return err
	})
	return enrolled
}

func TestExternalIssuanceAtomicIdentityAndPostcommitCookie(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	now := f.now.Add(time.Second)
	proof := f.proof(t, now, "alice", "ops", "keep")
	old := f.view(t)
	before, _ := f.st.ReadGeneration(t.Context())
	boom := errors.New("failed outer preauth or audit write")
	var aborted Enrollment
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		aborted, err = IssueExternal(ctx, tx, f.deps, proof, issuanceClock(now, now.Add(time.Millisecond)))
		if err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || len(ownerState(t, f.st).ExternalIdentities) != 0 {
		t.Fatal("failed issuance leaked identity, session or revision")
	}
	if _, _, err := aborted.Seal(f.view(t), now.Add(time.Second)); err == nil {
		t.Fatal("rolled-back enrollment issued cookie")
	}
	enrolled := f.issue(t, proof, now)
	if _, _, err := enrolled.Seal(old, now.Add(time.Second)); err == nil {
		t.Fatal("precommit view issued cookie")
	}
	cookie, session, err := enrolled.Seal(f.view(t), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s := ownerState(t, f.st)
	if len(s.ExternalIdentities) != 1 || len(s.ExternalSessions) != 1 || len(s.ExternalIdentities[0].UID) != 32 || s.ExternalIdentities[0].UID == string(f.provider.UID) || s.ExternalIdentities[0].UID == string(f.user.UID) || session.Principal.AccountUID != s.ExternalIdentities[0].UID || session.Principal.External == nil || !slices.Equal(session.RolesSnapshot, []string{"viewer"}) {
		t.Fatal("identity/session authority was not independently enrolled")
	}
	if !session.ExpiresAt.Equal(now.Add(time.Millisecond+10*time.Minute)) || !session.IdleExpiresAt.Equal(now.Add(time.Millisecond+8*time.Minute)) {
		t.Fatal("canonical session lifetime ignored")
	}
	if _, err := f.view(t).CookieSession(cookie, now.Add(6*time.Minute)); err != nil {
		t.Fatal("enrollment incorrectly expired with original ID token", err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	if _, err := f.view(t).CookieSession(cookie, now.Add(time.Minute)); err != nil {
		t.Fatal("persisted external session failed after restart", err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		state, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := state.Revoke(enrolled.ID()); err != nil {
			return err
		}
		return state.Commit(ctx)
	})
	if _, err := f.view(t).CookieSession(cookie, now.Add(time.Minute)); err == nil {
		t.Fatal("revoked external session survived")
	}
}

func TestExternalSyncFloorsPreventRemovalRegrantAndUsernameRevival(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	now := f.now.Add(time.Second)
	oldProof := f.proof(t, now, "alice", "ops", "keep")
	first := f.issue(t, oldProof, now)
	oldCookie, _, err := first.Seal(f.view(t), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	uid := ownerState(t, f.st).ExternalIdentities[0].UID
	narrowAt := now.Add(10 * time.Second)
	narrow := f.proof(t, narrowAt, "alice", "keep")
	before, _ := f.st.ReadGeneration(t.Context())
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueExternal(ctx, tx, f.deps, narrow, func() time.Time { return narrowAt })
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("same-time issuance fabricated a floor bypass", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("failed sync retained a group removal/floor")
	}
	second := f.issue(t, narrow, narrowAt)
	s := ownerState(t, f.st)
	if s.ExternalIdentities[0].UID != uid || !slices.Equal(s.ExternalIdentities[0].Groups, []string{"corp:keep"}) || len(s.AccountFloors) != 1 || !s.AccountFloors[0].ValidAfter.Equal(narrowAt) {
		t.Fatal("sync lost exact UID, groups or persistent floor")
	}
	if _, err := f.view(t).CookieSession(oldCookie, narrowAt.Add(time.Second)); err == nil {
		t.Fatal("old credential survived narrowed identity")
	}
	secondCookie, _, err := second.Seal(f.view(t), narrowAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueExternal(ctx, tx, f.deps, oldProof, func() time.Time { return narrowAt.Add(time.Second) })
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("old source JWT minted fresh cookie past account floor", err)
	}
	for n, username := range []string{"renamed", "alice"} {
		at := narrowAt.Add(time.Duration(n+1) * 10 * time.Second)
		f.issue(t, f.proof(t, at, username, "ops", "keep"), at)
	}
	for _, cookie := range []string{oldCookie, secondCookie} {
		if _, err := f.view(t).CookieSession(cookie, narrowAt.Add(time.Minute)); err == nil {
			t.Fatal("username/group regrant revived prior credential")
		}
	}
}

func TestExternalCreationSnapshotWithoutSyncAndProofExpiryDuringIssuance(t *testing.T) {
	f := newExternalIssueFixture(t, false)
	now := f.now.Add(time.Second)
	proof := f.proof(t, now, "alice", "ops")
	before, _ := f.st.ReadGeneration(t.Context())
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueExternal(ctx, tx, f.deps, proof, issuanceClock(now, now.Add(6*time.Minute)))
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("expired proof issued new session", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("expired proof retained identity mutation")
	}
	f.issue(t, proof, now)
	initial := ownerState(t, f.st).ExternalIdentities[0]
	if !slices.Equal(initial.Groups, []string{"corp:ops"}) || initial.GroupsVerifiedAt == nil {
		t.Fatal("creation did not persist genuine initial group authority")
	}
	at := now.Add(10 * time.Second)
	f.issue(t, f.proof(t, at, "alice", "keep"), at)
	current := ownerState(t, f.st).ExternalIdentities[0]
	if !slices.Equal(current.Groups, initial.Groups) || !current.GroupsVerifiedAt.Equal(*initial.GroupsVerifiedAt) {
		t.Fatal("disabled group sync replaced creation snapshot")
	}
}

func TestConcurrentExternalIssuanceKeepsOneIndependentIdentity(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	now := f.now.Add(time.Second)
	proof := f.proof(t, now, "alice", "ops")
	type result struct {
		enrollment Enrollment
		err        error
	}
	done := make(chan result, 4)
	for range cap(done) {
		go func() {
			var enrolled Enrollment
			err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var err error
				enrolled, err = IssueExternal(ctx, tx, f.deps, proof, issuanceClock(now, now.Add(time.Millisecond)))
				return err
			})
			done <- result{enrolled, err}
		}()
	}
	ids := map[string]bool{}
	for range cap(done) {
		got := <-done
		if got.err != nil || ids[got.enrollment.ID()] {
			t.Fatal("concurrent enrollment failed or reused session ID", got.err)
		}
		ids[got.enrollment.ID()] = true
	}
	state := ownerState(t, f.st)
	if len(state.ExternalIdentities) != 1 || len(state.ExternalSessions) != cap(done) {
		t.Fatal("concurrent commands duplicated identity or lost session")
	}
	for _, session := range state.ExternalSessions {
		if session.Account.UID != state.ExternalIdentities[0].UID {
			t.Fatal("session bound to a discarded identity")
		}
	}
}

func TestExternalProofExpiresAfterEnrollmentBeforeCommandCommit(t *testing.T) {
	for _, regress := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "clock-regression"}[regress], func(t *testing.T) {
			f := newExternalIssueFixture(t, true)
			now := f.now.Add(time.Second)
			proof := f.proof(t, now, "alice", "ops")
			before, err := f.st.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			clock := now
			var pending Enrollment
			err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var err error
				pending, err = IssueExternal(ctx, tx, f.deps, proof, func() time.Time { return clock })
				if err != nil {
					return err
				}
				// Additional command work may outlive the accepted JWT, even
				// though the enrollment itself completed while it was valid.
				clock = now.Add(5 * time.Minute)
				if regress {
					clock = now.Add(-time.Nanosecond)
				}
				return nil
			})
			if !errors.Is(err, servingauth.ErrCredentials) || pending.ID() == "" {
				t.Fatal("final proof check did not reject staged enrollment", err)
			}
			after, err := f.st.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("expired enrollment retained identity, session or revision", err)
			}
			if _, _, err := pending.Seal(f.view(t), now); err == nil {
				t.Fatal("rolled-back enrollment produced a cookie")
			}
		})
	}
}
