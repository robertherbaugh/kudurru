package internalapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PathZones is GET /internal/zones: the authoritative zones a node serves
// with their current serials, polled every 5 s by edge supervisors to
// flush dnsdist's authoritative packet cache on serial change and to route
// zone names to the authoritative pool (docs/architecture.md §3.6 step 5).
//
// Only the client side exists so far; the route lands on the server with
// the zone status work (docs/followups.md). Until then Client.Zones
// returns ErrZonesUnsupported.
const PathZones = "/internal/zones"

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
	Node  string `json:"node"`
	Zones []Zone `json:"zones"`
}

// Zones fetches GET /internal/zones from addr (host:port).
func (c *Client) Zones(ctx context.Context, addr string) (ZoneList, error) {
	resp, err := c.get(ctx, addr, PathZones)
	if errors.Is(err, ErrListNotFound) {
		return ZoneList{}, ErrZonesUnsupported
	}
	if err != nil {
		return ZoneList{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var zl ZoneList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&zl); err != nil {
		return ZoneList{}, fmt.Errorf("internalapi: decode zones from %s: %w", addr, err)
	}
	return zl, nil
}
