package persistence

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Initialize creates only a genuinely empty owner. Cross-owner bootstrap and
// current source/target/credential/license/lease admission belong to the caller.
// The returned snapshot is not committed until the surrounding Command succeeds.
func Initialize(ctx context.Context, tx *sqlite.CommandTx, clusterID string, admittedAt time.Time) (*Snapshot, error) {
	rows, err := loadRows(ctx, tx, clusterID)
	if err != nil {
		return nil, err
	}
	if len(rows) != 0 {
		return nil, ErrInitialized
	}
	state := inventory.State{FormatVersion: inventory.StateFormatVersion}
	next, err := Encode(state, clusterID, admittedAt)
	if err != nil {
		return nil, err
	}
	snapshot, err := Decode(next, clusterID, admittedAt)
	if err != nil {
		return nil, err
	}
	for _, row := range next {
		if err := tx.PutSection(ctx, Section, row); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

// Load reads the complete owner inside the caller's concrete admitted command.
// Historical payloads are bounded before allocation and never repaired/pruned.
func Load(ctx context.Context, tx *sqlite.CommandTx, clusterID string, at time.Time) (*Snapshot, error) {
	rows, err := loadRows(ctx, tx, clusterID)
	if err != nil {
		return nil, err
	}
	return Decode(rows, clusterID, at)
}

// Stage performs exact root CAS and one global generation advance in the same
// command as source heads, allocation and dedup. Cluster identity is exclusively
// tx.Authority(). The caller must return errors, register final immutable proof
// checks and retain dependency exclusions through the entire Command commit.
func Stage(ctx context.Context, tx *sqlite.CommandTx, expected Version, next inventory.State, admittedAt time.Time) (*Snapshot, error) {
	if tx == nil {
		return nil, sqlite.ErrTransactionEnded
	}
	clusterID := tx.Authority().ClusterID
	oldRows, err := loadRows(ctx, tx, clusterID)
	if err != nil {
		return nil, err
	}
	current, err := Decode(oldRows, clusterID, admittedAt)
	if err != nil {
		return nil, err
	}
	if current.Version != expected || current.Version.Generation >= math.MaxUint64-1 || next.Generation != current.Version.Generation+1 {
		return nil, ErrCAS
	}
	if admittedAt.Before(current.UpdatedAt) {
		return nil, ErrInvalid
	}
	rows, err := Encode(next, clusterID, admittedAt)
	if err != nil {
		return nil, err
	}
	snapshot, err := Decode(rows, clusterID, admittedAt)
	if err != nil {
		return nil, err
	}
	old := map[string]replication.Entry{}
	for _, row := range oldRows {
		old[row.Key] = row
	}
	keep := map[string]bool{}
	for _, row := range rows {
		keep[row.Key] = true
		if previous, ok := old[row.Key]; ok && row.Key != RootKey {
			canonical, err := validateRow(previous)
			if err != nil || !bytes.Equal(canonical, row.Data) {
				return nil, fmt.Errorf("%w: immutable chunk changed", ErrInvalid)
			}
		}
	}
	// All complete next/old evidence has been validated before the first write.
	// Delete only proven owned unused chunks, never foreign runtime authority.
	for _, row := range oldRows {
		if !keep[row.Key] {
			if err := tx.DeleteSection(ctx, Section, row.Key); err != nil {
				return nil, err
			}
		}
	}
	for _, row := range rows {
		if _, exists := old[row.Key]; !exists || row.Key == RootKey {
			if err := tx.PutSection(ctx, Section, row); err != nil {
				return nil, err
			}
		}
	}
	return snapshot, nil
}

func loadRows(ctx context.Context, tx *sqlite.CommandTx, clusterID string) ([]replication.Entry, error) {
	if tx == nil {
		return nil, sqlite.ErrTransactionEnded
	}
	// This first concrete call checks original cancellation/transaction liveness.
	if err := tx.CheckSectionPrefixBounds(ctx, Section, Prefix, MaxRows, MaxDurableBytes); err != nil {
		return nil, err
	}
	if tx.Authority().ClusterID != clusterID {
		return nil, replication.ErrWrongCluster
	}
	rows, err := tx.ListSectionPrefix(ctx, Section, Prefix)
	if err != nil {
		return nil, err
	}
	if _, err := ownerBounds(rows); err != nil {
		return nil, err
	}
	return rows, nil
}
