// Package persistence owns bounded complete inventory evidence in the runtime
// replication section. It is independent of enterprise sources and admission.
package persistence

import (
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
)

// Runtime owner framing and hard allocation budgets.
const (
	Section         = "runtime"
	Prefix          = "inventory/"
	RootKey         = "inventory/v1/root"
	ChunkPrefix     = "inventory/v1/chunks/"
	FormatVersion   = 1
	MaxDurableBytes = 64 << 20
	MaxRowBytes     = 256 << 10
	MaxChunkBytes   = 180 << 10
	MaxChunks       = 256
	MaxRows         = MaxChunks + 1
	MaxStateBytes   = MaxChunks * MaxChunkBytes
)

// Errors distinguish invalid evidence, bounded allocation, initialization and CAS.
var (
	ErrInvalid       = errors.New("inventory/persistence: invalid complete evidence")
	ErrTooLarge      = errors.New("inventory/persistence: durable evidence exceeds bounds")
	ErrUninitialized = errors.New("inventory/persistence: missing root")
	ErrInitialized   = errors.New("inventory/persistence: owner already exists")
	ErrCAS           = errors.New("inventory/persistence: generation changed")
)

// ChunkRef preserves order in the original complete State byte stream.
type ChunkRef struct {
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

// Root binds the entire global inventory, including original removal watermarks.
type Root struct {
	FormatVersion int        `json:"formatVersion"`
	ClusterID     string     `json:"clusterID"`
	Generation    uint64     `json:"generation,string"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	StateDigest   string     `json:"stateDigest"`
	StateBytes    int        `json:"stateBytes"`
	Chunks        []ChunkRef `json:"chunks"`
}

// Chunk is immutable; Digest addresses the original decoded bytes, not JSON.
type Chunk struct {
	FormatVersion int    `json:"formatVersion"`
	Digest        string `json:"digest"`
	Data          []byte `json:"data"`
}

// Version is the exact generation and canonical-root digest for same-command CAS.
type Version struct {
	Generation uint64
	Digest     string
}

// Snapshot is detached evidence, staged until its surrounding Command commits.
type Snapshot struct {
	State     inventory.State
	Version   Version
	UpdatedAt time.Time
}
