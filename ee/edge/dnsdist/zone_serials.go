package dnsdist

// zoneSerialHistory retains successful backend observations across poll cycles.
// Failed/stale polls never replace it. An empty initial history is expected.
type zoneSerialHistory map[string]map[string]uint32

func (h zoneSerialHistory) retain(live, unsupported map[string]bool) {
	for address := range h {
		if !live[address] {
			delete(h, address)
			delete(unsupported, address)
		}
	}
}

// observe compares complete successful responses, then replaces that backend's
// history. The first response establishes a baseline; only later serial changes
// request a flush. A missing zone is not fabricated as serial zero.
func (h zoneSerialHistory) observe(address string, next map[string]uint32) []string {
	var changed []string
	previous := h[address]
	for name, serial := range next {
		if old, ok := previous[name]; ok && old != serial {
			changed = append(changed, name)
		}
	}
	h[address] = next
	return changed
}
