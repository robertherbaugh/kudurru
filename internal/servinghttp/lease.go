package servinghttp

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// CloneLease transfers a new reference to the original request's complete cut
// to the caller. A retained cut remains valid after replacement; this grants
// neither current writer authority nor verified DNS transport credentials.
func CloneLease(ctx context.Context) (*serving.Lease, error) {
	if ctx == nil {
		return nil, serving.ErrReleased
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	original, _ := ctx.Value(leaseKey{}).(*serving.Lease)
	lease, err := original.Clone()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

// SourceToken returns the source captured before this request's authentication
// and body acquisition. It never reads or captures a replacement Store image.
func SourceToken(ctx context.Context) (sqlite.GenerationToken, error) {
	if ctx == nil {
		return sqlite.GenerationToken{}, serving.ErrReleased
	}
	if err := ctx.Err(); err != nil {
		return sqlite.GenerationToken{}, err
	}
	original, _ := ctx.Value(leaseKey{}).(*serving.Lease)
	token, err := original.SourceToken()
	if err != nil {
		return sqlite.GenerationToken{}, err
	}
	if err := ctx.Err(); err != nil {
		return sqlite.GenerationToken{}, err
	}
	return token, nil
}
