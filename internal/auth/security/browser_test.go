package security

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func browserFixture(t *testing.T) (*externalIssueFixture, browseroidc.State, servingauth.BrowserAttempt) {
	t.Helper()
	f := newExternalIssueFixture(t, true)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		p := f.provider.DeepCopy()
		p.Spec.RedirectURL = "https://dns.example/api/v1/auth/oidc/corp/callback"
		out, err := tx.Apply(ctx, p, store.ApplyOptions{Manager: store.ManagerAPI})
		if err == nil {
			f.provider = out.(*public.IdentityProvider)
		}
		return err
	})
	sources, err := servingauth.ProviderSources(t.Context(), f.cut(t), f.deps.ConfigProviders)
	if err != nil || len(sources) != 1 {
		t.Fatal("browser source", err)
	}
	f.deps.ProviderResources[0].SpecIdentity = sources[0].SpecIdentity
	f.deps.ProviderResources[0].PolicyIdentity = sources[0].PolicyIdentity
	state, err := browseroidc.New("corp", "/zones", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.view(t).ProviderInfo("corp")
	if err != nil {
		t.Fatal(err)
	}
	a := servingauth.BrowserAttempt{FormatVersion: 1, ID: state.ID(), Provider: info.Descriptor.Ref,
		SpecIdentity: info.Descriptor.SpecIdentity, ResourceIdentity: info.ResourceIdentity,
		ClientID: info.Descriptor.Spec.ClientID, RedirectURI: info.Descriptor.Spec.RedirectURL,
		IssuedAt: state.IssuedAt(), ExpiresAt: state.ExpiresAt(), PayloadHash: state.PayloadHash()}
	return f, state, a
}

func browserProof(t *testing.T, f *externalIssueFixture, state browseroidc.State, at time.Time) servingauth.ExternalLoginProof {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.key}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": "test", "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"iss": f.provider.Spec.Issuer, "sub": "browser-subject", "aud": "browser", "iat": at.Unix(), "exp": at.Add(20 * time.Minute).Unix(), "nonce": state.Nonce(), "preferred_username": "browser-user", "groups": []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.view(t).ProviderInfo("corp")
	if err != nil {
		t.Fatal(err)
	}
	return servingauth.ExternalLoginProof{Provider: info.Descriptor.Ref, SpecIdentity: info.Descriptor.SpecIdentity, ResourceIdentity: info.ResourceIdentity,
		RawIDToken: wire, Mode: oidccore.BrowserIDToken, Browser: oidccore.BrowserProof{Nonce: state.Nonce()}}
}

func unchangedBrowserImage(t *testing.T, st *sqlite.Store, before *replication.Image) {
	t.Helper()
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
		t.Fatal("failed browser command changed durable state", err)
	}
}

func TestBrowserPendingPostcommitSealingAndAtomicConsumption(t *testing.T) {
	f, state, a := browserFixture(t)
	now := a.IssuedAt
	clock := func() time.Time { return now }
	old := f.view(t)
	if _, err := old.SealBrowserPreauth(state, now); err == nil {
		t.Fatal("precommit view sealed unadmitted state")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error { return BeginBrowser(ctx, tx, f.deps, a, clock) })
	view := f.view(t)
	cookie, err := view.SealBrowserPreauth(state, now)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := view.OpenBrowserPreauth(cookie, now)
	if err != nil || opened.PayloadHash() != state.PayloadHash() {
		t.Fatal("postcommit pending cookie failed", err)
	}
	proof := browserProof(t, f, state, now)
	before, _ := f.st.ReadGeneration(t.Context())
	fault := errors.New("after browser issuance")
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := IssueBrowser(ctx, tx, f.deps, a, proof, clock); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	unchangedBrowserImage(t, f.st, before)
	var enrolled Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		enrolled, err = IssueBrowser(ctx, tx, f.deps, a, proof, clock)
		return err
	})
	s := ownerState(t, f.st)
	if len(s.BrowserAttempts) != 0 || len(s.ExternalSessions) != 1 || len(s.ExternalIdentities) != 1 {
		t.Fatal("consumption and enrollment were not atomic")
	}
	if _, _, err := enrolled.Seal(view, now); err == nil {
		t.Fatal("old pending view sealed enrollment")
	}
	if _, _, err := enrolled.Seal(f.view(t), now); err != nil {
		t.Fatal("postcommit enrollment failed", err)
	}
	if _, err := f.view(t).OpenBrowserPreauth(cookie, now); err == nil {
		t.Fatal("consumed cookie reopened in current view")
	}
	// A retained original view can still read its old immutable row. Current
	// admission must refuse it, including after database close/reopen.
	if _, err := view.OpenBrowserPreauth(cookie, now); err != nil {
		t.Fatal("held view changed", err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	before, _ = f.st.ReadGeneration(t.Context())
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueBrowser(ctx, tx, f.deps, a, proof, clock)
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("restart revived consumed attempt", err)
	}
	unchangedBrowserImage(t, f.st, before)
}

func TestBrowserClockAndPayloadFailuresRollbackPendingAndEnrollment(t *testing.T) {
	for _, scenario := range []string{"start-expiry", "start-backward", "callback-expiry", "callback-backward", "payload", "nonce", "floor", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			f, state, a := browserFixture(t)
			now := a.IssuedAt.Add(time.Second)
			clock := func() time.Time { return now }
			start := scenario == "start-expiry" || scenario == "start-backward"
			if !start {
				command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error { return BeginBrowser(ctx, tx, f.deps, a, clock) })
			}
			proof := browserProof(t, f, state, a.IssuedAt)
			if scenario == "payload" {
				a.PayloadHash = servingauth.CSRFHash("different authenticated payload")
			}
			if scenario == "nonce" {
				proof.Browser.Nonce = "wrong-nonce"
			}
			before, _ := f.st.ReadGeneration(t.Context())
			outer, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := f.st.Command(outer, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if scenario == "floor" {
					s, err := Load(ctx, tx)
					if err != nil {
						return err
					}
					if err := s.InvalidateProvider(a.Provider.ID, a.Provider.UID, a.IssuedAt); err != nil {
						return err
					}
					if err := s.Commit(ctx); err != nil {
						return err
					}
				}
				if start {
					if err := BeginBrowser(ctx, tx, f.deps, a, clock); err != nil {
						return err
					}
				} else if _, err := IssueBrowser(ctx, tx, f.deps, a, proof, clock); err != nil {
					return err
				}
				switch scenario {
				case "start-expiry", "callback-expiry":
					now = a.ExpiresAt
				case "start-backward", "callback-backward":
					now = a.IssuedAt // valid for payload, but before authentication
				case "cancel":
					cancel()
				}
				return nil
			})
			if err == nil {
				t.Fatal("invalid browser command committed")
			}
			unchangedBrowserImage(t, f.st, before)
		})
	}
}

func TestBrowserConcurrentConsumptionAndExpiredCollectionNeverRevive(t *testing.T) {
	f, state, a := browserFixture(t)
	now := a.IssuedAt
	clock := func() time.Time { return now }
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error { return BeginBrowser(ctx, tx, f.deps, a, clock) })
	proof := browserProof(t, f, state, now)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			results <- f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := IssueBrowser(ctx, tx, f.deps, a, proof, clock)
				return err
			})
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, servingauth.ErrCredentials) {
			t.Fatal(err)
		}
	}
	if successes != 1 || len(ownerState(t, f.st).ExternalSessions) != 1 {
		t.Fatal("concurrent callback consumed more than once")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.CollectExpired(a.ExpiresAt.Add(time.Hour)); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	// Rewinding the clock cannot reconstruct absent durable pending authority.
	if _, err := f.view(t).BrowserAttempt(a.ID, a.IssuedAt); err == nil {
		t.Fatal("collection/clock regression revived absent row")
	}
}

func TestBrowserV3MigrationPreservesCompletePriorAuthority(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	f.issue(t, f.proof(t, f.now.Add(time.Second), "existing", "ops"), f.now.Add(time.Second))
	state := ownerState(t, f.st)
	path := filepath.Join(t.TempDir(), "v3.db")
	st := openSecurityStoreValidator(t, path, nil)
	legacySecurityVersion(t, st, state, 3)
	_ = st.Close()
	st = openSecurityStore(t, path)
	old, _ := st.ReadGeneration(t.Context())
	fault := errors.New("outer migration fault")
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := Initialize(ctx, tx); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	unchangedBrowserImage(t, st, old)
	command(t, st, Initialize)
	state.Epoch++
	if !reflect.DeepEqual(ownerState(t, st), state) {
		t.Fatal("migration altered existing floors, enrollment or identity")
	}
	current, _ := st.ReadGeneration(t.Context())
	command(t, st, Initialize)
	unchangedBrowserImage(t, st, current)
	found := false
	for _, row := range current.Payload.Security {
		if row.Key == "other-owner/value" {
			found = bytes.Equal(row.Data, []byte(`{"keep":true}`))
		}
	}
	if !found {
		t.Fatal("migration changed another owner's row")
	}
}

func TestBrowserPendingReplicaInstallAndPromotionPreserveConsumption(t *testing.T) {
	f, state, attempt := browserFixture(t)
	clock := func() time.Time { return attempt.IssuedAt }
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return BeginBrowser(ctx, tx, f.deps, attempt, clock)
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := make([]store.Kind, 0, len(runtimekinds.Private()))
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	peer, err := sqlite.Open(filepath.Join(t.TempDir(), "peer.db"), sqlite.Options{Scheme: scheme, InternalKinds: private,
		InitialMode: replication.Follower, ValidateReplicaEntry: servingauth.ValidateEntry,
		ValidateReplicaImage: func(image *replication.Image) error {
			_, err := servingauth.DecodeSecurity(image.Payload.Security)
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	initial, _ := f.st.ReadGeneration(t.Context())
	if err := peer.Enroll(t.Context(), initial.Term); err != nil {
		t.Fatal(err)
	}
	install := func(image *replication.Image) {
		t.Helper()
		validated, err := peer.ValidateImage(t.Context(), *image)
		if err != nil {
			t.Fatal(err)
		}
		authority := peer.Authority()
		if _, err := peer.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: authority.Term, ExpectedInstalled: authority.Installed}); err != nil {
			t.Fatal(err)
		}
	}
	install(initial)
	if got := ownerState(t, peer); len(got.BrowserAttempts) != 1 || got.BrowserAttempts[0] != attempt {
		t.Fatal("complete replica lost exact pending row")
	}
	before, _ := peer.ReadGeneration(t.Context())
	if err := peer.Command(t.Context(), before.Term, Initialize); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower admitted mutation", err)
	}
	unchangedBrowserImage(t, peer, before)
	proof := browserProof(t, f, state, attempt.IssuedAt)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueBrowser(ctx, tx, f.deps, attempt, proof, clock)
		return err
	})
	consumed, _ := f.st.ReadGeneration(t.Context())
	install(consumed)
	authority := peer.Authority()
	if _, err := peer.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: authority.Term, ExpectedInstalled: authority.Installed, RequestID: "browser-promotion"}); err != nil {
		t.Fatal(err)
	}
	before, _ = peer.ReadGeneration(t.Context())
	err = peer.Command(t.Context(), peer.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueBrowser(ctx, tx, f.deps, attempt, proof, clock)
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("promotion revived already consumed preauth", err)
	}
	unchangedBrowserImage(t, peer, before)
}
