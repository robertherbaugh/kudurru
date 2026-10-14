package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMissingProviderRootRetainsDNSAndGatesCompletePublication(t *testing.T) {
	f := newFixture(t)
	var root json.RawMessage
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		root, err = tx.GetSection(ctx, "security", providerregistry.RootKey)
		if err != nil {
			return err
		}
		if _, err := tx.Apply(ctx, settings("192.0.2.22"), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		return tx.DeleteSection(ctx, "security", providerregistry.RootKey)
	})
	if _, _, err := f.store.ReadServingCandidate(t.Context()); !errors.Is(err, providerregistry.ErrManifest) {
		t.Fatal("missing root obtained publication token", err)
	}
	if lease, err := f.pub.CaptureHTTP(t.Context()); !errors.Is(err, serving.ErrPending) {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("HTTP accepted incomplete security state", err)
	}
	wantIP(t, query(t, f.chain, nil, "blocked.example."), "192.0.2.11")
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "security", replication.Entry{Key: providerregistry.RootKey, Data: root})
	})
	f.waitCurrent(t)
	wantIP(t, query(t, f.chain, nil, "blocked.example."), "192.0.2.22")
}

func TestBuilderDerivesConfigProviderPrecedenceFromActualRegistry(t *testing.T) {
	f := newFixture(t)
	f.stop()
	var descriptor servingauth.ProviderDescriptor
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		spec := public.IdentityProviderSpec{Issuer: "https://config.example/issuer", ClientID: "browser", ClientSecretRef: &public.CredentialReference{Env: "BUILDER_PROVIDER_TEST"}}
		if err := security.ReconcileProviderConfigs(ctx, tx, []providerregistry.DesiredConfig{{ID: "corp", Spec: spec}}, f.now); err != nil {
			return err
		}
		spec.Issuer = "https://shadowed.example/issuer"
		if _, err := tx.Apply(ctx, &public.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp"}, Spec: spec}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		registry, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, registry.ConfigSources())
		if err != nil {
			return err
		}
		descriptor = sources[0]
		return nil
	})
	opts := f.builder.opts
	capture := opts.Capture
	opts.Capture = func(ctx context.Context, image *replication.Image) (Dependencies, error) {
		d, err := capture(ctx, image)
		// Local omission must not expose the shadowed object. The explicit
		// unavailable resource still owns the registry's exact config identity.
		d.Auth.ConfigProviders = []servingauth.ProviderSource{}
		d.Auth.ProviderResources = []servingauth.ProviderResource{{Ref: descriptor.Ref, SpecIdentity: descriptor.SpecIdentity, PolicyIdentity: descriptor.PolicyIdentity, Availability: servingauth.ProviderUnavailable, TrustIdentity: "test-unavailable-v1", UnavailableReason: "host-policy"}}
		return d, err
	}
	builder, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	image, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Prepare(t.Context(), serving.Candidate{Image: image, Token: token})
	if err != nil {
		t.Fatal("persisted config did not preserve unavailable precedence", err)
	}
	for _, part := range prepared.Components {
		if part.Release != nil {
			defer part.Release()
		}
		if part.Name == "auth" {
			info, err := part.Value.(*servingauth.View).ProviderInfo("corp")
			if err != nil || info.Descriptor.Ref != descriptor.Ref || info.Availability != servingauth.ProviderUnavailable {
				t.Fatal("auth owner replaced configured identity", info, err)
			}
		}
	}
}
