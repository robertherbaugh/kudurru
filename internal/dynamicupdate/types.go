// Package dynamicupdate implements bounded, storage-independent RFC 2136
// planning. A plan is valid only for the committed revision it was evaluated
// against. A CAS conflict requires fresh authorization and prerequisite checks.
package dynamicupdate

import (
	"fmt"

	"github.com/miekg/dns"
)

// Limits bound both incoming transactions and complete candidate zones.
type Limits struct {
	MaxUpdateRecords int
	MaxUpdateBytes   int64
	MaxRecords       int
	MaxBytes         int64
	// MaxPolicyBytes bounds preflight policy/index workspace, independently
	// of wire RRs. Oversized policy refuses before canonicalization or copies.
	MaxPolicyBytes int64
}

// Defaults returns limits with nonpositive fields replaced by finite defaults.
func (l Limits) Defaults() Limits {
	if l.MaxUpdateRecords <= 0 {
		l.MaxUpdateRecords = 4096
	}
	if l.MaxUpdateBytes <= 0 {
		l.MaxUpdateBytes = 65535
	}
	if l.MaxRecords <= 0 {
		l.MaxRecords = 1_000_000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 64 << 20
	}
	if l.MaxPolicyBytes <= 0 {
		l.MaxPolicyBytes = 128 << 20
	}
	return l
}

// ProtocolError is an RFC response code. No partial plan is returned on failure.
type ProtocolError struct {
	Rcode  int
	Reason string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("UPDATE %s: %s", dns.RcodeToString[e.Rcode], e.Reason)
}
func failure(code int, reason string) *ProtocolError {
	return &ProtocolError{Rcode: code, Reason: reason}
}

// Principal must come from transport-verified TSIG. Version identifies the
// captured credential, not an original-wire proof or secret.
type Principal struct {
	Key     string
	Version string
}

// Credential references a captured key version without storing a secret.
type Credential struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

// Grant uses an exact absolute owner or a leading *. suffix for strict
// descendants. Types is numeric; empty or ANY grants all eligible data types.
type Grant struct {
	Key   string   `json:"key"`
	Owner string   `json:"owner"`
	Types []uint16 `json:"types,omitempty"`
}

// Reservation protects source-owned identities, including records that are
// disabled, expired or view-specific. Type=0 protects the entire name; Subtree
// also protects descendants (for managed DNAME occlusion).
type Reservation struct {
	Name    string `json:"name"`
	Type    uint16 `json:"type,omitempty"`
	Subtree bool   `json:"subtree,omitempty"`
	Source  string `json:"source"`
}

// State is an immutable committed input. Plan never mutates its records/slices.
type State struct {
	Apex          string
	Records       []dns.RR
	UpdateEnabled bool
	AuthEpoch     uint64
	Credentials   []Credential
	Grants        []Grant
	Reservations  []Reservation
}

type prerequisite struct {
	name   string
	typ    uint16
	class  uint16
	values []string
}
type mutation struct {
	rr   dns.RR
	name string
	key  string
}

// Transaction is created only by Parse; its slices are private and copied.
type Transaction struct {
	apex          string
	prerequisites []prerequisite
	mutations     []mutation
}

// Apex returns the canonical UPDATE zone name.
func (t *Transaction) Apex() string { return t.apex }

// Result contains complete copied data; publication requires a durable CAS
// against the same committed head and AuthEpoch. AD must never be inferred.
type Result struct {
	Records           []dns.RR
	Serial            uint32
	Changed           bool
	AuthEpoch         uint64
	CredentialVersion string
}
