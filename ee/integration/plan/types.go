// Package plan builds deterministic, ownership-aware publication deltas.
// It performs no network, store, credential or DNS serving work.
package plan

import (
	"errors"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaxSnapshotBytes bounds the entire executable snapshot without truncation.
const MaxSnapshotBytes = 32 << 20

// Input validation and completeness errors never produce a partial plan.
var (
	ErrInvalid    = errors.New("integration plan: invalid input")
	ErrCapacity   = errors.New("integration plan: capacity exceeded")
	ErrIncomplete = errors.New("integration plan: incomplete snapshot")
)

// Policy defines deletion and update permissions for a destination.
type Policy string

// Publication policies constrain mutations of proven owned records.
const (
	PolicySync       Policy = "sync"
	PolicyUpsertOnly Policy = "upsert-only"
	PolicyCreateOnly Policy = "create-only"
)

// RemoteOwner preserves authoritative provider ownership markers.
type RemoteOwner struct{ ClusterID, IntegrationUID, ExternalDNSOwner string }

// RemoteRecord preserves each provider row's IDs and observed version even
// when several rows represent one RRset. Unknown ownership means unowned.
type RemoteRecord struct {
	Endpoint public.Endpoint
	IDs      []string
	Owner    RemoteOwner
	Version  string
}

// RecordPolicy explicitly overrides the policy for a canonical RRset.
// Hosts may cover removed sources using current zone rules; otherwise ledger
// policy retains their last applied restriction. Never split full destinations
// into independent sync planners, which could delete each other's RRsets.
type RecordPolicy struct {
	Key    kinds.RecordKey
	Policy Policy
}

// Input binds complete desired, remote, and durable ownership observations.
//
// Cleanup selects the terminating plan of an Integration whose deletionPolicy
// is Delete (D367). It requires an empty Desired set and no RecordPolicies,
// and plans only deletes: every RRset whose rows all carry this owner's
// marker (or its ledger-proven legacy marker), whose provider IDs are all in
// the confirmed ledger baseline of that RRset, and whose name has no
// conflict. The effective policy is sync for every such RRset, overriding
// the destination's and the ledger's retained upsert-only or create-only
// policy, because the sink itself is being removed. Nothing is deleted on a
// marker alone, and every row of this owner the plan leaves in place is
// listed in Plan.Retained.
type Input struct {
	Owner                                        kinds.Owner
	Destination                                  kinds.Destination
	ConfigDigest, CredentialDigest, SourceDigest string
	Policy                                       Policy
	RecordPolicies                               []RecordPolicy
	AdoptUnowned                                 bool
	AdoptExternalDNSOwner                        string
	Desired                                      []public.Endpoint
	Current                                      []RemoteRecord
	Ledger                                       []*kinds.IntegrationLedger
	// Claimed names RRset keys another owner claims at the destination
	// without a row the snapshot lists, such as a foreign ownership registry
	// row whose data row is gone (D380). Each is a conflict at its name, as a
	// foreign row would be, so nothing there is created over the claim.
	Claimed                          []kinds.RecordKey
	DesiredComplete, CurrentComplete bool
	MaxRecords                       int
	Cleanup                          bool
}

// Operation is one deterministic whole-RRset provider action.
type Operation struct {
	ID             string           `json:"id"`
	Key            kinds.RecordKey  `json:"key"`
	Action         string           `json:"action"`
	Policy         Policy           `json:"policy"`
	Before         *public.Endpoint `json:"before,omitempty"`
	After          *public.Endpoint `json:"after,omitempty"`
	ProviderIDs    []string         `json:"providerIDs,omitempty"`
	RemoteVersions []string         `json:"remoteVersions,omitempty"`
}

// Conflict preserves an owner without unsafe mutation.
type Conflict struct{ Name, Type, Reason string }

// Reasons a cleanup plan leaves an RRset of its owner in place.
const (
	// RetainedConflict: the name holds a conflict (a foreign or unowned row),
	// so nothing at that name is touched, exactly as in ordinary planning.
	RetainedConflict = "conflict at name"
	// RetainedMixed: the RRset holds rows of this owner next to rows that are
	// not proven its own; a whole-RRset delete would remove the others.
	RetainedMixed = "record set shared with rows of another owner"
	// RetainedUnproven: the rows carry this owner's marker, but the confirmed
	// ledger baseline does not list every provider ID. A marker can be copied
	// with a record; it alone never authorizes a delete.
	RetainedUnproven = "owner marker without ledger evidence"
)

// Retained is one RRset holding rows of the plan's owner that a cleanup plan
// does not delete, and why.
type Retained struct {
	Key    kinds.RecordKey
	Reason string
}

// Plan is a deterministic preview tied to exact observations.
//
// Cleanup and Retained are set only by a cleanup plan (Input.Cleanup). A
// destination is clean of its owner's records exactly when a cleanup plan
// built from a fresh complete snapshot has neither Operations nor Retained.
// Both are omitted from the digest encoding of an ordinary plan, whose
// digests and operation IDs are therefore unchanged.
type Plan struct {
	Digest                                       string
	Owner                                        kinds.Owner
	Destination                                  kinds.Destination
	ConfigDigest, CredentialDigest, SourceDigest string
	Operations                                   []Operation
	Conflicts                                    []Conflict
	Unchanged                                    int
	Cleanup                                      bool       `json:",omitempty"`
	Retained                                     []Retained `json:",omitempty"`
}

// Intent materializes a private copy suitable for durable create-only storage.
// The repository validates the supplied guard and reserves ledger state before
// any provider call; obtaining an Intent alone grants no mutation permission.
func (p *Plan) Intent(index int, at time.Time) *kinds.IntegrationOperation {
	if p == nil || index < 0 || index >= len(p.Operations) {
		return nil
	}
	o := p.Operations[index]
	s := kinds.IntegrationOperationSpec{FormatVersion: kinds.FormatVersion, Record: kinds.RecordIdentity{Owner: p.Owner, Destination: p.Destination, Key: o.Key}, OperationID: o.ID, PlanDigest: p.Digest, ConfigDigest: p.ConfigDigest, CredentialDigest: p.CredentialDigest, Action: o.Action, Policy: string(o.Policy), Before: o.Before.DeepCopy(), After: o.After.DeepCopy(), BeforeHash: kinds.EndpointDigest(o.Before), AfterHash: kinds.EndpointDigest(o.After), ProviderIDs: append([]string(nil), o.ProviderIDs...), RemoteVersions: append([]string(nil), o.RemoteVersions...), CreatedAt: at.UTC()}
	return &kinds.IntegrationOperation{ObjectMeta: metav1.ObjectMeta{Name: kinds.OperationName(o.ID)}, Spec: s}
}
