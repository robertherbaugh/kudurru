// Package accepted frames bounded complete managed-anchor history. Decoding
// establishes structure, not authentication, durable acceptance, or serving
// authority. A proposed snapshot remains staged until its outer Command commits.
package accepted

import (
	"errors"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// Owner framing and allocation limits are independent of other runtime owners.
const (
	Section           = "runtime"
	Prefix            = "trustanchors/"
	RootKey           = "trustanchors/v1/root"
	ChunkPrefix       = "trustanchors/v1/chunks/"
	FormatVersion     = 1
	MaxRootBytes      = 32 << 10
	MaxRowBytes       = 96 << 10
	MaxChunkBytes     = 64 << 10
	MaxChunks         = 32
	MaxRows           = MaxChunks + 1
	MaxDurableBytes   = 4 << 20
	AwaitingOwnerJSON = `{"formatVersion":1,"phase":"awaiting-owner"}`
)

// Fixed errors do not disclose received records or proofs.
var (
	ErrInvalid = errors.New("trustanchors/accepted: invalid owner")
	ErrBounds  = errors.New("trustanchors/accepted: owner exceeds bounds")
	ErrMissing = errors.New("trustanchors/accepted: missing owner")
	ErrCAS     = errors.New("trustanchors/accepted: original owner changed")
)

// Phase distinguishes never-managed history from retained managed history.
type Phase string

// Ordinary phases exclude the physical-migration sentinel.
const (
	Dormant Phase = "dormant"
	Managed Phase = "managed"
)

// PureVersion binds the complete pure history, including checkpoints/tombstones.
type PureVersion struct {
	Generation uint64 `json:"generation,string"`
	Digest     string `json:"digest"`
}

// Version binds BOTH the envelope and pure versions. A value is not a receipt
// of commit; a transaction's proposed version is reusable only after success.
type Version struct {
	Generation uint64      `json:"generation,string"`
	Digest     string      `json:"digest"`
	Pure       PureVersion `json:"pure"`
}

// ChunkRef preserves the exact byte-stream order, including repeated content.
type ChunkRef struct {
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

// Root binds immutable cluster/history/policy identity and retained pure history.
// Previous and ObservationUntil are transition metadata, never an Observation
// serialization, signature proof, or independent source of grant authority.
type Root struct {
	FormatVersion    int         `json:"formatVersion"`
	TrustPoint       string      `json:"trustPoint"`
	ClusterID        string      `json:"clusterID"`
	HistoryID        string      `json:"historyID"`
	Phase            Phase       `json:"phase"`
	Generation       uint64      `json:"generation,string"`
	InitializedAt    time.Time   `json:"initializedAt"`
	UpdatedAt        time.Time   `json:"updatedAt"`
	PolicyID         string      `json:"policyID"`
	SeedDigest       string      `json:"seedDigest"`
	Pure             PureVersion `json:"pure"`
	StateBytes       int         `json:"stateBytes"`
	Chunks           []ChunkRef  `json:"chunks"`
	Previous         Version     `json:"previous"`
	ObservationUntil time.Time   `json:"observationUntil"`
}

// Chunk is immutable and addressed by its raw decoded content.
type Chunk struct {
	FormatVersion int    `json:"formatVersion"`
	Digest        string `json:"digest"`
	Data          []byte `json:"data"`
}

// Snapshot is immutable structurally validated evidence, not commit authority.
type Snapshot struct {
	root    Root
	state   trustanchors.State
	version Version
}

// Inspect returns detached envelope metadata.
func (s Snapshot) Inspect() Root {
	r := s.root
	r.Chunks = slices.Clone(r.Chunks)
	return r
}

// Version returns the complete structural CAS identity without certifying commit.
func (s Snapshot) Version() Version { return s.version }

// State returns the immutable pure history only for managed owners. The caller
// must establish its accepted durable source before using it as transition input.
func (s Snapshot) State() (trustanchors.State, error) {
	if s.root.Phase != Managed || s.version.Generation == 0 {
		return trustanchors.State{}, ErrInvalid
	}
	return s.state, nil
}
