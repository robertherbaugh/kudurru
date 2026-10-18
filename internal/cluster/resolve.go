package cluster

import (
	"context"
	"fmt"
)

// EndpointSliceResolve returns a ResolveFunc that finds a node identity
// (the leader pod's name) in the EndpointSlices of src (the headless
// Service) and returns that pod's internal address, IP:port. A pod with
// dnsPolicy Default cannot resolve <pod>.<headless-service> through the
// node's resolver, so the EndpointSlice is the source of truth in
// Kubernetes; TLS still verifies the internal certificate name, never the
// address. When the slice has no endpoint for the identity, or the list
// fails, fallback (the DNS name) is used if set. Endpoints are matched
// whatever their readiness: a leader that is not ready still serves
// /internal.
func EndpointSliceResolve(src *EndpointSliceSource, fallback ResolveFunc) ResolveFunc {
	return func(ctx context.Context, identity string) (string, error) {
		eps, err := src.List(ctx)
		if err == nil {
			for _, e := range eps {
				if e.Name == identity && e.InternalAddress != "" {
					return e.InternalAddress, nil
				}
			}
		}
		if fallback != nil {
			return fallback(ctx, identity)
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("cluster: %s has no endpoint for %q", src.Name(), identity)
	}
}
