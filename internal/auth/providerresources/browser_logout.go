package providerresources

import (
	"bytes"
	"context"
	"crypto/subtle"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
)

// LogoutRedirect checks the original installed credential once, outside ALL
// owner/command gates, before exposing an ID-token hint to its captured endpoint.
// There is no discovery, successor selection, provider request, retry or writer
// admission. Failure leaves the already committed local logout intact.
func (b *Browser) LogoutRedirect(ctx context.Context, l browseroidc.Logout) (string, error) {
	if ctx == nil || !b.Current() {
		return "", ErrStale
	}
	i := b.Info()
	expected := browseroidc.LogoutBinding{Provider: i.Provider.ID, ProviderUID: i.Provider.UID, Origin: string(i.Provider.Origin), SpecIdentity: i.SpecIdentity, ResourceIdentity: i.ResourceIdentity, Issuer: i.Issuer, ClientID: i.ClientID, RedirectURI: i.RedirectURI}
	if l.Binding() != expected || l.Phase() != "continue" || i.EndSessionURL == "" {
		return "", ErrInvalid
	}
	select {
	case browserOperations <- struct{}{}:
		defer func() { <-browserOperations }()
	default:
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, b.lease.owner.opts.OperationTimeout)
	defer cancel()
	started := b.lease.owner.opts.Now()
	value, err := b.resolver.Resolve(ctx, *b.ref.Spec.ClientSecretRef)
	defer clear(value.Bytes)
	if err != nil || len(value.Bytes) > MaxCredentialBytes || len(bytes.TrimSpace(value.Bytes)) == 0 {
		return "", ErrUnavailable
	}
	if subtle.ConstantTimeCompare([]byte(value.Fingerprint), []byte(b.binding.CredentialFingerprint)) != 1 || ctx.Err() != nil || !b.Current() || b.lease.owner.opts.Now().Before(started) {
		return "", ErrStale
	}
	target, err := l.RedirectURL(i.EndSessionURL, b.lease.owner.opts.Now())
	if err != nil {
		return "", ErrInvalid
	}
	if ctx.Err() != nil || !b.Current() || b.lease.owner.opts.Now().Before(started) || l.ValidAt(b.lease.owner.opts.Now()) != nil {
		return "", ErrStale
	}
	return target, nil
}
