package internalapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// PathZones is GET /internal/zones: the authoritative zones a node serves
// with their current serials, polled every 5 s by edge supervisors to
// flush dnsdist's authoritative packet cache on serial change and to route
// zone names to the authoritative pool (dev-docs/design-record.md §3.6 step 5).
const PathZones = "/internal/zones"

// ResolverLease bounds remote packet-cache eligibility without a successful pull.
const ResolverLease = 15 * time.Second

// ResolverState is an observation of ONE original complete serving publication.
// It grants no cache command authority. Missing metadata denotes an older peer;
// Available=false explicitly denies packet caching for this observation.
type ResolverState struct {
	Version           int       `json:"version"`
	Node              string    `json:"node"`
	Instance          string    `json:"instance"`
	Publication       uint64    `json:"publication,string"`
	Identity          string    `json:"identity"`
	Phase             string    `json:"phase"`
	ObservedAt        time.Time `json:"observedAt"`
	ValidUntil        time.Time `json:"validUntil"`
	BlockingEnabled   bool      `json:"blockingEnabled"`
	DisabledUntil     time.Time `json:"disabledUntil,omitzero"`
	Available         bool      `json:"available"`
	UnavailableReason string    `json:"unavailableReason,omitempty"`
	// AuthoritativeAvailable is an explicit additional proof, never inferred
	// from resolver availability by older or partially upgraded sources.
	AuthoritativeAvailable bool   `json:"authoritativeAvailable,omitempty"`
	AuthoritativeReason    string `json:"authoritativeReason,omitempty"`
}

// ErrZonesUnsupported is returned by Client.Zones when the peer has no
// /internal/zones route (a 404 from an older node).
var ErrZonesUnsupported = errors.New("internalapi: peer does not serve " + PathZones)

// Zone is one entry of GET /internal/zones.
type Zone struct {
	// Name is the fully qualified zone name, lower case.
	Name string `json:"name"`
	// Serial is the SOA serial currently served.
	Serial uint32 `json:"serial"`
	// Type is "primary" or "secondary"; forward zones are not listed.
	Type string `json:"type,omitempty"`
}

// ZoneList is the body of GET /internal/zones.
type ZoneList struct {
	Node          string         `json:"node"`
	Zones         []Zone         `json:"zones"`
	Resolver      *ResolverState `json:"resolver,omitempty"`
	ResolverNonce string         `json:"resolverNonce,omitempty"`
}

// Zones fetches GET /internal/zones from addr (host:port).
func (c *Client) Zones(ctx context.Context, addr string) (ZoneList, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ZoneList{}, err
	}
	challenge := hex.EncodeToString(nonce[:])
	resp, err := c.get(ctx, addr, PathZones+"?resolverNonce="+challenge)
	if errors.Is(err, ErrListNotFound) {
		return ZoneList{}, ErrZonesUnsupported
	}
	if err != nil {
		return ZoneList{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return ZoneList{}, err
	}
	if len(data) > 4<<20 {
		return ZoneList{}, ErrTooLarge
	}
	var zl ZoneList
	if err := json.Unmarshal(data, &zl); err != nil {
		return ZoneList{}, fmt.Errorf("internalapi: decode zones from %s: %w", addr, err)
	}
	if zl.Resolver != nil && zl.ResolverNonce != challenge {
		return ZoneList{}, errors.New("internalapi: resolver observation challenge mismatch")
	}
	return zl, nil
}
