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
	DesiredComplete, CurrentComplete             bool
	MaxRecords                                   int
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

// Plan is a deterministic preview tied to exact observations.
type Plan struct {
	Digest                                       string
	Owner                                        kinds.Owner
	Destination                                  kinds.Destination
	ConfigDigest, CredentialDigest, SourceDigest string
	Operations                                   []Operation
	Conflicts                                    []Conflict
	Unchanged                                    int
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
