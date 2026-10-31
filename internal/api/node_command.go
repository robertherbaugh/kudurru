package api

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// NodeConfigReceipt contains detached local documents before and after an
// admitted command. It describes desired readback, not a complete serving cut.
type NodeConfigReceipt struct {
	Before    *config.Config
	After     *config.Config
	Persisted bool
}

// NodeConfigCommand admits requested scalar leaves against current raw proof
// and the original local writer, then adopts local operational settings. Only
// values named in changed may replace the current document; next as a whole
// must never be installed. Success acknowledges logger level adoption, while
// restart-required format is desired until restart. reset lists original PATCH
// nulls: remove their local overlay rather than persist a default. Persisted
// acknowledges durable local commit only; no cluster replication is implied.
// Nil preserves legacy behavior.
type NodeConfigCommand func(ctx context.Context, next *config.Config, changed []string, reset ...string) (NodeConfigReceipt, error)
