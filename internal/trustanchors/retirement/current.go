package retirement

// MatchesInstalled observes one immutable gate state without taking exclusion.
// It checks only the exact installed managed mark, domain, floor and closure;
// it neither requires a published Snapshot nor rejects an active writer/plan.
// True grants no source, publication, durable, cache-insertion or output
// authority, and cannot protect a later use. No registration is exposed.
func (g *Gate) MatchesInstalled(expected Mark) bool {
	if g == nil || !validMark(expected) {
		return false
	}
	state := g.state.Load()
	return state != nil && !state.closed && state.installed == expected && state.domain == expected.DomainID && state.floor < expected.EnvelopeGeneration
}
