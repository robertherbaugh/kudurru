package dnsdist

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

// ManagedBypass observes one actual retained policy and acknowledged running
// child/source cut. It does no network or disk I/O and never manufactures an
// acknowledgment from the desired configuration or the durable file alone.
// Only the authenticated internal server should expose this remote observation;
// /status remains diagnostic. Backend activation still needs its own original
// attempt, expected topology and local admission boundary.
func (s *Supervisor) ManagedBypass(ctx context.Context, deployment string) (internalapi.EdgeBypassState, error) {
	if err := ctx.Err(); err != nil {
		return internalapi.EdgeBypassState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.managedBypassInstalledLocked() || s.configState.bypassID != deployment ||
		s.configState.runID == "" || len(s.endpoints) == 0 || len(s.endpoints) > internalapi.MaxEdgeBypassBackends {
		return internalapi.EdgeBypassState{}, internalapi.ErrEdgeBypassUnavailable
	}
	out := internalapi.EdgeBypassState{
		Deployment: s.configState.bypassID, Run: s.configState.runID,
		SourceEpoch: s.cacheBypassFence.source, ChildEpoch: s.cacheBypassFence.child,
		Backends: make([]internalapi.EdgeBypassBackend, 0, len(s.endpoints)),
	}
	for _, endpoint := range s.endpoints {
		out.Backends = append(out.Backends, internalapi.EdgeBypassBackend{
			Name: endpoint.Name, Address: endpoint.Address, InternalAddress: endpoint.InternalAddress,
		})
	}
	if err := ctx.Err(); err != nil {
		return internalapi.EdgeBypassState{}, err
	}
	return out, nil
}
