package policy

import "slices"

// ClientIndependent proves that every original client selects the same group
// set. It deliberately rejects narrower selectors even when their current
// policies happen to have equivalent answers. No runtime identity is sampled.
func (ix *Index) ClientIndependent() bool {
	if ix == nil || ix.def == nil || len(ix.ip)+len(ix.mac)+len(ix.macGrants)+len(ix.v4.m)+len(ix.v6.m)+len(ix.hostname)+len(ix.globs)+len(ix.clientID) != 0 {
		return false
	}
	for _, catch := range []*Membership{ix.catch4, ix.catch6} {
		if catch != nil && !slices.Equal(catch.GroupIDs, ix.def.GroupIDs) {
			return false
		}
	}
	return true
}
