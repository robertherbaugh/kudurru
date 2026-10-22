package security

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ReconcileProviderConfigs stages a complete, explicitly admitted config-source
// set and all required revocation floors. The caller owns authorization and
// current host/provider exclusion through the full Store.Command, including
// no-op replay. This helper neither activates resources nor samples node config.
func ReconcileProviderConfigs(ctx context.Context, tx *sqlite.CommandTx, desired []providerregistry.DesiredConfig, at time.Time) error {
	registry, err := currentProviderRegistry(ctx, tx)
	if err != nil {
		return err
	}
	plan, err := providerregistry.PlanReconcile(registry, desired, at, func() (string, error) {
		var uid [16]byte
		_, _ = rand.Read(uid[:])
		return hex.EncodeToString(uid[:]), nil
	})
	if err != nil {
		return err
	}
	return stageProviderPlan(ctx, tx, plan, at)
}

// AcceptProviderAuthorities stages the complete BOTH-origin active inventory of
// admitted exact source/resource receipts and floors for changes or withdrawals.
// The caller must hold the source and resource pin
// through the whole command and register final memory/clock admission checks.
// Actual resource activation happens only after this command commits.
func AcceptProviderAuthorities(ctx context.Context, tx *sqlite.CommandTx, desired []providerregistry.DesiredAuthority, at time.Time) error {
	registry, err := currentProviderRegistry(ctx, tx)
	if err != nil {
		return err
	}
	plan, err := providerregistry.PlanAcceptAuthority(registry, desired, at)
	if err != nil {
		return err
	}
	return stageProviderPlan(ctx, tx, plan, at)
}

func currentProviderRegistry(ctx context.Context, tx *sqlite.CommandTx) (*providerregistry.Registry, error) {
	if tx == nil {
		return nil, providerregistry.ErrInvalid
	}
	rows, err := readSecurityRows(ctx, tx)
	if err != nil {
		return nil, err
	}
	return providerregistry.Decode(rows, tx.Authority().ClusterID)
}

func stageProviderPlan(ctx context.Context, tx *sqlite.CommandTx, plan *providerregistry.Plan, at time.Time) error {
	state, err := Load(ctx, tx)
	if err != nil {
		return err
	}
	for _, targets := range [][]providerregistry.ProviderIncarnation{plan.Retired(), plan.Changed()} {
		for _, target := range targets {
			if err := state.InvalidateProvider(target.ID, target.UID, at); err != nil {
				return err
			}
		}
	}
	// Stage checks the actual already-staged floor, not this in-memory State.
	if err := state.Commit(ctx); err != nil {
		return err
	}
	return plan.Stage(ctx, tx)
}
