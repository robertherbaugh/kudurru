package inventory

import (
	"net/netip"
	"time"
)

// AddressState explains what one device address means for DNS identity in a
// captured snapshot (D196, D397). Only AddressAssigned enriches a query.
type AddressState string

// Address states. A device keeps every address its source reported; the state
// says whether the ownership arbitration of Replace gave it the address.
const (
	// AddressAssigned: this device holds the newest live claim on the address.
	AddressAssigned AddressState = "assigned"
	// AddressExpired: this device holds the newest claim, but its observation
	// passed its absolute expiry, so queries from the address get no identity.
	AddressExpired AddressState = "expired"
	// AddressAmbiguous: equally new claims of different devices collide, so no
	// device identifies the address until one source reports a newer claim.
	AddressAmbiguous AddressState = "ambiguous"
	// AddressSuperseded: a newer claim of another device (or that device's
	// newer removal) owns the address.
	AddressSuperseded AddressState = "superseded"
	// AddressWithdrawn: this device's own source later removed or re-dated
	// the claim; the address is not restored from the older observation.
	AddressWithdrawn AddressState = "withdrawn"
)

// Address is one reported address of a device with its arbitration result.
// OwnerSource and OwnerID name the winning claim for AddressSuperseded.
type Address struct {
	IP          netip.Addr
	State       AddressState
	OwnerSource string
	OwnerID     string
}

// Device is a private copy of one inventory client with its staleness and the
// arbitration result of each address. It never reads the DNS query path.
type Device struct {
	Client
	// Stale reports that the observation passed ExpiresAt at the read time.
	Stale     bool
	Addresses []Address
}

// Devices returns every client of the snapshot, including stale ones and
// those whose addresses are owned elsewhere, sorted by source and ID. at is
// the read time used for staleness; it is the same rule Lookup applies.
func (s *Snapshot) Devices(at time.Time) []Device {
	if s == nil {
		return nil
	}
	out := make([]Device, 0, len(s.clients))
	for _, c := range s.clients {
		d := Device{Client: clone(*c), Stale: !at.Before(c.ExpiresAt), Addresses: make([]Address, 0, len(c.IPs))}
		for _, ip := range c.IPs {
			d.Addresses = append(d.Addresses, s.address(c, ip, at))
		}
		out = append(out, d)
	}
	return out
}

func (s *Snapshot) address(c *Client, ip netip.Addr, at time.Time) Address {
	a := Address{IP: ip}
	p, ok := s.byIP[ip]
	switch {
	case ok && p.client == c:
		a.State = AddressAssigned
		if !at.Before(p.expires) {
			a.State = AddressExpired
		}
	case !ok || p.source == "" && p.id == "":
		// Replace records every reported address, so a missing claim can only
		// be an ambiguous tie that also lost its watermark; treat it as one.
		a.State = AddressAmbiguous
	case p.source == c.Source && p.id == c.ID:
		a.State = AddressWithdrawn
	default:
		a.State, a.OwnerSource, a.OwnerID = AddressSuperseded, p.source, p.id
	}
	return a
}
