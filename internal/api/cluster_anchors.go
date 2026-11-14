package api

import (
	"net/http"
	"time"
)

// RootAnchorStatus is a read-only view of this node's managed root trust
// anchors (RFC 5011) and the workers that maintain them (D369). It is
// diagnostic output: no field grants publication, promotion or an anchor
// operation, and nothing here is a recommendation to act.
type RootAnchorStatus struct {
	// Node is this node's name.
	Node string `json:"node"`
	// AnchorMode is the host's upstreams.recursion.anchorMode ("static" or
	// "managed"); ManagedTopology is its managedTopology.
	AnchorMode      string `json:"anchorMode"`
	ManagedTopology string `json:"managedTopology,omitempty"`
	// Authority is this member's replication authority and installed image.
	Authority RootAnchorAuthority `json:"authority"`
	// Owner is the accepted root-anchor owner of the installed image. Phase
	// is "awaiting" (no owner yet), "dormant" (managed anchors never started)
	// or "managed"; Keys and the schedule are set only when managed.
	Owner RootAnchorOwner `json:"owner"`
	// Local is this member's node-local carrier: the keys it denied or
	// carried and whether its output is retired. Absent before schema 8.
	Local *RootAnchorLocal `json:"local,omitempty"`
	// AdmissionCurrent reports that this process still holds managed serving
	// admission. False on a managed node means that it publishes nothing new
	// until it restarts.
	AdmissionCurrent bool `json:"admissionCurrent"`
	// Updater is the elected writer's RFC 5011 updater; Supervisor is the
	// deny-only root check of every other role.
	Updater    RootAnchorWorker `json:"updater"`
	Supervisor RootAnchorWorker `json:"supervisor"`
	// Pull is the last scheduled or explicit pull from the primary on an
	// enrolled member that is not the primary. Absent on a primary and on a
	// standalone node.
	Pull *RootAnchorPull `json:"pull,omitempty"`
	// Warnings are fixed sentences for conditions an operator should look
	// at; see the managed-anchor runbooks for each.
	Warnings []string `json:"warnings"`
}

// RootAnchorAuthority is the member's replication authority as seen locally.
type RootAnchorAuthority struct {
	Mode               string `json:"mode"`
	MemberID           string `json:"memberID,omitempty"`
	TermEpoch          uint64 `json:"termEpoch"`
	InstalledTermEpoch uint64 `json:"installedTermEpoch"`
	// FencedBehindTerm is true when this member is fenced by a newer term
	// whose image it never installed. Such a member keeps serving its own
	// last-good data DNS-only; it can be kept only by abandoning that term
	// (cluster resolve --offline --abandon-term). It is not an instruction.
	FencedBehindTerm bool `json:"fencedBehindTerm"`
	Conflict         bool `json:"conflict"`
}

// RootAnchorOwner is the accepted owner envelope and its pure state.
type RootAnchorOwner struct {
	Phase          string `json:"phase"`
	Digest         string `json:"digest,omitempty"`
	Generation     uint64 `json:"generation"`
	PureGeneration uint64 `json:"pureGeneration"`
	PolicyID       string `json:"policyID,omitempty"`
	SeedDigest     string `json:"seedDigest,omitempty"`
	// Quarantined means every trusted key is lost: the node answers no
	// validated recursion until `kudurru anchors recover --offline`.
	Quarantined         bool            `json:"quarantined"`
	ClockUncertain      bool            `json:"clockUncertain"`
	Keys                []RootAnchorKey `json:"keys"`
	RecoveryGenerations []uint64        `json:"recoveryGenerations"`
	// CheckpointAt is the wall time of the last accepted sample; NextRefresh
	// is when the elected updater polls next (RFC 5011 section 2.3).
	CheckpointAt         *time.Time `json:"checkpointAt,omitempty"`
	NextRefresh          *time.Time `json:"nextRefresh,omitempty"`
	RetryIntervalSeconds float64    `json:"retryIntervalSeconds"`
}

// RootAnchorKey is one root key of the managed state.
type RootAnchorKey struct {
	// KeyTag is the tag of the key's DS record (the REVOKE bit cleared).
	// Tags are not unique; ID is the key's identity.
	KeyTag    uint16 `json:"keyTag"`
	Algorithm uint8  `json:"algorithm"`
	ID        string `json:"id"`
	// Status is AddPend, Valid, Missing or Revoked (RFC 5011 section 4).
	Status    string    `json:"status"`
	FirstSeen time.Time `json:"firstSeen"`
	// HoldDownRemainingSeconds is the add hold-down credit an AddPend key
	// still needs. Credit accrues only within one process session.
	HoldDownRemainingSeconds float64    `json:"holdDownRemainingSeconds,omitempty"`
	AcceptedAt               *time.Time `json:"acceptedAt,omitempty"`
	MissingSince             *time.Time `json:"missingSince,omitempty"`
	RevokedAt                *time.Time `json:"revokedAt,omitempty"`
}

// RootAnchorLocal is the node-local carrier of denied keys.
type RootAnchorLocal struct {
	Phase           string `json:"phase"`
	OwnerGeneration uint64 `json:"ownerGeneration"`
	RetiredThrough  uint64 `json:"retiredThrough"`
	// Retired means this member's output for its installed owner is retired
	// by a local denial: it answers SERVFAIL until it installs an owner that
	// revokes the denied key. Terminal means it can never publish again.
	Retired  bool               `json:"retired"`
	Terminal bool               `json:"terminal"`
	Denied   []RootAnchorDenial `json:"denied"`
}

// RootAnchorDenial is one key this member denied or carried.
type RootAnchorDenial struct {
	KeyTag               uint16 `json:"keyTag,omitempty"`
	ID                   string `json:"id"`
	FirstOwnerGeneration uint64 `json:"firstOwnerGeneration"`
	// RevokedByOwner is false while the installed owner still trusts the
	// key: a pending local denial.
	RevokedByOwner bool `json:"revokedByOwner"`
}

// RootAnchorWorker is one anchor worker's last recorded state. State is one
// of not_running, running, clock_wait, retry, raced, waiting_image, retired
// or stopped. Stopped and retired workers stay so for the authority term
// (updater) or for this member, term and role (supervisor), until a restart.
type RootAnchorWorker struct {
	State string     `json:"state"`
	Since *time.Time `json:"since,omitempty"`
	Next  *time.Time `json:"next,omitempty"`
	// Error is the fixed error category that stopped the worker, if any;
	// LastError keeps the last such category after the worker ended.
	Error     string `json:"error,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

// RootAnchorPull is the result of the last pull from the primary.
type RootAnchorPull struct {
	// Result is installed, unreachable, rejected or refused (an image whose
	// root-anchor owner this member refuses, D369).
	Result string    `json:"result"`
	At     time.Time `json:"at"`
	// Refusal names why this member refuses the primary's owner while the
	// refusal lasts: history, policy, dormant_owner, recovery_history,
	// carried_revocation or not_successor. RefusedSince is the first refusal
	// of this run; unreachable pulls do not end it.
	Refusal      string     `json:"refusal,omitempty"`
	RefusedSince *time.Time `json:"refusedSince,omitempty"`
	// RefusedKeyTags and RefusedKeyIDs name, for carried_revocation, the keys
	// this member knows to be revoked that the primary's owner still trusts.
	RefusedKeyTags []uint16 `json:"refusedKeyTags,omitempty"`
	RefusedKeyIDs  []string `json:"refusedKeyIDs,omitempty"`
}

// ClusterAnchorsRuntime excludes legacy, Kubernetes and static runtimes.
type ClusterAnchorsRuntime interface{ ClusterAnchorsAvailable() bool }

func (s *Server) registerClusterAnchors() {
	op := operation(http.MethodGet, "/cluster/anchors", "getClusterAnchors", "Read this node's managed root trust-anchor status", "cluster")
	op.Description = "Complete nonstatic SQLite runtime only. Requires cluster.read. A read-only view of this node's accepted root-anchor owner (RFC 5011 key states, hold-down, schedule, quarantine and recovery records), its node-local denials, its anchor workers and, on a member that pulls, whether it refuses the primary's owner and why. The response grants nothing and is not a recommendation. A fenced or conflicted member's HTTP API is closed; read its metrics (kudurru_root_anchor_*) or stop it and run `kudurru anchors status --offline`."
	available := func() error {
		runtime, ok := s.opts.CanonicalSettings.(ClusterAnchorsRuntime)
		if s.staticMode() || !ok || !runtime.ClusterAnchorsAvailable() || s.opts.AuthHandler == nil {
			return newError(http.StatusServiceUnavailable, KeyUnavailable, "root anchor status unavailable", "requires the complete nonstatic SQLite runtime")
		}
		return nil
	}
	s.forwardWithCapability(op, perm{resource: "cluster", verb: "read"}, nil, RootAnchorStatus{}, available)
}
