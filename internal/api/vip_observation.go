package api

import "time"

// VIPObservationSnapshot is read-only provider-reported ownership evidence.
// It does not establish physical ARP reachability or cluster writer authority.
type VIPObservationSnapshot struct {
	Provider string                `json:"provider"`
	VIPs     []VIPOwnerObservation `json:"vips" nullable:"false"`
}

// VIPOwnerObservation keeps acquisition freshness distinct from provider age.
type VIPOwnerObservation struct {
	Name       string                `json:"name"`
	Address    string                `json:"address"`
	Owner      string                `json:"owner,omitempty"`
	Known      bool                  `json:"known"`
	Reason     string                `json:"reason,omitempty"`
	CheckedAt  time.Time             `json:"checkedAt"`
	ExpiresAt  time.Time             `json:"expiresAt"`
	ServiceUID string                `json:"serviceUID,omitempty"`
	Evidence   *VIPAnnouncerEvidence `json:"evidence,omitempty"`
}

// VIPAnnouncerEvidence identifies the observed provider resource. RenewedAt is
// a provider heartbeat only for kube-vip's per-Service Lease. MetalLB L2 status
// has no heartbeat; LivenessAt separately reports its speaker Node Lease.
type VIPAnnouncerEvidence struct {
	Kind            string     `json:"kind"`
	UID             string     `json:"uid"`
	ResourceVersion string     `json:"resourceVersion"`
	CreatedAt       time.Time  `json:"createdAt"`
	RenewedAt       *time.Time `json:"renewedAt,omitempty"`
	LivenessAt      *time.Time `json:"livenessAt,omitempty"`
}
