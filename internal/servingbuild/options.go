package servingbuild

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Dependencies is one immutable host/runtime cut. Capture returns an owned
// reference even on failure; the builder releases it exactly once. Current must
// be a cheap thread-safe memory cursor, with no Store/network I/O or side effects.
// Nil runtime snapshots explicitly mean no acquired data, preserving coverage.
type Dependencies struct {
	Identity  string
	Host      *config.Config
	Catalog   CatalogResource
	Inventory *inventory.Snapshot
	Auth      servingauth.Dependencies
	Keys      *dnsserver.StaticKeyring
	Secondary *secondary.Snapshot
	Stub      *stub.Snapshot
	RPZ       *rpz.Snapshot
	Current   func() bool
	Release   func()
}

// Options supplies local infrastructure and a complete dependency capture.
// Capture receives an immutable candidate and must bind its resources to this
// candidate's configuration. Authority reads only the store's in-memory cursor.
type Options struct {
	Schema     *Schema
	Lists      *listfetcher.DiskCache
	Namespace  string
	Capture    func(context.Context, *replication.Image) (Dependencies, error)
	Authority  func() (replication.Authority, bool)
	Now        func() time.Time
	Upstreams  upstream.Options
	ZoneLimits zonestate.Limits
	MaxZones   int
}
