// Package replication defines the complete logical image protocol. It is
// deliberately independent of the backup/apply format and of any transport.
package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Protocol identifiers and the canonical settings owner.
const (
	Format        = "kudurru.replication"
	FormatVersion = 1
	SettingsOwner = "kudurru.io/Kudurru/default"
)

// Protocol validation and authority transition errors.
var (
	ErrInvalid           = errors.New("replication: invalid image")
	ErrTooLarge          = errors.New("replication: image exceeds bounds")
	ErrIncompatible      = errors.New("replication: incompatible image")
	ErrWrongCluster      = errors.New("replication: wrong cluster")
	ErrStale             = errors.New("replication: stale image")
	ErrDivergent         = errors.New("replication: divergent image")
	ErrPromotionConflict = errors.New("replication: conflicting promotions")
	ErrCAS               = errors.New("replication: authority changed")
	ErrReadOnly          = errors.New("replication: read only")
	ErrFenced            = errors.New("replication: writer fenced")
)

// Term identifies authority, not an address, wall clock or resourceVersion.
type Term struct {
	ClusterID   string `json:"clusterID"`
	Epoch       uint64 `json:"epoch,string"`
	PrimaryID   string `json:"primaryID"`
	PromotionID string `json:"promotionID"`
	PrimaryName string `json:"primaryName,omitempty"`
}

// Same compares authority identity, excluding the display name.
func (t Term) Same(other Term) bool {
	return t.ClusterID == other.ClusterID && t.Epoch == other.Epoch && t.PrimaryID == other.PrimaryID && t.PromotionID == other.PromotionID
}

// Validate checks the required authority identifiers and their bounds.
func (t Term) Validate() error {
	if t.ClusterID == "" || t.PrimaryID == "" || t.PromotionID == "" || t.Epoch == 0 {
		return fmt.Errorf("%w: incomplete term", ErrInvalid)
	}
	for _, id := range []string{t.ClusterID, t.PrimaryID, t.PromotionID, t.PrimaryName} {
		if len(id) > 256 {
			return fmt.Errorf("%w: term identifier", ErrTooLarge)
		}
	}
	return nil
}

// Generation identifies a complete image within an authority term.
type Generation struct {
	Term     Term   `json:"term"`
	Revision uint64 `json:"revision,string"`
	Digest   string `json:"digest"`
}

// Same compares the term, revision and payload digest.
func (g Generation) Same(other Generation) bool {
	return g.Term.Same(other.Term) && g.Revision == other.Revision && g.Digest == other.Digest
}

// ETag returns a stable HTTP validator for this generation's identity.
func (g Generation) ETag() string {
	// Display names are not authority identity (see Term.Same).
	g.Term.PrimaryName = ""
	b, _ := json.Marshal(g)
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// Validate checks the authority term and canonical payload digest.
func (g Generation) Validate() error {
	if err := g.Term.Validate(); err != nil {
		return err
	}
	b, err := hex.DecodeString(g.Digest)
	if err != nil || len(b) != sha256.Size || g.Digest != strings.ToLower(g.Digest) {
		return fmt.Errorf("%w: generation digest", ErrInvalid)
	}
	return nil
}

// Reader consumers subscribe before reading, then re-read a complete image
// when the coalesced notification closes. They never assemble kind streams.
type Reader interface {
	ReadGeneration(context.Context) (*Image, error)
	GenerationChanges() <-chan struct{}
}

// Mode describes a member's durable replication role.
type Mode string

// Replication roles and authority refusal states.
const (
	Standalone Mode = "standalone"
	Primary    Mode = "primary"
	Follower   Mode = "follower"
	Unenrolled Mode = "unenrolled"
	Fenced     Mode = "fenced"
	Conflict   Mode = "conflict"
)

// Authority is local durable metadata. Only Installed belongs to the payload's
// source generation; the member ID and fencing observations are never copied.
type Authority struct {
	MemberID              string      `json:"memberID"`
	Term                  Term        `json:"term"`
	Mode                  Mode        `json:"mode"`
	Incarnation           uint64      `json:"incarnation,string"`
	MaxObservedEpoch      uint64      `json:"maxObservedEpoch,string"`
	Installed             Generation  `json:"installed"`
	Parent                *Generation `json:"parent,omitempty"`
	Conflict              *Term       `json:"conflict,omitempty"`
	Divergence            *Generation `json:"divergence,omitempty"`
	WatchLineage          string      `json:"watchLineage"`
	InstallSequence       uint64      `json:"installSequence,string"`
	LastPromotionRequest  string      `json:"lastPromotionRequest,omitempty"`
	LastResolutionRequest string      `json:"lastResolutionRequest,omitempty"`
}

// Writable reports whether this member owns an uncontested writer term.
func (a Authority) Writable() bool {
	return (a.Mode == Standalone || a.Mode == Primary) && a.Term.PrimaryID == a.MemberID && a.Conflict == nil && a.Divergence == nil
}

// Kind declares an object kind and row count in the complete-image manifest.
type Kind struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Count      int    `json:"count"`
}

// Entry is an extension row. Each registered section owns its rows in the SAME
// transaction as objects. Sensitive data must already be sealed by its owner.
type Entry struct {
	Key  string          `json:"key"`
	Data json.RawMessage `json:"data"`
}

// Settings is a reference to the future singleton, never a second value map.
type Settings struct {
	Owner string `json:"owner"`
}

// Payload contains all replicated objects and owner-specific sections.
type Payload struct {
	Objects         []json.RawMessage `json:"objects"`
	ClusterSettings Settings          `json:"clusterSettings"`
	Runtime         []Entry           `json:"runtime"`
	Security        []Entry           `json:"security"`
	Membership      []Entry           `json:"membership"`
	Dedup           []Entry           `json:"dedup"`
}

// Image is a complete logical snapshot with its authority and content digest.
type Image struct {
	Format                string      `json:"format"`
	FormatVersion         int         `json:"formatVersion"`
	StoreSchemaVersion    int         `json:"storeSchemaVersion"`
	RequiredFeatures      []string    `json:"requiredFeatures"`
	Term                  Term        `json:"term"`
	Revision              uint64      `json:"revision,string"`
	ObjectResourceVersion uint64      `json:"objectResourceVersion,string"`
	Parent                *Generation `json:"parent"`
	CreatedAt             time.Time   `json:"createdAt"`
	Complete              bool        `json:"complete"`
	KindManifest          []Kind      `json:"kindManifest"`
	PayloadSHA256         string      `json:"payloadSHA256"`
	Payload               Payload     `json:"payload"`
}

// Generation returns the image's authority, revision and payload digest.
func (i Image) Generation() Generation {
	return Generation{Term: i.Term, Revision: i.Revision, Digest: i.PayloadSHA256}
}

// Validated cannot be populated by callers; Image returns a defensive copy.
type Validated struct{ encoded []byte }

// Image returns a defensive copy of the accepted complete image.
func (v *Validated) Image() Image {
	var i Image
	if v != nil {
		_ = json.Unmarshal(v.encoded, &i)
	}
	return i
}

// Limits bounds encoded images, rows and nesting during validation.
type Limits struct {
	MaxBytes                                         int64
	MaxObjects, MaxObjectBytes, MaxEntries, MaxDepth int
}

// Defaults fills unset limits with the protocol's supported bounds.
func (l Limits) Defaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 256 << 20
	}
	if l.MaxObjects <= 0 {
		l.MaxObjects = 100_000
	}
	if l.MaxObjectBytes <= 0 {
		l.MaxObjectBytes = 1536 << 10
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = 100_000
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = 64
	}
	return l
}

// Validation supplies schema compatibility and owner-specific row checks.
type Validation struct {
	SchemaVersion  int
	Kinds          []Kind
	Limits         Limits
	ValidateObject func(json.RawMessage) error
	ValidateEntry  func(section string, entry Entry) error
	// ValidateImage checks cross-row owner inventories after generic validation
	// and digest verification. The image is detached from the accepted encoding;
	// mutations cannot change the returned Validated value. No I/O is permitted.
	ValidateImage func(*Image) error
}

// InstallOptions fences installation against the exact current local base.
type InstallOptions struct {
	ExpectedTerm      Term
	ExpectedInstalled Generation
}

// PromoteRequest names the local base and a unique promotion intent.
type PromoteRequest struct {
	ExpectedTerm      Term
	ExpectedInstalled Generation
	RequestID         string
}

// ResolveRequest names the exact local base and observed conflict. The chosen
// validated image must be one of those branches. Operator authorization and
// confirmation belong above this atomic store transition.
type ResolveRequest struct {
	ExpectedTerm       Term
	ExpectedInstalled  Generation
	ExpectedConflict   *Term
	ExpectedDivergence *Generation
	RequestID          string
}

// SectionNames returns the fixed ownership registry. New sections require a
// format/feature negotiation; adding a kind needs only scheme registration.
func SectionNames() []string { return []string{"runtime", "security", "membership", "dedup"} }

// Section returns a registered payload section, or nil for an unknown name.
func Section(p *Payload, name string) *[]Entry {
	switch name {
	case "runtime":
		return &p.Runtime
	case "security":
		return &p.Security
	case "membership":
		return &p.Membership
	case "dedup":
		return &p.Dedup
	default:
		return nil
	}
}
