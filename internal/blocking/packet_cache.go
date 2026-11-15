package blocking

// ClientIndependent proves that none of the original compiled rules requires
// client address/ID/name or tag input absent from the shared packet cache key.
// It is used once during preparation, never on the DNS query path.
func (s *Snapshot) ClientIndependent() bool {
	if s == nil {
		return false
	}
	for _, list := range s.lists {
		for _, rule := range list.slow {
			if len(rule.clients)+len(rule.tags) != 0 {
				return false
			}
		}
	}
	return true
}
