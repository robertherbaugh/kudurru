package matcher

import (
	"cmp"
	"net/netip"
	"slices"
)

// IPSet is a sorted set of IP addresses for response-side checks: a list
// entry that is a bare address blocks answers that contain it.
type IPSet struct {
	addrs []netip.Addr
	tags  []uint8
}

// Len returns the number of unique addresses.
func (s *IPSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.addrs)
}

// Match reports whether a (IPv4-mapped IPv6 addresses are unmapped, zones
// ignored) is in the set.
func (s *IPSet) Match(a netip.Addr) (tag uint8, ok bool) {
	if s == nil || !a.IsValid() {
		return 0, false
	}
	a = canonicalAddr(a)
	i, found := slices.BinarySearchFunc(s.addrs, a, netip.Addr.Compare)
	if !found {
		return 0, false
	}
	return s.tags[i], true
}

// Bytes returns the approximate heap footprint of the set.
func (s *IPSet) Bytes() int {
	if s == nil {
		return 0
	}
	return 24*len(s.addrs) + len(s.tags)
}

// IPSetBuilder accumulates addresses for an IPSet. The zero value is ready to
// use.
type IPSetBuilder struct {
	addrs []netip.Addr
	tags  []uint8
}

// Add adds an address. It returns false for the zero Addr.
func (b *IPSetBuilder) Add(a netip.Addr, tag uint8) bool {
	if !a.IsValid() {
		return false
	}
	b.addrs = append(b.addrs, canonicalAddr(a))
	b.tags = append(b.tags, tag)
	return true
}

// Added returns the number of successful Add calls, duplicates included.
func (b *IPSetBuilder) Added() int { return len(b.addrs) }

// Build sorts and de-duplicates the addresses (first tag wins) and resets
// the builder. It returns nil when nothing was added.
func (b *IPSetBuilder) Build() *IPSet {
	if len(b.addrs) == 0 {
		return nil
	}
	idx := make([]int, len(b.addrs))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(x, y int) int {
		if c := b.addrs[x].Compare(b.addrs[y]); c != 0 {
			return c
		}
		return cmp.Compare(x, y)
	})
	s := &IPSet{}
	for k, i := range idx {
		if k > 0 && b.addrs[i] == s.addrs[len(s.addrs)-1] {
			continue
		}
		s.addrs = append(s.addrs, b.addrs[i])
		s.tags = append(s.tags, b.tags[i])
	}
	*b = IPSetBuilder{}
	return s
}

func canonicalAddr(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}
