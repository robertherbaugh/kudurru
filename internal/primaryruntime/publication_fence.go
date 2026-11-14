package primaryruntime

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// PublicationFence is the caller's captured writer, never a sampled replacement
// term. Context metadata is not credential or TSIG verification authority.
type PublicationFence struct {
	Term        replication.Term
	Incarnation uint64
}
type publicationFenceKey struct{}

// WithPublicationFence binds the exact root-issued writer to background work.
// Invalid fences remain invalid and are rejected at admission/publication.
func WithPublicationFence(ctx context.Context, fence PublicationFence) context.Context {
	return context.WithValue(ctx, publicationFenceKey{}, fence)
}

// PublicationFenceFrom returns only a nonzero valid captured writer fence.
func PublicationFenceFrom(ctx context.Context) (PublicationFence, bool) {
	f, ok := ctx.Value(publicationFenceKey{}).(PublicationFence)
	return f, ok && f.Term.Validate() == nil && f.Incarnation != 0
}
