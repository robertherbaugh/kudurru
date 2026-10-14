package security

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInitializeOwnersAtomicRestartAndIncompleteManifestDenial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := openSecurityStoreValidator(t, path, ValidateEntry, ValidateImage)
	if _, _, err := st.ReadServingCandidate(t.Context()); err == nil {
		t.Fatal("uninitialized security owners admitted")
	}
	command(t, st, InitializeOwners)
	before, _, err := st.ReadServingCandidate(t.Context())
	if err != nil || len(before.Payload.Security) != 2 {
		t.Fatal("owners were not initialized together", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openSecurityStoreValidator(t, path, ValidateEntry, ValidateImage)
	command(t, st, InitializeOwners)
	after, _, err := st.ReadServingCandidate(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("startup reset security owners", err)
	}

	// Keep a config row while removing its manifest, simulating lost history.
	desired := []providerregistry.DesiredConfig{{ID: "corp", Spec: providerSpec()}}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ReconcileProviderConfigs(ctx, tx, desired, time.Now())
	})
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.DeleteSection(ctx, "security", providerregistry.RootKey)
	})
	broken, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReadServingCandidate(t.Context()); !errors.Is(err, providerregistry.ErrManifest) {
		t.Fatal("incomplete provider inventory admitted", err)
	}
	if err := st.Command(t.Context(), st.Authority().Term, InitializeOwners); !errors.Is(err, providerregistry.ErrManifest) {
		t.Fatal("startup recreated missing provider history", err)
	}
	after, _ = st.ReadGeneration(t.Context())
	if !broken.Generation().Same(after.Generation()) {
		t.Fatal("rejected startup changed durable state")
	}
	// Each receiving owner is required even when another owner's root is valid.
	for _, removed := range []string{servingauth.RootKey, providerregistry.RootKey} {
		image := *before
		image.Payload.Security = nil
		for _, row := range before.Payload.Security {
			if row.Key != removed {
				image.Payload.Security = append(image.Payload.Security, row)
			}
		}
		if err := replication.Seal(&image); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ValidateImage(t.Context(), image); err == nil {
			t.Fatal("replica accepted missing owner", removed)
		}
	}
}

func providerSpec() public.IdentityProviderSpec {
	return public.IdentityProviderSpec{Issuer: "https://issuer.example/realm", ClientID: "browser", ClientSecretRef: &public.CredentialReference{Env: "SECURITY_PROVIDER_TEST"}}
}

func TestProviderChangesCommitFloorsAndReceiptsAtomically(t *testing.T) {
	st := openSecurityStoreValidator(t, filepath.Join(t.TempDir(), "state.db"), ValidateEntry, ValidateImage)
	command(t, st, InitializeOwners)
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	at := time.Now().UTC().Add(-time.Minute)
	desired := []providerregistry.DesiredConfig{{ID: "corp", Spec: providerSpec()}}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ReconcileProviderConfigs(ctx, tx, desired, at)
	})
	var accepted providerregistry.DesiredAuthority
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		registry, err := currentProviderRegistry(ctx, tx)
		if err != nil {
			return err
		}
		sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, registry.ConfigSources())
		if err != nil {
			return err
		}
		accepted = providerregistry.DesiredAuthority{Source: sources[0], Binding: providerregistry.AuthorityBinding{Profile: "provider-authority/v1", FingerprintKeyID: "test-key", CredentialFingerprint: strings.Repeat("a", 64), CredentialPolicy: "credentials-v1", NetworkPolicy: "tls-v1", MetadataBinding: "metadata-v1", EntitlementIdentity: "management-v1"}}
		return AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{accepted}, at)
	})
	first, _ := st.ReadGeneration(t.Context())
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{accepted}, at.Add(time.Second))
	})
	noop, _ := st.ReadGeneration(t.Context())
	if !first.Generation().Same(noop.Generation()) {
		t.Fatal("receipt replay changed floors or manifest")
	}
	accepted.Binding.CredentialFingerprint = strings.Repeat("b", 64)
	fault := errors.New("final resource admission rejected")
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{accepted}, at.Add(2*time.Second)); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return fault })
	})
	if err != fault {
		t.Fatal("failed to reach final admission after staging floor and receipt", err)
	}
	after, _ := st.ReadGeneration(t.Context())
	if !first.Generation().Same(after.Generation()) {
		t.Fatal("final failure retained staged provider floor or receipt")
	}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{accepted}, at.Add(2*time.Second))
	})
	state := ownerState(t, st)
	if len(state.ProviderFloors) != 1 || state.ProviderFloors[0].UID != accepted.Source.Ref.UID || !state.ProviderFloors[0].ValidAfter.Equal(at.Add(2*time.Second)) {
		t.Fatal("accepted material change did not persist exact provider floor")
	}
	// The authority inventory is complete: removal must withdraw the receipt
	// and raise its floor in the same transaction without dropping history.
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{}, at.Add(3*time.Second))
	})
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		registry, err := currentProviderRegistry(ctx, tx)
		if err != nil {
			return err
		}
		if registry.Authorities()[0].Active || registry.Sources()[0].AuthorityKey != "" {
			t.Fatal("withdrawn receipt remained active")
		}
		return nil
	})
	if !ownerState(t, st).ProviderFloors[0].ValidAfter.Equal(at.Add(3 * time.Second)) {
		t.Fatal("withdrawal lost its floor")
	}
}
