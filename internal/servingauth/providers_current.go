package servingauth

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ProviderSourcesCurrent projects exact provider sources from the caller's
// concrete admitted transaction. It uses the complete bounded current capture;
// it returns detached descriptors, never a View or mutation authorization.
func ProviderSourcesCurrent(ctx context.Context, tx *sqlite.CommandTx, config []ProviderSource) ([]ProviderDescriptor, error) {
	if tx == nil {
		return nil, ErrInvalid
	}
	cut, err := captureCurrent(ctx, tx)
	if err != nil {
		return nil, err
	}
	return ProviderSources(ctx, cut, config)
}
