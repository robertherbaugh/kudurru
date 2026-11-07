package providerresources

import (
	"bytes"
	"context"
	"crypto/subtle"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// CLIInfo contains detached public metadata from one exact admitted capability.
type CLIInfo struct {
	Source                                             clioidc.Source
	DisplayName, AuthorizationURL, TokenURL, DeviceURL string
	Scopes                                             []string
	Browser, Device                                    bool
}

// CLI borrows its original complete lease. It grants neither identity nor writes.
type CLI struct {
	base *Browser
	info CLIInfo
}

func (*CLI) String() string { return "private captured CLI capability" }

// GoString redacts capability internals.
func (c *CLI) GoString() string { return c.String() }

// MarshalJSON prevents incidental capability disclosure.
func (*CLI) MarshalJSON() ([]byte, error) { return []byte(`"private CLI capability"`), nil }

// CLI borrows one exact ready public-client capability.
func (l *Lease) CLI(ref servingauth.ProviderRef, resourceID string) (*CLI, error) {
	if l == nil || l.owner == nil || !l.Current() {
		return nil, ErrStale
	}
	cut := l.snapshot.browser
	if cut == nil || cut.host == nil || !cut.host.allowed {
		return nil, ErrUnavailable
	}
	for _, e := range l.snapshot.entries {
		if e.descriptor.Ref != ref {
			continue
		}
		id, err := servingauth.ProviderResourceIdentity(e.descriptor, e.resource)
		if err != nil || id != resourceID || e.resource.Availability != servingauth.ProviderReady {
			return nil, ErrStale
		}
		browser := e.metadata.cliBrowser && clioidc.HTTPS(e.metadata.authorization)
		device := e.metadata.cliDevice && clioidc.HTTPS(e.metadata.device)
		if e.descriptor.Spec.CLIClientID == "" || (!browser && !device) || !clioidc.HTTPS(e.descriptor.Issuer) || !clioidc.HTTPS(e.metadata.token) {
			return nil, ErrUnavailable
		}
		resolver := cut.host.objects
		if ref.Origin == servingauth.ProviderConfig {
			if cut.host.configSpecs[ref.ID] != e.descriptor.SpecIdentity {
				return nil, ErrStale
			}
			resolver = cut.host.configs[ref.ID]
		}
		if resolver == nil || e.descriptor.Spec.ClientSecretRef == nil {
			return nil, ErrUnavailable
		}
		for _, a := range cut.authorities {
			if a.Active && a.Provider == ref && a.SpecIdentity == e.descriptor.SpecIdentity && hash(a.Binding) == e.resource.TrustIdentity {
				return &CLI{base: &Browser{lease: l, resolver: resolver, ref: e.descriptor, binding: a.Binding}, info: CLIInfo{Source: clioidc.Source{Provider: ref.ID, UID: ref.UID, Origin: string(ref.Origin), Spec: e.descriptor.SpecIdentity, Policy: e.descriptor.PolicyIdentity, Resource: id, Issuer: e.descriptor.Issuer, ClientID: e.descriptor.Spec.CLIClientID}, DisplayName: e.descriptor.Spec.DisplayName, AuthorizationURL: e.metadata.authorization, TokenURL: e.metadata.token, DeviceURL: e.metadata.device, Device: device, Scopes: slices.Clone(e.descriptor.Spec.Scopes), Browser: browser}}, nil
			}
		}
		return nil, ErrStale
	}
	return nil, ErrUnavailable
}

// BindCurrent retains the original publication currency check.
func (c *CLI) BindCurrent(current func() bool) (*CLI, error) {
	if c == nil || current == nil || !c.Current() || !current() {
		return nil, ErrStale
	}
	base := *c.base
	base.current = func() bool { return c.Current() && current() }
	return &CLI{&base, c.Info()}, nil
}

// Current checks the original borrowed lease and publication.
func (c *CLI) Current() bool { return c != nil && c.base.Current() }

// Info returns detached public metadata.
func (c *CLI) Info() CLIInfo {
	if c == nil {
		return CLIInfo{}
	}
	i := c.info
	i.Scopes = slices.Clone(i.Scopes)
	return i
}

// Check resolves only the installed credential from the original captured
// origin outside all guards. It compares fingerprints without exporting bytes.
func (c *CLI) Check(ctx context.Context) error {
	if !c.Current() {
		return ErrStale
	}
	ctx, cancel := context.WithTimeout(ctx, c.base.lease.owner.opts.OperationTimeout)
	defer cancel()
	started := c.base.lease.owner.opts.Now()
	v, e := c.base.resolver.Resolve(ctx, *c.base.ref.Spec.ClientSecretRef)
	defer clear(v.Bytes)
	if e != nil || len(v.Bytes) > MaxCredentialBytes || len(bytes.TrimSpace(v.Bytes)) == 0 {
		return ErrUnavailable
	}
	if subtle.ConstantTimeCompare([]byte(v.Fingerprint), []byte(c.base.binding.CredentialFingerprint)) != 1 || ctx.Err() != nil || !c.Current() || c.base.lease.owner.opts.Now().Before(started) {
		return ErrStale
	}
	return nil
}
