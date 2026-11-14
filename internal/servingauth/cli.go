package servingauth

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
)

func (v *View) checkCLISource(s clioidc.Source) error {
	p, e := v.externalProvider(s.Provider)
	if e != nil {
		return e
	}
	if s != (clioidc.Source{Provider: p.descriptor.Ref.ID, UID: p.descriptor.Ref.UID, Origin: string(p.descriptor.Ref.Origin), Spec: p.descriptor.SpecIdentity, Policy: p.descriptor.PolicyIdentity, Resource: p.identity, Issuer: p.descriptor.Issuer, ClientID: p.descriptor.Spec.CLIClientID}) || s.ClientID == "" {
		return ErrCredentials
	}
	return nil
}

// SealCLIReceipt correlates a ready public source without issuing any authority.
func (v *View) SealCLIReceipt(s clioidc.Source, q clioidc.PrepareRequest, now time.Time) (string, time.Time, error) {
	if e := v.checkCLISource(s); e != nil {
		return "", time.Time{}, e
	}
	r, e := clioidc.NewReceipt(s, q, now)
	if e != nil {
		return "", time.Time{}, e
	}
	raw, e := v.cliCodec.Seal(r, now)
	return raw, r.ExpiresAt(), e
}

// OpenCLIReceipt checks source correlation only; callers still need original raw
// bearer authentication and installed credential/capability currency.
func (v *View) OpenCLIReceipt(raw string, now time.Time) (clioidc.Receipt, error) {
	if v == nil || v.released.Load() {
		return clioidc.Receipt{}, ErrCredentials
	}
	r, e := v.cliCodec.Open(raw, now)
	if e != nil {
		return r, ErrCredentials
	}
	return r, v.checkCLISource(r.Source())
}

// VerifyCLILogin requires current raw JWT admission and narrows only this login
// to the exact public CLI audience and expected nonce/at_hash. It issues nothing.
func (v *View) VerifyCLILogin(ctx context.Context, proof Credential, r clioidc.Receipt, access string, now time.Time) (auth.Principal, time.Time, error) {
	if proof.Kind != OIDCJWT || r.ValidAt(now) != nil || v.checkCLISource(r.Source()) != nil {
		return auth.Principal{}, time.Time{}, ErrCredentials
	}
	proof.ProviderID = r.Source().Provider
	p, e := v.Authenticate(ctx, proof, now)
	if e != nil {
		return auth.Principal{}, time.Time{}, e
	}
	provider, e := v.externalProvider(proof.ProviderID)
	if e != nil {
		return auth.Principal{}, time.Time{}, e
	}
	var token *oidccore.VerifiedToken
	if r.Request().Mode == "device" {
		token, e = provider.resource.Verifier.VerifyDevice(ctx, proof.Value, access, now)
	} else {
		token, e = provider.resource.Verifier.VerifyBrowser(ctx, proof.Value, oidccore.BrowserProof{Nonce: r.Request().Nonce, AccessToken: access}, now)
	}
	if e != nil {
		return auth.Principal{}, time.Time{}, ErrCredentials
	}
	if _, e = provider.policy.EvaluateCLI(ctx, token, now); e != nil {
		return auth.Principal{}, time.Time{}, ErrCredentials
	}
	return p, token.Facts().ExpiresAt, nil
}
