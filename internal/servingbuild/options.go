package servingbuild

import (
	"context"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
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
	Identity string
	Host     *config.Config
	Catalog  CatalogResource
	Auth     servingauth.Dependencies
	// Browser selects an exchange capability from this same retained provider
	// cut. Nil denies browser login; it never triggers live provider acquisition.
	Browser func(servingauth.ProviderRef, string) (*providerresources.Browser, error)
	Keys    *dnsserver.StaticKeyring
	// KeyringIdentity names the local installation that supplied Keys, from the
	// same KeyringCut. Equal key bytes after replacement do not reuse this ID.
	KeyringIdentity string
	Secondary       *secondary.Snapshot
	Stub            *stub.Snapshot
	RPZ             *rpz.Snapshot
	Current         func() bool
	Release         func()
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
	// PrepareIntegrations is a pure root-owned complete-image adapter. It may
	// suppress ineligible source claims and overlay granted private MAC members;
	// it must never acquire resources, perform I/O or confer write authority.
	PrepareIntegrations func(context.Context, *replication.Image, *primaryruntime.PreparedImage, time.Time) (*InventoryOwner, []*public.ClientGroup, error)
}
