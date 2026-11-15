package security

import (
	"context"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ValidateEntry composes the independent security owners for local writes and
// replica validation. Complete inventories require ValidateImage as well.
func ValidateEntry(section string, entry replication.Entry) error {
	if err := servingauth.ValidateEntry(section, entry); err != nil {
		return err
	}
	return providerregistry.ValidateEntry(section, entry)
}

// ValidateImage requires explicit complete auth and provider-registry roots.
// It performs no initialization, configuration reconciliation or network I/O.
func ValidateImage(image *replication.Image) error {
	if image == nil || !image.Complete {
		return servingauth.ErrManifest
	}
	if _, err := servingauth.DecodeSecurity(image.Payload.Security); err != nil {
		return err
	}
	return providerregistry.ValidateImage(image)
}

// InitializeOwners explicitly initializes or migrates both security owners in
// one admitted startup command. Followers receive roots from their primary and
// must never invoke this helper. Existing incomplete history is never reset.
func InitializeOwners(ctx context.Context, tx *sqlite.CommandTx) error {
	if err := Initialize(ctx, tx); err != nil {
		return err
	}
	_, err := tx.GetSection(ctx, "security", providerregistry.RootKey)
	if errors.Is(err, store.ErrNotFound) {
		plan, err := providerregistry.PlanInitialize(tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		return plan.Stage(ctx, tx)
	}
	if err != nil {
		return err
	}
	rows, err := readSecurityRows(ctx, tx)
	if err != nil {
		return err
	}
	_, err = providerregistry.Decode(rows, tx.Authority().ClusterID)
	return err
}

// readSecurityRows rejects malformed historical or staged section sizes before
// allocating their payloads. Open/raw migration does not imply owner validity.
func readSecurityRows(ctx context.Context, tx *sqlite.CommandTx) ([]replication.Entry, error) {
	if err := tx.CheckSectionBounds(ctx, "security", servingauth.MaxRows, servingauth.MaxSecurityBytes); err != nil {
		if errors.Is(err, replication.ErrTooLarge) {
			return nil, errors.Join(servingauth.ErrBounds, err)
		}
		return nil, err
	}
	return tx.ListSections(ctx, "security")
}
