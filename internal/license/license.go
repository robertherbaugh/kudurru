// Package license verifies kudurru license keys and answers what a node may
// do with them (docs/architecture/licensing.md).
//
// A key is a JWS (compact serialization, EdDSA) signed by the kudurru
// licensor and verified offline against the public keys embedded in the
// binary, so no node ever needs to reach a license server. The package makes
// one promise to the rest of the code: licensing never touches the DNS data
// path. An absent, expired or invalid key only narrows the management plane
// (the node limit, the features under ee/); queries keep being answered.
package license

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Issuer is the iss claim of every key.
const Issuer = "kudurru"

// Grace is how long an expired key keeps working, so a renewal that is a few
// days late never changes the behavior of a running node.
const Grace = 30 * 24 * time.Hour

// CommunityNodes is the node limit without a key: one node.
const CommunityNodes = 1

// CommunityPolicies is how many Policy objects can exist without a key: one,
// the Policy "default" the bootstrap seeds (internal/bootstrap). The API
// creates no other Policy, ever, without a key (CommunityPolicyName); a key
// lifts the limit.
const CommunityPolicies = 1

// CommunityPolicyName is the name of the one Policy the API creates without a
// key: the default policy the bootstrap seeds (internal/bootstrap
// DefaultPolicyName; a test there keeps the names equal), in the server's
// default namespace, which is "default" in Docker and the release namespace in
// Kubernetes, so the namespace is passed to CheckPolicyCreate and not fixed
// here. Deleting it does not open the way to another.
const CommunityPolicyName = "default"

// Plan is the kind of license a key grants.
type Plan string

// The plans. Personal is the free key for eligible community use under the
// Kudurru Community License; the others come with a commercial agreement.
const (
	PlanPersonal   Plan = "personal"
	PlanBusiness   Plan = "business"
	PlanEnterprise Plan = "enterprise"
	PlanTrial      Plan = "trial"
)

// Valid reports whether p is a known plan.
func (p Plan) Valid() bool {
	switch p {
	case PlanPersonal, PlanBusiness, PlanEnterprise, PlanTrial:
		return true
	}
	return false
}

// Feature names a capability that needs a key. The code behind each lives
// under ee/.
type Feature string

// The licensed features.
const (
	// FeatureOIDC is single sign-on with OpenID Connect (ee/oidc).
	FeatureOIDC Feature = "oidc"
	// FeatureEdge is the dnsdist and keepalived high-availability edge and
	// the VIP supervisors (ee/edge).
	FeatureEdge Feature = "edge"
	// FeatureIntegrations is the integration framework and its providers,
	// today the external-dns webhook (ee/integration).
	FeatureIntegrations Feature = "integrations"
	// FeatureRBAC is role-based access control beyond the one built-in
	// administrator: role bindings, scoped roles and Kubernetes RBAC
	// delegation (ee/rbac).
	FeatureRBAC Feature = "rbac"
	// FeatureAudit is the persisted audit trail with its API and export
	// (ee/auditstore). The audit log lines are always written.
	FeatureAudit Feature = "audit"
	// FeatureLongTerm is long-term statistics: any statistics window that
	// reaches back further than 24 hours (ee/longterm). Rollups are always
	// collected.
	FeatureLongTerm Feature = "longterm"
	// FeatureRequests is the unblock request workflow: the "Request access"
	// form of the block page and the review of the requests. The block page
	// itself is free.
	FeatureRequests Feature = "requests"
	// FeatureBranding permits custom product branding under an Enterprise key.
	FeatureBranding Feature = "branding"
)

// Features lists every licensed feature.
func Features() []Feature {
	return []Feature{FeatureOIDC, FeatureEdge, FeatureIntegrations, FeatureRBAC, FeatureAudit, FeatureLongTerm, FeatureRequests, FeatureBranding}
}

func (f Feature) known() bool { return slices.Contains(Features(), f) }

// Claims is the signed payload of a key.
type Claims struct {
	// ID uniquely names the license (jti).
	ID string `json:"jti"`
	// Issuer is "kudurru" (iss).
	Issuer string `json:"iss"`
	// Licensee is who the license was issued to (sub): a name or a company.
	Licensee string `json:"sub"`
	// Plan is the kind of license.
	Plan Plan `json:"plan"`
	// Nodes is the node limit.
	Nodes int `json:"nodes"`
	// Features are the licensed features; empty grants all available to Plan.
	Features []Feature `json:"features,omitempty"`
	// IssuedAt, NotBefore and ExpiresAt are Unix seconds (iat, nbf, exp).
	IssuedAt  int64 `json:"iat"`
	NotBefore int64 `json:"nbf,omitempty"`
	ExpiresAt int64 `json:"exp"`
}

// Validate checks the claims' shape (not the time window).
func (c Claims) Validate() error {
	switch {
	case c.ID == "":
		return errors.New("jti is empty")
	case c.Issuer != Issuer:
		return fmt.Errorf("iss is %q, want %q", c.Issuer, Issuer)
	case strings.TrimSpace(c.Licensee) == "":
		return errors.New("sub (licensee) is empty")
	case !c.Plan.Valid():
		return fmt.Errorf("plan %q is not one of personal, business, enterprise, trial", c.Plan)
	case c.Nodes < 1:
		return fmt.Errorf("nodes is %d, want at least 1", c.Nodes)
	case c.ExpiresAt <= c.IssuedAt:
		return errors.New("exp is not after iat")
	}
	return nil
}

// Allows reports whether the claims grant f. Unknown feature names in a key
// are ignored (a newer licensor may issue them), but an empty list grants
// every feature available to the plan. Branding always requires Enterprise.
func (c Claims) Allows(f Feature) bool {
	if f == FeatureBranding && c.Plan != PlanEnterprise {
		return false
	}
	if len(c.Features) == 0 {
		return f.known()
	}
	return slices.Contains(c.Features, f)
}

// Validity returns the original capture's clock lower bound and exclusive
// absolute deadline. It performs no live lookup or replacement check. A zero
// deadline is not a finite entitlement; callers must also inspect Status.
func (c Cut) Validity() (from, until time.Time) { return c.at, c.until }

// Expires returns the expiry time.
func (c Claims) Expires() time.Time { return time.Unix(c.ExpiresAt, 0).UTC() }

// Errors of Verify.
var (
	ErrMalformed    = errors.New("license key is not a valid signed token")
	ErrUnknownKey   = errors.New("license key is signed with a key this build does not trust")
	ErrBadSignature = errors.New("license key signature does not verify")
)

// Keyring maps a key id (the JWS kid) to a trusted licensor public key.
type Keyring map[string]ed25519.PublicKey

// Verify parses raw, checks its signature against ring and returns the
// claims. It checks the shape of the claims but not the time window; Manager
// does that against its clock.
func Verify(raw string, ring Keyring) (Claims, error) {
	raw = clean(raw)
	sig, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(sig.Signatures) != 1 {
		return Claims{}, ErrMalformed
	}
	pub, ok := ring[sig.Signatures[0].Header.KeyID]
	if !ok {
		return Claims{}, ErrUnknownKey
	}
	payload, err := sig.Verify(pub)
	if err != nil {
		return Claims{}, ErrBadSignature
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := c.Validate(); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return c, nil
}

// Sign issues a key for c, signed by priv under key id kid. It is for the
// licensor's tooling (hack/licensegen) and for tests; the private key never
// ships.
func Sign(priv ed25519.PrivateKey, kid string, c Claims) (string, error) {
	if c.Issuer == "" {
		c.Issuer = Issuer
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

// clean drops the whitespace a key picks up in an email or a mounted file.
func clean(raw string) string {
	return strings.Join(strings.Fields(raw), "")
}
