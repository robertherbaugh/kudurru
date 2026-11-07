package providerresources

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func TestEmptyOwnerPureCaptureExclusionAndHostABA(t *testing.T) {
	h, err := NewHost(HostOptions{Identity: "host", CredentialPolicy: "credential-policy", EntitlementIdentity: "community", FingerprintKeyID: "key-v1", ConfigSources: []servingauth.ProviderSource{}, AllowedOrigins: []string{}, LocalOrigins: []string{}}) //nolint:gosec // Test policy identities are not credential material.
	if err != nil {
		t.Fatal(err)
	}
	o, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	d := Desired{RegistryDigest: hash("registry"), Host: h, Sources: []servingauth.ProviderDescriptor{}, Authorities: []providerregistry.AuthorityRecord{}}
	if err = o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	lease, err := o.Acquire(t.Context(), d)
	if err != nil || !lease.Current() {
		t.Fatal(err)
	}
	if err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
		defer cancel()
		if err := o.SetDesired(ctx, d); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("replacement crossed read exclusion", err)
		}
		return p.Match(d)
	}); err != nil {
		t.Fatal(err)
	}
	h2, err := NewHost(HostOptions{Identity: "host", CredentialPolicy: "credential-policy", EntitlementIdentity: "community", FingerprintKeyID: "key-v1", ConfigSources: []servingauth.ProviderSource{}, AllowedOrigins: []string{}, LocalOrigins: []string{}}) //nolint:gosec // Test policy identities are not credential material.
	if err != nil {
		t.Fatal(err)
	}
	d2 := d
	d2.Host = h2
	if err = o.SetDesired(t.Context(), d2); err != nil {
		t.Fatal(err)
	}
	if lease.Current() {
		t.Fatal("replacement capability reused host identity")
	}
	if err = o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if lease.Current() {
		t.Fatal("ABA revived lease")
	}
	lease.Release()
	if lease.Resources() != nil || lease.Identity() != "" {
		t.Fatal("released lease")
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Acquire(t.Context(), d); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestRetireInvalidatesPinAndCandidateAcrossIdenticalDesiredABA(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	d := Desired{RegistryDigest: hash("registry"), Host: f.host(t, s, "test-only", true), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	c := candidate(t, o)
	if err := o.WithReplacement(t.Context(), func(r *Replacement) error {
		return r.WithCurrent(func(p *Pinned) error {
			r.Retire()
			if err := p.CurrentAt(f.now()); !errors.Is(err, ErrStale) {
				t.Fatal("retired pin", err)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Acquire(t.Context(), d); !errors.Is(err, ErrStale) {
		t.Fatal("retired cut captured", err)
	}
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := c.Match(d); !errors.Is(err, ErrStale) {
		t.Fatal("ABA revived authority provenance", err)
	}
}
func TestNestedSpecOutputsActuallyDetach(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	d := Desired{RegistryDigest: hash("registry"), Host: f.host(t, s, "test-only", true), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	c := candidate(t, o)
	receipt := c.Receipt()
	receipt.Source.Spec.ClientSecretRef.Env = "mutated"
	receipt.Source.Spec.Scopes[0] = "mutated"
	receipt.Source.Spec.ClaimMappings.Username.Prefix = new(string)
	if c.Receipt().Source.Spec.ClientSecretRef.Env != "TEST_CREDENTIAL" || c.Receipt().Source.Spec.Scopes[0] == "mutated" {
		t.Fatal("nested candidate alias")
	}
	d.Sources[0].Spec.ClientSecretRef.Env = "caller-mutated"
	if o.desired.Sources[0].Spec.ClientSecretRef.Env != "TEST_CREDENTIAL" {
		t.Fatal("nested desired alias")
	}
}
