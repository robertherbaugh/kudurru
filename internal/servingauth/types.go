package servingauth

import (
	"context"
	"errors"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/runtime"
)

// Errors are safe operator diagnostics; no raw verifier, cookie or object data.
var (
	ErrInvalid     = errors.New("servingauth: invalid owner state")
	ErrVersion     = errors.New("servingauth: unsupported owner version")
	ErrBounds      = errors.New("servingauth: owner bounds exceeded")
	ErrManifest    = errors.New("servingauth: complete auth manifest required")
	ErrDependency  = errors.New("servingauth: dependency identity mismatch")
	ErrCredentials = errors.New("servingauth: credential rejected")
	ErrDenied      = errors.New("servingauth: permission denied")
	ErrUnsupported = errors.New("servingauth: credential capability unavailable")
)

// Schema supplies the complete LOCAL object registry, not zonecache.DefaultKinds.
// RequiredKinds includes all six IAM kinds and the singleton settings kind.
type Schema struct {
	Scheme        *runtime.Scheme
	Kinds         []replication.Kind
	SchemaVersion int
}

// LicensePolicy is an explicit immutable management entitlement decision.
// It has no effect on DNS readiness or DNS components.
type LicensePolicy struct{ RBAC, Providers bool }

// Dependencies contains host-prepared policy and pure cryptographic resources.
// Identity is host-issued; ExpectedIdentity is the publisher's captured cursor.
// Provider handles and keys must remain leased to the bundle until Release.
type Dependencies struct {
	Identity, ExpectedIdentity string
	License                    LicensePolicy
	Permissions                *rbac.Permissions
	StaticBindings             []rbac.Binding
	CurrentKey, PreviousKey    []byte
	Hasher                     PasswordVerifier
	TOTPKeys                   *totp.Keyring
	ConfigProviders            []ProviderSource
	ProviderResources          []ProviderResource
}

// PasswordVerifier is a bounded pure crypto seam; users.Hasher implements it.
type PasswordVerifier interface {
	Verify(context.Context, string, string) (bool, error)
}

// Credential is an untrusted proof, never a forwarded Principal. Password is
// for local proof verification only; issuance/replay consumption is a command.
type Credential struct {
	Kind            CredentialKind
	Value, Username string
	TOTPCode        string
	ProviderID      string
}

// CredentialKind selects a supported proof format.
type CredentialKind string

// Supported raw credential formats.
const (
	APIToken CredentialKind = "api-token"
	Cookie   CredentialKind = "cookie"
	Password CredentialKind = "password"
	OIDCJWT  CredentialKind = "oidc-jwt"
)

// SessionDecision is a detached current login result. BuiltinAdmin stays on
// Principal rather than becoming a durable binding grant. It is not proof for
// another transaction; root must enroll within the transaction that resolved it.
type SessionDecision struct {
	Principal auth.Principal
	Grants    []string
	Settings  *publicv1.Kudurru
}

// AccountRef binds a durable session/floor to an exact object incarnation.
type AccountRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// RootEntry is the required explicit owner manifest. Every set must be non-nil.
type RootEntry struct {
	FormatVersion      int      `json:"formatVersion"`
	Epoch              uint64   `json:"epoch,string"`
	GrantFloors        []string `json:"grantFloors"`
	AccountFloors      []string `json:"accountFloors"`
	ProviderFloors     []string `json:"providerFloors"`
	Sessions           []string `json:"sessions"`
	TOTP               []string `json:"totp"`
	ExternalIdentities []string `json:"externalIdentities"`
	ExternalSessions   []string `json:"externalSessions"`
}

// GrantFloor persists current behavior: issuedAt <= ValidAfter fails.
type GrantFloor struct {
	FormatVersion int       `json:"formatVersion"`
	Grant         string    `json:"grant"`
	ValidAfter    time.Time `json:"validAfter"`
}

// AccountFloor revokes sessions for one incarnation, never a reused name.
type AccountFloor struct {
	FormatVersion int        `json:"formatVersion"`
	Account       AccountRef `json:"account"`
	ValidAfter    time.Time  `json:"validAfter"`
}

// ProviderFloor binds provider revocation to its actual public IdP UID.
type ProviderFloor struct {
	FormatVersion int       `json:"formatVersion"`
	Provider      string    `json:"provider"`
	UID           string    `json:"uid"`
	ValidAfter    time.Time `json:"validAfter"`
}

// SessionRecord is the authoritative enrollment. Legacy cookies have no row and
// are rejected. Epoch is issuance provenance, not a global logout on any change.
type SessionRecord struct {
	FormatVersion int        `json:"formatVersion"`
	ID            string     `json:"id"`
	Account       AccountRef `json:"account"`
	Provider      string     `json:"provider"`
	ProviderUID   string     `json:"providerUID"`
	Epoch         uint64     `json:"epoch,string"`
	IssuedAt      time.Time  `json:"issuedAt"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	IdleExpiresAt time.Time  `json:"idleExpiresAt"`
	Grants        []string   `json:"grants"`
	CSRFHash      string     `json:"csrfHash"`
	Revoked       bool       `json:"revoked"`
}

// Security is a detached writable command input; it is never a View's live map.
type Security struct {
	Epoch              uint64
	GrantFloors        []GrantFloor
	AccountFloors      []AccountFloor
	ProviderFloors     []ProviderFloor
	Sessions           []SessionRecord
	TOTP               []totp.Record
	ExternalIdentities []ExternalIdentity
	ExternalSessions   []ExternalSessionRecord
}

// CurrentReader is implemented by sqlite.CommandTx. Root must pass only its
// admitted transaction reader, never a live Store or captured View principal.
type CurrentReader interface {
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
	CheckSectionBounds(context.Context, string, int, int) error
	ListSections(context.Context, string) ([]replication.Entry, error)
}

// requiredKinds is deliberately independent of DNS and public safe exports.
var requiredKinds = []string{"APIToken", "IdentityProvider", "Kudurru", "LocalUser", "PrincipalState", "RoleBinding", "ServiceAccount"}

// DetachedPrincipal documents the return boundary used by View helpers.
func detachedPrincipal(p auth.Principal) auth.Principal {
	if p.External != nil {
		e := *p.External
		p.External = &e
	}
	p.Groups = append([]string(nil), p.Groups...)
	p.Grants = append([]string(nil), p.Grants...)
	p.Scopes = append([]string(nil), p.Scopes...)
	p.Implicit = append([]string(nil), p.Implicit...)
	return p
}

// ProviderOrigin identifies the immutable authority for a provider incarnation.
type ProviderOrigin string

const (
	// ProviderObject derives from the actual captured object UID.
	ProviderObject ProviderOrigin = "object"
	// ProviderConfig derives from a durable host source incarnation.
	ProviderConfig ProviderOrigin = "config"
)

// ProviderRef never identifies an authority by display name alone.
type ProviderRef struct {
	ID     string         `json:"id"`
	UID    string         `json:"uid"`
	Origin ProviderOrigin `json:"origin"`
}

// ProviderSource is host config input; object inputs are derived only from Cut.
type ProviderSource struct {
	Ref       ProviderRef
	CreatedAt time.Time
	Spec      publicv1.IdentityProviderSpec
}

// ProviderDescriptor is detached compiled source policy and exact provenance.
type ProviderDescriptor struct {
	Ref                                  ProviderRef
	CreatedAt                            time.Time
	Issuer, SpecIdentity, PolicyIdentity string
	Spec                                 publicv1.IdentityProviderSpec
}

// ProviderAvailability makes a denying unavailable capability explicit.
type ProviderAvailability string

const (
	// ProviderReady carries an exact immutable offline verifier.
	ProviderReady ProviderAvailability = "ready"
	// ProviderUnavailable owns its ID without falling back to another source.
	ProviderUnavailable ProviderAvailability = "unavailable"
)

// ProviderResource pins a detached offline verifier or explicit unavailability.
type ProviderResource struct {
	Ref                                                ProviderRef
	SpecIdentity, PolicyIdentity                       string
	Availability                                       ProviderAvailability
	Verifier                                           *oidccore.Verifier
	VerifierIdentity, TrustIdentity, UnavailableReason string
}

// ExternalIdentity is a durable account independent of LocalUser and IdP UIDs.
type ExternalIdentity struct {
	FormatVersion    int         `json:"formatVersion"`
	Provider         ProviderRef `json:"provider"`
	Issuer           string      `json:"issuer"`
	Subject          string      `json:"subject"`
	UID              string      `json:"uid"`
	CreatedAt        time.Time   `json:"createdAt"`
	Disabled         bool        `json:"disabled"`
	Username         string      `json:"username"`
	DisplayName      string      `json:"displayName"`
	Email            string      `json:"email"`
	Groups           []string    `json:"groups"`
	GroupsVerifiedAt *time.Time  `json:"groupsVerifiedAt"`
}

// ExternalSessionRecord has immutable, purpose-bound verified login evidence.
type ExternalSessionRecord struct {
	FormatVersion int         `json:"formatVersion"`
	ID            string      `json:"id"`
	Account       AccountRef  `json:"account"`
	Provider      ProviderRef `json:"provider"`
	Issuer        string      `json:"issuer"`
	Subject       string      `json:"subject"`
	Epoch         uint64      `json:"epoch,string"`
	IssuedAt      time.Time   `json:"issuedAt"`
	ExpiresAt     time.Time   `json:"expiresAt"`
	IdleExpiresAt time.Time   `json:"idleExpiresAt"`
	Grants        []string    `json:"grants"`
	CSRFHash      string      `json:"csrfHash"`
	Revoked       bool        `json:"revoked"`
	Evidence      string      `json:"evidence"`
}
