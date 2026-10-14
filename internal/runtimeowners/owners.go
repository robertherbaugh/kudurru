// Package runtimeowners composes durable owner validation and explicit startup
// initialization. Serving and replica reads validate complete evidence; only an
// admitted writer startup may initialize a previously empty owner.
package runtimeowners

import (
	"context"
	"errors"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ValidateEntry checks each registered owner without granting completeness.
func ValidateEntry(section string, row replication.Entry) error {
	if err := security.ValidateEntry(section, row); err != nil {
		return err
	}
	return persistence.ValidateEntry(section, row)
}

// ValidateImage requires all security and global inventory manifests, including
// explicit empty owners. It never synthesizes missing history from client rows.
func ValidateImage(image *replication.Image) error {
	if err := security.ValidateImage(image); err != nil {
		return err
	}
	return persistence.ValidateImage(image)
}

// Initialize stages all owner initialization/migration in one concrete command.
// The caller retains original writer admission through the entire commit.
// Existing inventory must decode unchanged; a missing root beside any private
// source authority cannot be interpreted as an empty historical inventory.
func Initialize(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
	if tx == nil || at.IsZero() {
		return persistence.ErrInvalid
	}
	if err := security.InitializeOwners(ctx, tx); err != nil {
		return err
	}
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
