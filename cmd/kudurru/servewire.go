package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// Serve wiring helpers for the API's leader-side triggers and the leader
// address lookup (M5 burn-down, dev-docs/milestones/m5/burndown-server.md).

// listRefresher backs POST /blocklists/{ns}/{name}/refresh: the leader's
// fetcher downloads the list now; a replica answers 409 naming the leader.
func listRefresher(m cluster.Membership, f *listfetcher.Fetcher) api.ListRefresher {
	return api.RefreshFunc(func(ctx context.Context, namespace, name string) error {
		leader, err := m.Leader(ctx)
		if err != nil || !leader.Self {
			return api.ErrNotLeader(leader.Name)
		}
		err = f.RefreshNow(namespace, name)
		if errors.Is(err, listfetcher.ErrUnknownList) {
			return api.ErrListNotRefreshable(namespace + "/" + name)
		}
		return err
	})
}

// listBodies backs the entries browser of URL and ConfigMap sources with
// this node's list cache.
type listBodies struct{ cache *listfetcher.DiskCache }

func (b listBodies) OpenSourceBody(namespace string, src v1alpha1.ListSource) (io.ReadCloser, api.SourceBody, error) {
	key := listfetcher.SourceKey(namespace, src)
	if key == "" {
		return nil, api.SourceBody{}, fs.ErrNotExist
	}
	f, e, err := b.cache.Open(key)
	if err != nil {
		return nil, api.SourceBody{}, err
	}
	return f, api.SourceBody{Digest: e.Digest, Fetched: e.Fetched, Size: e.Size}, nil
}

// querylogPurger backs DELETE /querylog with the sqlite writer (nil: 501).
func querylogPurger(w *sqlitelog.Writer) api.QuerylogPurger {
	if w == nil {
		return nil
	}
	return w
}

// leaderResolver finds the leader pod's internal address in the headless
// Service's EndpointSlices (pods with dnsPolicy Default cannot resolve
// <pod>.<headless> through the node's resolver), falling back to byName.
// headless is KUDURRU_HEADLESS_SERVICE, "<svc>.<ns>.svc.<domain>" or a bare
// Service name; without it only byName is used.
func leaderResolver(restCfg *rest.Config, headless, port string, byName cluster.ResolveFunc) (cluster.ResolveFunc, cluster.MembershipSource, error) {
	svc, tail, _ := strings.Cut(headless, ".")
	if restCfg == nil || svc == "" {
		return byName, nil, nil
	}
	ns := kube.PodNamespace()
	if n, _, ok := strings.Cut(tail, "."); ok && n != "" {
		ns = n
	}
	client, err := discoveryv1client.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("endpointslice client: %w", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, nil, fmt.Errorf("listeners.internal.address port %q: %w", port, err)
	}
	src := &cluster.EndpointSliceSource{Client: client, Namespace: ns, Service: svc, InternalPort: p}
	return cluster.EndpointSliceResolve(src, byName), src, nil
}
