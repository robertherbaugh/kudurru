package api

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// NodeConfigReceipt contains detached local documents before and after an
// admitted command. It describes desired readback, not a complete serving cut.
type NodeConfigReceipt struct {
	Before *config.Config
	After  *config.Config
}

// NodeConfigCommand admits requested scalar leaves against current raw proof
// and the original local writer, then adopts local operational settings. Only
// values named in changed may replace the current document; next as a whole
// must never be installed. Success acknowledges logger level adoption, while
// restart-required format remains an in-memory desired value. No forwarding or
// durable/local atomic transaction is implied. Nil preserves legacy behavior.
type NodeConfigCommand func(ctx context.Context, next *config.Config, changed []string) (NodeConfigReceipt, error)
