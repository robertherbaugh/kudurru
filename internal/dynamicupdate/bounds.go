package dynamicupdate

import "fmt"

// Policy bounds apply before canonical names, indexes or slice copies are
// allocated. Presentation names may escape every DNS octet as four characters.
const (
	MaxNameBytes       = 4 * 255
	MaxIdentityBytes   = 512
	MaxPolicyEntries   = 4096
	MaxGrantTypes      = 65535
	MaxTotalGrantTypes = 65535
	MaxReservations    = 2_000_000
)

// PolicyAllocation checks per-field/count limits and charges a conservative
// workspace budget for strings, canonical names, copied slices, sort keys and
// reservation indexes. Charges accumulate by subtraction to avoid overflow.
// Shared backing slices still count once per logical grant; aliases cannot
// bypass a later copy's allocation limit. No names or types are copied here.
func PolicyAllocation(credentials []Credential, grants []Grant, reservations []Reservation, maxBytes int64) (int64, error) {
	if maxBytes <= 0 || len(credentials) > MaxPolicyEntries || len(grants) > MaxPolicyEntries || len(reservations) > MaxReservations {
		return 0, fmt.Errorf("policy count/budget exceeded")
	}
	left := maxBytes
	charge := func(n int64) bool {
		if n < 0 || n > left {
			return false
		}
		left -= n
		return true
	}
	if !charge(int64(len(credentials))*64) || !charge(int64(len(grants))*128) || !charge(int64(len(reservations))*160) {
		return 0, fmt.Errorf("policy workspace exceeded")
	}
	for _, c := range credentials {
		if len(c.Key) > MaxNameBytes || len(c.Version) > MaxIdentityBytes {
			return 0, fmt.Errorf("credential string limit exceeded")
		}
		if !charge(int64(len(c.Key))*4) || !charge(int64(len(c.Version))) {
			return 0, fmt.Errorf("credential workspace exceeded")
		}
	}
	typesLeft := MaxTotalGrantTypes
	for _, g := range grants {
		if len(g.Key) > MaxNameBytes || len(g.Owner) > MaxNameBytes+2 || len(g.Types) > MaxGrantTypes || len(g.Types) > typesLeft {
			return 0, fmt.Errorf("grant field/count limit exceeded")
		}
		typesLeft -= len(g.Types)
		if !charge(int64(len(g.Key))*4) || !charge(int64(len(g.Owner))*4) || !charge(int64(len(g.Types))*8) {
			return 0, fmt.Errorf("grant workspace exceeded")
		}
	}
	for _, r := range reservations {
		if len(r.Name) > MaxNameBytes || len(r.Source) > MaxIdentityBytes {
			return 0, fmt.Errorf("reservation string limit exceeded")
		}
		if !charge(int64(len(r.Name))*4) || !charge(int64(len(r.Source))) {
			return 0, fmt.Errorf("reservation workspace exceeded")
		}
	}
	return maxBytes - left, nil
}
