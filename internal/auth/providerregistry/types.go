// Package providerregistry owns the complete durable config-source and provider
// authority registry. It never discovers providers, grants host capabilities,
// authorizes a caller, or activates serving resources. Root admits its opaque
// plans and auth floors in the same concrete SQLite command transaction.
package providerregistry

import (
	"errors"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// FormatVersion is independent of the auth root and existing row versions.
const FormatVersion = 1

// RootKey names the required disjoint owner manifest.
const RootKey = "oidc-provider/v1/root"

const prefix = "oidc-provider/"
const versionPrefix = "oidc-provider/v1/"

// Limits bound the complete registry before typed allocation or compilation.
const (
	MaxActiveSources     = 64
	MaxSources           = 256
	MaxActiveAuthorities = 64
	MaxAuthorities       = 256
	MaxBytes             = 1 << 20
	MaxRowBytes          = 64 << 10
	MaxRootBytes         = 32 << 10
	MaxRows              = 1 + MaxSources + MaxAuthorities
)

// Safe diagnostics never contain specs, credentials, or provider responses.
var (
	ErrInvalid  = errors.New("providerregistry: invalid owner state")
	ErrManifest = errors.New("providerregistry: complete registry required")
	ErrVersion  = errors.New("providerregistry: unsupported format")
	ErrBounds   = errors.New("providerregistry: bounds exceeded")
	ErrConflict = errors.New("providerregistry: registry changed")
	ErrFloor    = errors.New("providerregistry: required provider floor missing")
)

// Root explicitly inventories both active and retained historical row families.
type Root struct {
	FormatVersion int      `json:"formatVersion"`
	ClusterID     string   `json:"clusterID"`
	Epoch         uint64   `json:"epoch,string"`
	Sources       []string `json:"sources"`
	Authorities   []string `json:"authorities"`
}

// Source retains a config incarnation and exact nonsecret schema through restart.
type Source struct {
	FormatVersion int                           `json:"formatVersion"`
	ID            string                        `json:"id"`
	UID           string                        `json:"uid"`
	CreatedAt     time.Time                     `json:"createdAt"`
	Revision      uint64                        `json:"revision,string"`
	Active        bool                          `json:"active"`
	Spec          publicv1.IdentityProviderSpec `json:"spec"`
	SpecDigest    string                        `json:"specDigest"`
	AuthorityKey  string                        `json:"authorityKey"`
}

// AuthorityBinding is a root-admitted opaque receipt, never a capability grant.
type AuthorityBinding struct {
	Profile               string `json:"profile"`
	FingerprintKeyID      string `json:"fingerprintKeyID"`
	CredentialFingerprint string `json:"credentialFingerprint"`
	CredentialPolicy      string `json:"credentialPolicy"`
	NetworkPolicy         string `json:"networkPolicy"`
	MetadataBinding       string `json:"metadataBinding"`
	EntitlementIdentity   string `json:"entitlementIdentity"`
}

// AuthorityRecord binds accepted material/trust for both actual source origins.
type AuthorityRecord struct {
	FormatVersion int                     `json:"formatVersion"`
	Provider      servingauth.ProviderRef `json:"provider"`
	SpecIdentity  string                  `json:"specIdentity"`
	Revision      uint64                  `json:"revision,string"`
	Active        bool                    `json:"active"`
	Binding       AuthorityBinding        `json:"binding"`
}

// Registry is an immutable validated owner cut. All accessors detach outputs.
type Registry struct {
	root        Root
	sources     []Source
	authorities []AuthorityRecord
	digest      string
}

// DesiredConfig is the complete root-approved config input, not a public object.
type DesiredConfig struct {
	ID   string
	Spec publicv1.IdentityProviderSpec
}

// DesiredAuthority is a root-admitted acquisition receipt for an exact source.
type DesiredAuthority struct {
	Source  servingauth.ProviderDescriptor
	Binding AuthorityBinding
}

// ProviderIncarnation identifies a required persistent revocation-floor target.
type ProviderIncarnation struct{ ID, UID string }

// Plan is opaque and cannot be forged or activated by editing returned fields.
// Stage requires current registry CAS and the plan's actual transaction floors.
type Plan struct {
	before           *Registry
	after            *Registry
	at               time.Time
	retired, changed []ProviderIncarnation
	descriptors      []servingauth.ProviderDescriptor
	initialize       bool
}

func (*Registry) String() string { return "private provider registry" }

// GoString redacts arbitrary generic diagnostics.
func (r *Registry) GoString() string { return r.String() }

// MarshalJSON prevents accidental authority/spec persistence outside owner encoding.
func (*Registry) MarshalJSON() ([]byte, error) { return []byte(`"private registry"`), nil }

func (*Plan) String() string { return "private provider registry plan" }

// GoString redacts generic plan diagnostics.
func (p *Plan) GoString() string { return p.String() }

// MarshalJSON prevents persisting private admission plans.
func (*Plan) MarshalJSON() ([]byte, error) { return []byte(`"private plan"`), nil }
