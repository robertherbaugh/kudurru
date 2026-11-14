// Package runtimeowners composes durable owner validation and explicit startup
// initialization. Serving and replica reads validate complete evidence; only an
// admitted writer startup may initialize a previously empty owner.
package runtimeowners

import (
	"context"
	"errors"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	loginlimits "github.com/robertherbaugh/kudurru/internal/auth/loginlimit/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/auth/requestlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	enrollmentowner "github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	enrollmentpersistence "github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	anchorpersistence "github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

// ValidateEntry checks each registered owner without granting completeness.
func ValidateEntry(section string, row replication.Entry) error {
	if err := enrollmentowner.ValidateEntry(section, row); err != nil {
		return err
	}
	if err := requestlimit.ValidateEntry(section, row); err != nil {
		return err
	}
	if err := loginlimits.ValidateEntry(section, row); err != nil {
		return err
	}
	if err := security.ValidateEntry(section, row); err != nil {
		return err
	}
	if err := persistence.ValidateEntry(section, row); err != nil {
		return err
	}
	if err := edgeblock.ValidateEntry(section, row); err != nil {
		return err
	}
	return accepted.ValidateEntry(section, row)
}

// ValidateImage requires security, inventory and root-anchor manifests, including
// explicit empty owners. It never synthesizes missing history from client rows.
func ValidateImage(image *replication.Image) error {
	if err := requestlimit.ValidateImage(image); err != nil {
		return err
	}
	if err := loginlimits.ValidateImage(image); err != nil {
		return err
	}
	if err := security.ValidateImage(image); err != nil {
		return err
	}
	if err := persistence.ValidateImage(image); err != nil {
		return err
	}
	if err := accepted.ValidateImage(image); err != nil {
		return err
	}
	if err := edgeblock.ValidateImage(image); err != nil {
		return err
	}
	return enrollmentowner.ValidateImage(image)
}

// Initialize stages all owner initialization/migration in one concrete command.
// The caller retains original writer admission through the entire commit.
// Existing inventory must decode unchanged; a missing root beside any private
// source authority cannot be interpreted as an empty historical inventory.
func Initialize(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
	if tx == nil || at.IsZero() {
		return persistence.ErrInvalid
	}
	// Retain the callback's already-admitted writer before any owner writes.
	// Neither this composition nor the anchor helper samples a successor token.
	original, err := anchorpersistence.CaptureInitialization(ctx)
	if err != nil {
		return err
	}
	if err := security.InitializeOwners(ctx, tx); err != nil {
		return err
	}
	if err := enrollmentpersistence.InitializeDormant(ctx, tx); err != nil {
		return err
	}
	if err := initializeInventory(ctx, tx, at); err != nil {
		return err
	}
	// Existing inventory must not bypass anchor validation on restart. Missing
	// current-schema history is an error, never an invitation to initialize it.
	_, err = anchorpersistence.InitializeDormant(ctx, tx, original)
	if err != nil {
		return err
	}
	current, err := anchorpersistence.Load(ctx, tx)
	if err != nil {
		return err
	}
	if current.Inspect().Phase == accepted.Dormant {
		// The physical migration supplies provenance; the original admitted
		// startup command binds this member's private carrier to the owner in
		// the same transaction, including an unchanged dormant owner.
		return tx.InitializeAnchorLocal(ctx)
	}
	// Managed startup never initializes or resets private history. The caller
	// must bind and validate the actual committed owner plus local carrier.
	return nil
}

func initializeInventory(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
	_, err := tx.GetSection(ctx, persistence.Section, persistence.RootKey)
	if err == nil {
		_, err = persistence.Load(ctx, tx, tx.Authority().ClusterID, at)
		return err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	for _, kind := range integration.SourceKinds {
		list, err := tx.List(ctx, store.Kind(kind), store.ListOptions{Limit: 1})
		if err != nil {
			return err
		}
		if len(list.Items) != 0 || list.Continue != "" {
			return persistence.ErrUninitialized
		}
	}
	// The owner itself rejects orphan chunks and unknown-version rows before
	// staging its first write. Earlier security migration rolls back on error.
	_, err = persistence.Initialize(ctx, tx, tx.Authority().ClusterID, at)
	return err
}
