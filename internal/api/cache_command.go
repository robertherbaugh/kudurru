package api

import "context"

// CacheFlushCommand admits a flush of this node's response cache. An empty name
// means all entries; otherwise name is a canonical absolute DNS name. Removed
// counts actual local work, including when cancellation follows that work.
// Installed refusals never fall back to the legacy cache path.
type CacheFlushCommand func(ctx context.Context, name string) (removed int, err error)
