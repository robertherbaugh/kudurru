package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

// edgeStatusSource uses EndpointSlices rather than cluster DNS: server pods
// normally use dnsPolicy Default. Unready edge pods remain visible.
func edgeStatusSource(restCfg *rest.Config, node string) (func(context.Context) ([]api.EdgeSupervisorEndpoint, error), bool, error) {
	edgeURL, vipURL := os.Getenv("KUDURRU_EDGE_STATUS_URL"), os.Getenv("KUDURRU_VIP_STATUS_URL")
	source := os.Getenv("KUDURRU_EDGE_SUPERVISORS")
	vip, _ := strconv.ParseBool(os.Getenv("KUDURRU_VIP_SUPERVISED"))
	if source == "" {
		if edgeURL == "" && vipURL == "" {
			return nil, false, nil
		}
		return func(context.Context) ([]api.EdgeSupervisorEndpoint, error) {
			return []api.EdgeSupervisorEndpoint{{Node: node, EdgeURL: edgeURL, VIPURL: vipURL}}, nil
		}, vipURL != "", nil
	}
	spec, err := cluster.ParseSourceSpec(source)
	if err != nil || spec.Kind != "endpointslice" {
		return nil, false, fmt.Errorf("KUDURRU_EDGE_SUPERVISORS must be endpointslice:<namespace>/<service>")
	}
	if restCfg == nil {
		restCfg, err = ctrlconfig.GetConfig()
		if err != nil {
			return nil, false, fmt.Errorf("edge status client: %w", err)
		}
	}
	client, err := discoveryv1client.NewForConfig(restCfg)
	if err != nil {
		return nil, false, err
	}
	internalPort := -1
	if vip {
		internalPort = 8085
	}
	src := &cluster.EndpointSliceSource{Client: client, Namespace: spec.Namespace, Service: spec.Service, PortName: "edge-ready", DefaultPort: 8084, InternalPort: internalPort}
	return supervisorEndpoints(src, vip), vip, nil
}

func supervisorEndpoints(src cluster.MembershipSource, vip bool) func(context.Context) ([]api.EdgeSupervisorEndpoint, error) {
	return func(ctx context.Context) ([]api.EdgeSupervisorEndpoint, error) {
		eps, err := src.List(ctx)
		if err != nil {
			return nil, err
		}
		out := []api.EdgeSupervisorEndpoint{}
		seen := map[string]bool{}
		for _, ep := range eps {
			name := ep.Name
			if name == "" {
				name = ep.Address
			}
			// Dual-stack slices may list the same pod twice.
			if seen[name] {
				continue
			}
			seen[name] = true
			item := api.EdgeSupervisorEndpoint{Node: name, EdgeURL: "http://" + ep.Address}
			if vip && ep.InternalAddress != "" {
				item.VIPURL = "http://" + ep.InternalAddress
			}
			out = append(out, item)
		}
		return out, nil
	}
}
