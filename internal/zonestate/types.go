// Package zonestate stores a complete committed primary generation through
// one CAS head and immutable bounded chunks. Source objects remain desired
// inputs; serving/materialization consumes only verified committed roots.
package zonestate

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/miekg/dns"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

var (
	// ErrInvalid rejects corrupt or incompatible committed data.
	ErrInvalid = errors.New("zonestate: invalid aggregate")
	// ErrCapacity rejects inputs that exceed finite bounds.
	ErrCapacity = errors.New("zonestate: capacity exceeded")
	// ErrConflict requires fresh reads and planning after a lost CAS.
	ErrConflict = errors.New("zonestate: head conflict; re-read and re-plan")
)

// Limits bound expanded data, metadata and immutable preparations together.
type Limits struct {
	MaxRecords        int
	MaxWireBytes      int64
	MaxAggregateBytes int64
	MaxChunks         int
	MaxReceipts       int
}

// Defaults replaces nonpositive limits with finite defaults.
func (l Limits) Defaults() Limits {
	if l.MaxRecords <= 0 {
		l.MaxRecords = 1_000_000
	}
	if l.MaxWireBytes <= 0 {
		l.MaxWireBytes = 64 << 20
	}
	if l.MaxAggregateBytes <= 0 {
		l.MaxAggregateBytes = 128 << 20
	}
	if l.MaxChunks <= 0 {
		l.MaxChunks = 1024
	}
	if l.MaxReceipts <= 0 {
		l.MaxReceipts = 256
	}
	return l
}

// Source freezes actual desired identity/revision/ownership, including
// inactive/view-specific data. Desired stores are never joined at serve time.
type Source struct {
	ID              string          `json:"id"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Manager         string          `json:"manager"`
	Desired         json.RawMessage `json:"desired"`
}

// Provenance binds an RR to one declarative source or verified credential.
type Provenance struct {
	SourceID          string `json:"sourceID,omitempty"`
	Key               string `json:"key,omitempty"`
	CredentialVersion string `json:"credentialVersion,omitempty"`
}

// Record preserves uncompressed RR wire and its ownership.
type Record struct {
	Wire       []byte     `json:"wire"`
	Provenance Provenance `json:"provenance"`
}

// Policy freezes UPDATE authorization at the committed head.
type Policy struct {
	Enabled     bool                       `json:"enabled"`
	AuthEpoch   uint64                     `json:"authEpoch"`
	Credentials []dynamicupdate.Credential `json:"credentials,omitempty"`
	Grants      []dynamicupdate.Grant      `json:"grants,omitempty"`
}

// Receipt IDs are internal one-operation retry IDs, not DNS IDs/content hashes.
// No secret or MAC proof is persisted. The current credential/policy must be
// checked even when returning a retained result.
type Receipt struct {
	ID             string                  `json:"id"`
	SemanticDigest string                  `json:"semanticDigest"`
	Principal      dynamicupdate.Principal `json:"principal"`
	AuthEpoch      uint64                  `json:"authEpoch"`
	CommitSequence uint64                  `json:"commitSequence"`
	Serial         uint32                  `json:"serial"`
	Rcode          int                     `json:"rcode"`
	Changed        bool                    `json:"changed"`
}

// Aggregate contains the entire durable primary generation.
type Aggregate struct {
	FormatVersion int              `json:"formatVersion"`
	Zone          zonekinds.ZoneID `json:"zone"`
	// Config is the complete frozen serving configuration, not a pointer to
	// the latest desired DNSZone. Root supplies its typed build adapter.
	Config       json.RawMessage             `json:"config"`
	Sources      []Source                    `json:"sources,omitempty"`
	Records      []Record                    `json:"records"`
	Reservations []dynamicupdate.Reservation `json:"reservations,omitempty"`
	Policy       Policy                      `json:"policy"`
	// Expiry is a committed serving deadline. Due expiry requires a new
	// content/serial commit; an overdue uncommitted removal fails closed.
	Expiry   time.Time `json:"expiry,omitempty"`
	Receipts []Receipt `json:"receipts,omitempty"`
}

// Expected is the exact head and authority fence a candidate was planned against.
type Expected struct {
	Zone            zonekinds.ZoneID
	ResourceVersion string // empty means CREATE ONLY, never unconditional Apply
	CommitSequence  uint64
	RootDigest      string
	AuthEpoch       uint64
	Term            replication.Term // SQLite's transactional HA fence
}

// View is a verified head and its independently owned complete data.
type View struct {
	Head      *zonekinds.ZoneState
	Aggregate *Aggregate
	Expected  Expected
}

// Prepared holds bounded immutable chunks and their candidate publication head.
type Prepared struct {
	Head   *zonekinds.ZoneState
	Chunks []*zonekinds.ZoneChunk
}

// Guard runs at the storage commit boundary, including metadata/no-op commits.
// It must be bounded, synchronous and cannot reenter independent Store methods
// inside SQLite Command. Root's HA adapter does current credential/domain
// authorization through its enclosing CommandTx, never a fake raw-wire proof.
type Guard func(context.Context, *zonekinds.ZoneState) error

// Backend has exactly one mutable publication operation. Commit MUST NOT
// retry a stale candidate. Immutable chunks can be prepared before the CAS.
type Backend interface {
	Head(context.Context, zonekinds.ZoneID) (*zonekinds.ZoneState, replication.Term, error)
	Chunk(context.Context, string) (*zonekinds.ZoneChunk, error)
	Stage(context.Context, *zonekinds.ZoneChunk) error
	Commit(context.Context, Expected, *Prepared, Guard) (*zonekinds.ZoneState, error)
}

// FromRR validates and copies RR wire without modifying the caller.
func FromRR(rr dns.RR, p Provenance) (Record, error) {
	if err := dynamicupdate.ValidateData(rr); err != nil {
		return Record{}, err
	}
	wire, err := dynamicupdate.Wire(rr)
	return Record{Wire: wire, Provenance: p}, err
}

// RRs decodes detached records from the committed wire representation.
func (a *Aggregate) RRs() ([]dns.RR, error) {
	out := make([]dns.RR, 0, len(a.Records))
	for _, r := range a.Records {
		rr, n, err := dns.UnpackRR(r.Wire, 0)
		if err != nil || n != len(r.Wire) {
			return nil, ErrInvalid
		}
		out = append(out, rr)
	}
	return out, nil
}

// UpdateState includes implicit protection for visible declarative records.
func (v *View) UpdateState() (dynamicupdate.State, error) {
	rrs, err := v.Aggregate.RRs()
	if err != nil {
		return dynamicupdate.State{}, err
	}
	a := v.Aggregate
	reservations := append([]dynamicupdate.Reservation(nil), a.Reservations...)
	for i, r := range a.Records {
		if r.Provenance.SourceID == "" {
			continue
		}
		h := rrs[i].Header()
		reservations = append(reservations, dynamicupdate.Reservation{Name: h.Name, Type: h.Rrtype, Subtree: h.Rrtype == dns.TypeDNAME, Source: r.Provenance.SourceID})
	}
	return dynamicupdate.State{Apex: a.Zone.Apex, Records: rrs, UpdateEnabled: a.Policy.Enabled, AuthEpoch: a.Policy.AuthEpoch, Credentials: a.Policy.Credentials, Grants: a.Policy.Grants, Reservations: reservations}, nil
}
