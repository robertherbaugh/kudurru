// Package unifi acquires bounded client observations off the DNS query path.
// Publication and allocation authority remain with the integration host.
package unifi

import (
	"context"
	"errors"
	"net/netip"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

const (
	maxClients  = 10000
	maxNetworks = 256
	maxPages    = 128
	pageSize    = 200
	maxResponse = 4 << 20
	maxSnapshot = 32 << 20
	maxAttempts = 1000
)

// Errors contain no authenticated response, URL or credential value.
var (
	ErrInvalid    = errors.New("unifi: invalid configuration or observation")
	ErrDrift      = errors.New("unifi: observation changed")
	ErrAllocation = errors.New("unifi: explicit stable name allocation required")
	ErrCapacity   = errors.New("unifi: observation capacity exceeded")
)

// Device separates current address evidence from supplementary legacy metadata.
// Historical-only rows cannot renew ObservedAt or ExpiresAt.
type Device struct {
	ID, MAC, Name, Alias, Hostname, LocalDNSName, NetworkID, Note string
	IPs                                                           []netip.Addr
	ObservedAt, ExpiresAt                                         time.Time
}

// Network describes authenticated concrete prefixes, never guessed delegation.
type Network struct {
	ID, Name, Domain string
	VLAN             int
	Enabled          bool
	Prefixes         []netip.Prefix
}

// Observation is a complete coherent source input. Values are private copies.
type Observation struct {
	SiteID     string
	ObservedAt time.Time
	Clients    []Device
	Networks   []Network
	Complete   bool
}

// NameRequest binds a stable client ID to its requested relative base label.
type NameRequest struct{ ID, Base string }

// TranslationInput binds pure translation to an explicit immutable name map.
// Allocations maps client IDs to relative base/base-2 labels; it is never mutated.
type TranslationInput struct {
	Owner       host.InstanceID
	Config      public.UniFiIntegration
	Zone        *public.DNSZone
	Observation Observation
	Allocations map[string]string
}

// Diagnostics describes unsupported/ambiguous facts without private bodies.
type Diagnostics struct {
	AmbiguousIPs, UnassignedNetworks, NoMACGroups, HistoricalWithoutTime int
	LegacyUnavailable                                                    []string
}

// Options delegates durable allocations to root's current guarded admission.
// The callback receives the original request scope and private captured inputs.
type Options struct {
	Allocations func(context.Context, host.InstanceID, *public.DNSZone, []NameRequest) (map[string]string, error)
}
