package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// BearerCacheTTL caps how long a verified bearer ID token is cached
// (§8.3: min(exp, 15m); dex issues 24 h ID tokens by default).
const BearerCacheTTL = 15 * time.Minute

const maxBearerCache = 10_000

// bearerEntry is one cached verdict.
type bearerEntry struct {
	p   auth.Principal
	exp time.Time
}

type bearerCache struct {
	mu sync.Mutex
	m  map[[32]byte]bearerEntry
}

func (c *bearerCache) get(k [32]byte, now time.Time) (auth.Principal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || !now.Before(e.exp) {
		return auth.Principal{}, false
	}
	return e.p, true
}

func (c *bearerCache) put(k [32]byte, p auth.Principal, exp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[[32]byte]bearerEntry{}
	}
	if len(c.m) >= maxBearerCache {
		clear(c.m)
	}
	c.m[k] = bearerEntry{p: p, exp: exp}
}

// acceptedAudiences are the client IDs a bearer ID token may be minted for:
// cliClientID (the v0.2 `kudurru login` public client), clientID and any
// configured extra audiences.
func (c *ProviderConfig) acceptedAudiences() []string {
	out := []string{}
	for _, a := range append([]string{c.CLIClientID, c.ClientID}, c.Audiences...) {
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// AcceptsIssuer implements auth.JWTVerifier: bearer ID tokens are routed to
// the provider whose issuer matches iss exactly.
func (p *Provider) AcceptsIssuer(_ context.Context, iss string) bool { return iss == p.cfg.Issuer }

// ErrBearerAudience is the cause of a 401 for an ID token minted for
// another relying party.
var ErrBearerAudience = errors.New("oidc: bearer token audience is not kudurru")

// VerifyJWT implements auth.JWTVerifier: the bearer ID-token path of §8.3,
// server side in v0.1 so that the CLI can use it later. The token is
// verified against the provider's JWKS (signature, iss, exp); aud must
// contain cliClientID, clientID or a configured audience, and when aud has
// several values azp must equal one of those (so an ID token issued for an
// unrelated application cannot be replayed); access tokens (typ at+jwt)
// are refused. The claim pipeline runs on every verification and the
// principal carries the prefixed groups, so live bindings authorize it.
// Verdicts are cached by sha256(token) until min(exp, 15m).
func (p *Provider) VerifyJWT(ctx context.Context, raw string) (auth.Principal, error) {
	now := p.opts.Now()
	key := sha256.Sum256([]byte(raw))
	if pr, ok := p.bearer.get(key, now); ok {
		return pr, nil
	}
	if typ, _ := jwsHeader(raw)["typ"].(string); typ == "at+jwt" || typ == "application/at+jwt" {
		return auth.Principal{}, auth.Unauthenticated(errors.New("access token presented as an ID token"))
	}
	v, err := p.bearerVerifier(ctx)
	if err != nil {
		return auth.Principal{}, auth.Unauthenticated(err)
	}
	idt, err := v.Verify(ctx, raw)
	if err != nil {
		return auth.Principal{}, auth.Unauthenticated(err)
	}
	if idt.Subject == "" {
		return auth.Principal{}, auth.Unauthenticated(errors.New("ID token without subject"))
	}
	var rawClaims json.RawMessage
	if err := idt.Claims(&rawClaims); err != nil {
		return auth.Principal{}, auth.Unauthenticated(err)
	}
	claims, err := DecodeClaims(rawClaims)
	if err != nil {
		return auth.Principal{}, auth.Unauthenticated(err)
	}
	if err := checkBearerAudience(idt.Audience, claims, p.cfg.acceptedAudiences()); err != nil {
		return auth.Principal{}, auth.Unauthenticated(err)
	}
	id, lerr := p.cfg.Evaluate(idt.Issuer, idt.Subject, claims)
	if lerr != nil {
		return auth.Principal{}, auth.Unauthenticated(lerr)
	}
	pr := id.Principal()
	pr.AuthMethod = auth.MethodJWT
	exp := now.Add(BearerCacheTTL)
	if idt.Expiry.Before(exp) {
		exp = idt.Expiry
	}
	p.bearer.put(key, pr, exp)
	return pr, nil
}

// checkBearerAudience applies the §8.3 audience rule.
func checkBearerAudience(aud []string, claims Claims, accepted []string) error {
	hit := false
	for _, a := range aud {
		if slices.Contains(accepted, a) {
			hit = true
			break
		}
	}
	if !hit {
		return ErrBearerAudience
	}
	azp, hasAzp := claims["azp"].(string)
	if len(aud) > 1 && (!hasAzp || !slices.Contains(accepted, azp)) {
		return fmt.Errorf("%w: several audiences and azp %q is not a kudurru client", ErrBearerAudience, azp)
	}
	if hasAzp && !slices.Contains(accepted, azp) {
		return fmt.Errorf("%w: issued to client %q", ErrBearerAudience, azp)
	}
	return nil
}

// bearerVerifier is a JWKS-backed verifier that leaves the audience to
// checkBearerAudience (go-oidc checks a single client ID only).
func (p *Provider) bearerVerifier(ctx context.Context) (*gooidc.IDTokenVerifier, error) {
	md, err := p.metadata(ctx)
	if err != nil {
		return nil, err
	}
	return md.bearer, nil
}

// jwsHeader decodes the protected header of a compact JWS (routing only).
func jwsHeader(raw string) map[string]any {
	i := 0
	for i < len(raw) && raw[i] != '.' {
		i++
	}
	b, err := decodeSegment(raw[:i])
	if err != nil {
		return nil
	}
	var h map[string]any
	_ = json.Unmarshal(b, &h)
	return h
}
