package providerentitlement_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pe "github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var validatorOnce sync.Once
var localValidator *validator.Validator
var validatorErr error

func open(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	validatorOnce.Do(func() { localValidator, validatorErr = validator.New() })
	if validatorErr != nil {
		t.Fatal(validatorErr)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := []store.Kind{}
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: localValidator, InitialMode: mode, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writer(t *testing.T, s *sqlite.Store) (context.Context, serving.TermActivation) {
	t.Helper()
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	return sqlite.WithWriterToken(t.Context(), token), serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}
}

func command(t *testing.T, s *sqlite.Store, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	ctx, target := writer(t, s)
	if err := s.Command(ctx, target.Term, fn); err != nil {
		t.Fatal(err)
	}
}

func image(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	i, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func registry(t *testing.T, s *sqlite.Store) *providerregistry.Registry {
	t.Helper()
	i := image(t, s)
	r, err := providerregistry.Decode(i.Payload.Security, i.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newStore(t *testing.T, at time.Time) *sqlite.Store {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "state.db"), replication.Standalone)
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := runtimeowners.Initialize(ctx, tx, at); err != nil {
			return err
		}
		if _, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		spec := publicv1.IdentityProviderSpec{Issuer: "https://issuer.example/realm", ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "ENTITLEMENT_TEST"}}
		if _, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "object", CreationTimestamp: metav1.NewTime(at.Add(-time.Minute))}, Spec: spec}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		spec.Issuer = "https://config.example/realm"
		return security.ReconcileProviderConfigs(ctx, tx, []providerregistry.DesiredConfig{{ID: "corp", Spec: spec}}, at.Add(-time.Minute))
	})
	return s
}

func pending(t *testing.T, s *sqlite.Store, p string) string {
	t.Helper()
	ctx, target := writer(t, s)
	e, err := pe.NewPending(ctx, target, p)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Receipts here are explicitly trusted TEST root admission inputs; the pure
// entitlement package never discovers, verifies provider candidates or stages them.
func receipts(t *testing.T, s *sqlite.Store, e string) []providerregistry.DesiredAuthority {
	t.Helper()
	out := []providerregistry.DesiredAuthority{}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		r, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
		if err != nil {
			return err
		}
		for _, source := range sources {
			out = append(out, providerregistry.DesiredAuthority{Source: source, Binding: providerregistry.AuthorityBinding{Profile: "provider-authority/v1", FingerprintKeyID: "test-key", CredentialFingerprint: strings.Repeat("a", 64), CredentialPolicy: "credentials-v1", NetworkPolicy: "network-v1", MetadataBinding: "metadata-v1", EntitlementIdentity: e}})
		}
		return nil
	})
	return out
}

func stage(t *testing.T, s *sqlite.Store, r []providerregistry.DesiredAuthority, at time.Time) {
	t.Helper()
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.AcceptProviderAuthorities(ctx, tx, r, at)
	})
}

func TestPendingNoGrantActualReceiptResetFloorsRollbackAndReactivation(t *testing.T) {
	f := newLicenseFixture(t)
	m := f.manager(t, f.claims(), "test")
	p := profile(t, m)
	s := newStore(t, f.at)
	cluster := s.Authority().Term.ClusterID
	r := registry(t, s)
	if r.ClusterID() != cluster || (*providerregistry.Registry)(nil).ClusterID() != "" {
		t.Fatal("read-only registry cluster accessor incorrect")
	}
	before := image(t, s)
	e := pending(t, s, p)
	if value, err := pe.Parse(e, cluster, p); err != nil || value != e {
		t.Fatal("valid pending structure denied", err)
	}
	if _, err := pe.Installed(r, cluster, p); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("pending string granted empty registry", err)
	}
	if !before.Generation().Same(image(t, s).Generation()) {
		t.Fatal("pure pending constructor wrote durable state")
	}
	accepted := receipts(t, s, e)
	if len(accepted) != 2 {
		t.Fatal("missing actual both-origin source projection")
	}
	stage(t, s, accepted, f.at)
	old := registry(t, s)
	if actual, err := pe.Installed(old, cluster, p); err != nil || actual != e {
		t.Fatal("committed receipt not selected", err)
	}
	first := image(t, s)
	stage(t, s, accepted, f.at.Add(time.Second))
	if !first.Generation().Same(image(t, s).Generation()) {
		t.Fatal("identical current receipts changed durable generation")
	}
	e2 := pending(t, s, p)
	changed := receipts(t, s, e2)
	abort := errors.New("test final admission abort")
	ctx, target := writer(t, s)
	if err := s.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := security.AcceptProviderAuthorities(ctx, tx, changed, f.at.Add(2*time.Second)); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return abort })
	}); err != abort {
		t.Fatal("actual final commit hook did not abort", err)
	}
	if !first.Generation().Same(image(t, s).Generation()) {
		t.Fatal("failed activation persisted staged floors/receipts")
	}
	stage(t, s, []providerregistry.DesiredAuthority{}, f.at.Add(3*time.Second))
	if _, err := pe.Installed(registry(t, s), cluster, p); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("withdrawn historical receipts granted entitlement", err)
	}
	withdrawn, err := servingauth.DecodeSecurity(image(t, s).Payload.Security)
	if err != nil || len(withdrawn.ProviderFloors) != 2 {
		t.Fatal("reset omitted both-origin exact UID floors", err)
	}
	stage(t, s, changed, f.at.Add(4*time.Second))
	current := registry(t, s)
	if actual, err := pe.Installed(current, cluster, p); err != nil || actual != e2 || actual == e {
		t.Fatal("reactivation did not select new committed nonce", err)
	}
	state, err := servingauth.DecodeSecurity(image(t, s).Payload.Security)
	if err != nil || len(state.ProviderFloors) != 2 {
		t.Fatal(err)
	}
	for _, floor := range state.ProviderFloors {
		matched := false
		for _, receipt := range changed {
			matched = matched || (floor.Provider == receipt.Source.Ref.ID && floor.UID == receipt.Source.Ref.UID)
		}
		if !matched || !floor.ValidAfter.Equal(f.at.Add(4*time.Second)) {
			t.Fatal("reactivation lost exact monotonic issuance floor")
		}
	}
	// Old immutable image remains its exact cut; root must use concrete CURRENT
	// registry for commands and enforce the separate follower/host fence.
	if actual, err := pe.Installed(old, cluster, p); err != nil || actual != e {
		t.Fatal("pure helper mutated held old registry", err)
	}
}

func TestInstalledActualRegistryClusterMixedProfilesVersionsAndHistory(t *testing.T) {
	f := newLicenseFixture(t)
	p := profile(t, f.manager(t, f.claims(), "test"))
	a, b := newStore(t, f.at), newStore(t, f.at)
	e := pending(t, a, p)
	// A real OTHER-cluster root admits structurally valid receipts containing E
	// from A. The helper must bind actual registry cluster too, not caller/E alone.
	stage(t, b, receipts(t, b, e), f.at)
	for _, cluster := range []string{a.Authority().Term.ClusterID, b.Authority().Term.ClusterID} {
		if _, err := pe.Installed(registry(t, b), cluster, p); !errors.Is(err, pe.ErrContinuity) {
			t.Fatal("receipt cluster spoof ignored actual registry cluster", err)
		}
	}
	cluster := a.Authority().Term.ClusterID
	for name, change := range map[string]func([]providerregistry.DesiredAuthority){
		"mixed-activation": func(r []providerregistry.DesiredAuthority) { r[1].Binding.EntitlementIdentity = pending(t, a, p) },
		"mixed-profile": func(r []providerregistry.DesiredAuthority) {
			r[1].Binding.EntitlementIdentity = pending(t, a, strings.Repeat("b", 64))
		},
		"unknown-version": func(r []providerregistry.DesiredAuthority) {
			r[1].Binding.EntitlementIdentity = strings.Replace(e, "/v1/", "/v9/", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := receipts(t, a, e)
			change(r)
			stage(t, a, r, f.at.Add(time.Second))
			if _, err := pe.Installed(registry(t, a), cluster, p); !errors.Is(err, pe.ErrContinuity) {
				t.Fatal("mixed/unknown cluster authority accepted", err)
			}
		})
	}
	stage(t, a, receipts(t, a, e), f.at.Add(2*time.Second))
	if _, err := pe.Installed(registry(t, a), cluster, strings.Repeat("b", 64)); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("incompatible verified local profile accepted", err)
	}
	stage(t, a, []providerregistry.DesiredAuthority{}, f.at.Add(3*time.Second))
	if _, err := pe.Installed(registry(t, a), cluster, p); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("inactive mixed historical inventory adopted")
	}
}

func TestActualRestartFollowerInstallPromotionAndClosedStoreReadFreeHelpers(t *testing.T) {
	f := newLicenseFixture(t)
	p := profile(t, f.manager(t, f.claims(), "primary source"))
	s := newStore(t, f.at)
	e := pending(t, s, p)
	stage(t, s, receipts(t, s, e), f.at)
	stage(t, s, []providerregistry.DesiredAuthority{}, f.at.Add(time.Second))
	e = pending(t, s, p)
	stage(t, s, receipts(t, s, e), f.at.Add(2*time.Second))
	before := image(t, s)
	cluster := before.Term.ClusterID
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, replication.Standalone)
	if actual, err := pe.Installed(registry(t, s), cluster, p); err != nil || actual != e {
		t.Fatal("restart changed committed entitlement evidence", err)
	}
	state, err := servingauth.DecodeSecurity(image(t, s).Payload.Security)
	if err != nil || len(state.ProviderFloors) != 2 {
		t.Fatal("restart lost exact both-origin revocation floors", err)
	}
	peer := open(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := peer.Enroll(t.Context(), before.Term); err != nil {
		t.Fatal(err)
	}
	valid, err := peer.ValidateImage(t.Context(), *before)
	if err != nil {
		t.Fatal(err)
	}
	a := peer.Authority()
	installed, err := peer.InstallReplica(t.Context(), valid, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed})
	if err != nil {
		t.Fatal(err)
	}
	peerProfile := profile(t, f.manager(t, f.claims(), "follower source"))
	if actual, err := pe.Installed(registry(t, peer), cluster, peerProfile); err != nil || actual != e {
		t.Fatal("independently verified follower profile could not select actual receipt", err)
	}
	if !installed.Same(image(t, peer).Generation()) {
		t.Fatal("receipt selection wrote follower state")
	}
	peerState, err := servingauth.DecodeSecurity(image(t, peer).Payload.Security)
	if err != nil || len(peerState.ProviderFloors) != 2 {
		t.Fatal("actual follower install lost revocation floors", err)
	}
	for i, floor := range state.ProviderFloors {
		if floor != peerState.ProviderFloors[i] {
			t.Fatal("follower install changed floor UID/issuance boundary")
		}
	}
	if _, err := peer.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: before.Term, ExpectedInstalled: installed, RequestID: "entitlement-test"}); err != nil {
		t.Fatal(err)
	}
	// Selection itself does NOT perform the required production promotion reset.
	// Root owns that actual transaction, then it can create/admit a new pending E.
	stage(t, peer, []providerregistry.DesiredAuthority{}, f.at.Add(time.Second))
	e2 := pending(t, peer, peerProfile)
	stage(t, peer, receipts(t, peer, e2), f.at.Add(2*time.Second))
	r := registry(t, peer)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if actual, err := pe.Installed(r, cluster, peerProfile); err != nil || actual != e2 || actual == e {
		t.Fatal("pure installed selection read closed Store or reused old activation", err)
	}
}

func TestActualLicenseDeadlineRollsBackStagedActivationFloors(t *testing.T) {
	f := newLicenseFixture(t)
	m := f.manager(t, f.claims(), "test")
	p := profile(t, m)
	s := newStore(t, f.at)
	stage(t, s, receipts(t, s, pending(t, s, p)), f.at)
	before := image(t, s)
	r := receipts(t, s, pending(t, s, p))
	ctx, target := writer(t, s)
	err := m.WithCurrent(ctx, func(c license.Cut) error {
		return s.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := security.AcceptProviderAuthorities(ctx, tx, r, f.at.Add(time.Second)); err != nil {
				return err
			}
			return tx.RequireCommitCheck(func(context.Context) error {
				f.clock.Store(f.at.Add(time.Hour + license.Grace).UnixNano())
				_, _, err := pe.Profile(c)
				return err
			})
		})
	})
	if !errors.Is(err, pe.ErrContinuity) || !before.Generation().Same(image(t, s).Generation()) {
		t.Fatal("expired captured license persisted activation or UID floors", err)
	}
}
