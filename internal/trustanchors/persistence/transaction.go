package persistence

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Staged is a PROPOSAL, not accepted durable authority. Do not reuse this version
// or its checkpoint until the outer original Store.Command has returned success.
// Callback/final-check/CAS/commit failure invalidates this result entirely. No
// State is returned that could accidentally be chained before durable success.
type Staged struct {
	ProposedVersion accepted.Version
}

// Load bounds allocation before reading the exact transaction's complete owner.
// It performs no repair, initialization, publication, or elapsed-time progress.
// Reads following staged writes remain staged until the outer Command succeeds.
func Load(ctx context.Context, tx *sqlite.CommandTx) (accepted.Snapshot, error) {
	rows, err := loadRows(ctx, tx)
	if err != nil {
		return accepted.Snapshot{}, err
	}
	return accepted.DecodeRows(rows, tx.Authority().ClusterID)
}

// InitializeDormant converts ONLY the exact singleton physical sentinel, or
// validates an unchanged existing normal root. Missing/orphan/unknown rows never
// mean empty history. Followers cannot enter this already-writer-only command.
// The actual shared migration/initialization mount is deliberately absent.
func InitializeDormant(ctx context.Context, tx *sqlite.CommandTx, original Original) (Staged, error) {
	if !original.initialization {
		return Staged{}, ErrOriginal
	}
	if err := original.enter(ctx, tx); err != nil {
		return Staged{}, err
	}
	rows, err := loadRows(ctx, tx)
	if err != nil {
		return Staged{}, err
	}
	if err := original.requireFinal(tx, nil); err != nil {
		return Staged{}, err
	}
	if !accepted.IsAwaitingOwner(rows) {
		current, err := accepted.DecodeRows(rows, tx.Authority().ClusterID)
		if err != nil {
			return Staged{}, err
		}
		if err := tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix); err != nil {
			return Staged{}, err
		}
		return Staged{ProposedVersion: current.Version()}, nil
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Staged{}, err
	}
	next, err := accepted.NewDormant(tx.Authority().ClusterID, hex.EncodeToString(entropy[:]), time.Now().UTC())
	if err != nil {
		return Staged{}, err
	}
	return stageRows(ctx, tx, rows, next)
}

// Bootstrap binds the exact dormant envelope AND original opaque bootstrap
// proof/proposal. It cannot replace managed history, re-seed, restamp a proof,
// reconstruct evidence from State, or substitute a confidential/default policy.
func Bootstrap(ctx context.Context, tx *sqlite.CommandTx, original Original, proposed trustanchors.State, observation trustanchors.Observation) (Staged, error) {
	current, rows, err := transitionBase(ctx, tx, original)
	if err != nil {
		return Staged{}, err
	}
	if current.Inspect().Phase != accepted.Dormant {
		return Staged{}, ErrPhase
	}
	if err := observation.CheckBootstrap(proposed, original.clock.Sample()); err != nil {
		return Staged{}, err
	}
	next, err := accepted.ProposeManaged(current, proposed, time.Now().UTC(), observation.ValidUntil())
	if err != nil {
		return Staged{}, err
	}
	if err := original.requireFinal(tx, &observation); err != nil {
		return Staged{}, err
	}
	return stageRows(ctx, tx, rows, next)
}

// Advance reloads accepted history from this transaction, compares both complete
// versions and invokes the pure transition with the SAME original observation
// and clock at actual entry. Arbitrary caller-supplied next states are unsupported.
func Advance(ctx context.Context, tx *sqlite.CommandTx, original Original, observation trustanchors.Observation) (Staged, error) {
	current, rows, err := transitionBase(ctx, tx, original)
	if err != nil {
		return Staged{}, err
	}
	state, err := current.State()
	if err != nil {
		return Staged{}, ErrPhase
	}
	nextState, err := trustanchors.Advance(state, observation, original.clock.Sample())
	if err != nil {
		return Staged{}, err
	}
	// Advance's checkpoint is the actual original-clock entry sample, not a
	// caller time, restored session, or an inferred failover/downtime duration.
	next, err := accepted.ProposeManaged(current, nextState, nextState.Inspect().Checkpoint.Wall, observation.ValidUntil())
	if err != nil {
		return Staged{}, err
	}
	if err := original.requireFinal(tx, &observation); err != nil {
		return Staged{}, err
	}
	return stageRows(ctx, tx, rows, next)
}

func transitionBase(ctx context.Context, tx *sqlite.CommandTx, original Original) (accepted.Snapshot, []replication.Entry, error) {
	if original.initialization || original.clock == nil {
		return accepted.Snapshot{}, nil, ErrOriginal
	}
	if err := original.enter(ctx, tx); err != nil {
		return accepted.Snapshot{}, nil, err
	}
	rows, err := loadRows(ctx, tx)
	if err != nil {
		return accepted.Snapshot{}, nil, err
	}
	current, err := accepted.DecodeRows(rows, tx.Authority().ClusterID)
	if err != nil {
		return accepted.Snapshot{}, nil, err
	}
	if current.Version() != original.expected {
		return accepted.Snapshot{}, nil, accepted.ErrCAS
	}
	return current, rows, nil
}

func loadRows(ctx context.Context, tx *sqlite.CommandTx) ([]replication.Entry, error) {
	if tx == nil {
		return nil, sqlite.ErrTransactionEnded
	}
	if err := tx.CheckSectionPrefixBounds(ctx, accepted.Section, accepted.Prefix, accepted.MaxRows, accepted.MaxDurableBytes); err != nil {
		return nil, err
	}
	return tx.ListSectionPrefix(ctx, accepted.Section, accepted.Prefix)
}

func stageRows(ctx context.Context, tx *sqlite.CommandTx, oldRows []replication.Entry, next accepted.Snapshot) (Staged, error) {
	// Full structural/bound/cross-row validation precedes the first write. The
	// old complete owner (or exact init sentinel) was validated by the caller.
	rows, err := accepted.EncodeRows(next)
	if err != nil {
		return Staged{}, err
	}
	if _, err := accepted.DecodeRows(rows, tx.Authority().ClusterID); err != nil {
		return Staged{}, err
	}
	old := map[string]replication.Entry{}
	for _, row := range oldRows {
		old[row.Key] = row
	}
	keep := map[string]bool{}
	for _, row := range rows {
		keep[row.Key] = true
		if prior, ok := old[row.Key]; ok && row.Key != accepted.RootKey {
			canonical, err := replication.ValidateEntry(accepted.Section, prior, replication.Limits{MaxObjectBytes: accepted.MaxRowBytes, MaxDepth: 8})
			if err != nil || !bytes.Equal(canonical.Data, row.Data) {
				return Staged{}, accepted.ErrInvalid
			}
		}
	}
	for _, row := range oldRows {
		if !keep[row.Key] {
			if err := tx.DeleteSection(ctx, accepted.Section, row.Key); err != nil {
				return Staged{}, err
			}
		}
	}
	for _, row := range rows {
		if prior, exists := old[row.Key]; !exists || !bytes.Equal(prior.Data, row.Data) {
			if err := tx.PutSection(ctx, accepted.Section, row); err != nil {
				return Staged{}, err
			}
		}
	}
	// Register AFTER our own validated writes, never before them. Registration
	// failure poisons/refuses the outer command, rolling back all staged rows.
	if err := tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix); err != nil {
		return Staged{}, err
	}
	return Staged{ProposedVersion: next.Version()}, nil
}
