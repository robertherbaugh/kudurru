package servinghttp

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
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
	original, _ := requestLease(ctx)
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
	original, _ := requestLease(ctx)
	token, err := original.SourceToken()
	if err != nil {
		return sqlite.GenerationToken{}, err
	}
	if err := ctx.Err(); err != nil {
		return sqlite.GenerationToken{}, err
	}
	return token, nil
}

// SourceMode reports the replication mode of the source captured for this
// request. Unlike SourceToken it also answers after the request's context has
// ended, for as long as the handler holds its lease, so that a handler can
// explain a refusal instead of mistaking an ended request for one that began
// in another role. It returns no token and grants nothing.
func SourceMode(ctx context.Context) (replication.Mode, error) {
	mode, _, err := SourceRole(ctx)
	return mode, err
}

// SourceRole is SourceMode with the display name of the primary in the
// captured source's term. Like SourceMode it answers for an ended request
// while the handler holds its lease, returns no token and grants nothing.
func SourceRole(ctx context.Context) (replication.Mode, string, error) {
	if ctx == nil {
		return "", "", serving.ErrReleased
	}
	original, _ := requestLease(ctx)
	token, err := original.SourceToken()
	if err != nil {
		return "", "", err
	}
	authority := token.Authority()
	return authority.Mode, authority.Term.PrimaryName, nil
}
