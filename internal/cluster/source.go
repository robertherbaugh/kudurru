package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
)

// Endpoint is one kudurru node address reported by a MembershipSource:
// what the dnsdist supervisor renders as a backend (`kudurru edge run
// --backends-from`, docs/architecture.md §3.6 step 1).
type Endpoint struct {
	// Name is the node identity (the pod name in Kubernetes, node.name in
	// compose); empty for an anonymous static address.
	Name string `json:"name,omitempty"`
	// Address is host:port of the node's DNS listener (:5353 behind an
	// edge).
	Address string `json:"address"`
	// InternalAddress is host:port of the node's internal listener
	// (:8082); empty when unknown.
	InternalAddress string `json:"internalAddress,omitempty"`
	// Ready, Serving and Terminating are the EndpointSlice conditions;
	// static endpoints are always ready and serving.
	Ready       bool `json:"ready"`
	Serving     bool `json:"serving"`
	Terminating bool `json:"terminating,omitempty"`
}

// MembershipSource enumerates the kudurru nodes an edge forwards to:
// "static" (compose, flags) and "endpointslice" (Kubernetes) in v0.1;
// "cluster" (follower/raft) from v0.2.
type MembershipSource interface {
	// Name is "static" or "endpointslice:<ns>/<svc>".
	Name() string
	// List returns the current endpoints, sorted by name then address.
	List(ctx context.Context) ([]Endpoint, error)
	// Watch calls fn with the full endpoint list once at start and again
	// after every change until ctx ends; it returns ctx.Err() then. fn is
	// never called concurrently and never with an unchanged list.
	Watch(ctx context.Context, fn func([]Endpoint)) error
}

// Default ports of a kudurru node behind an edge.
const (
	DefaultBackendPort  = 5353
	DefaultInternalPort = 8082
)

// ParseStaticEndpoints parses the --backends value: a comma-separated list
// of [name=]host[:port] entries. Entries without a port get port; every
// entry's internal address is host:internalPort (0 leaves it empty).
func ParseStaticEndpoints(spec string, port, internalPort int) ([]Endpoint, error) {
	var out []Endpoint
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var name string
		if i := strings.IndexByte(raw, '='); i >= 0 {
			name, raw = raw[:i], raw[i+1:]
			if name == "" {
				return nil, fmt.Errorf("cluster: backend %q: empty name before '='", raw)
			}
		}
		host, p, err := net.SplitHostPort(raw)
		if err != nil {
			// No port: the whole entry is a host (IPv6 must be bracketed
			// when a port is given).
			host, p = strings.Trim(raw, "[]"), strconv.Itoa(port)
		}
		if host == "" {
			return nil, fmt.Errorf("cluster: backend %q: empty host", raw)
		}
		if n, err := strconv.Atoi(p); err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("cluster: backend %q: invalid port %q", raw, p)
		}
		ep := Endpoint{Name: name, Address: net.JoinHostPort(host, p), Ready: true, Serving: true}
		if internalPort > 0 {
			ep.InternalAddress = net.JoinHostPort(host, strconv.Itoa(internalPort))
		}
		if seen[ep.Address] {
			return nil, fmt.Errorf("cluster: backend %s listed twice", ep.Address)
		}
		seen[ep.Address] = true
		out = append(out, ep)
	}
	if len(out) == 0 {
		return nil, errors.New("cluster: no backends given")
	}
	sortEndpoints(out)
	return out, nil
}

// StaticSource is a fixed endpoint list.
type StaticSource struct {
	Endpoints []Endpoint
}

// Name implements MembershipSource.
func (s *StaticSource) Name() string { return "static" }

// List implements MembershipSource.
func (s *StaticSource) List(context.Context) ([]Endpoint, error) {
	out := slices.Clone(s.Endpoints)
	sortEndpoints(out)
	return out, nil
}

// Watch implements MembershipSource: one call, then wait for ctx.
func (s *StaticSource) Watch(ctx context.Context, fn func([]Endpoint)) error {
	eps, _ := s.List(ctx)
	fn(eps)
	<-ctx.Done()
	return ctx.Err()
}

// SourceSpec is a parsed --backends-from value.
type SourceSpec struct {
	// Kind is "static" or "endpointslice".
	Kind string
	// Namespace and Service name the headless Service of an
	// endpointslice source.
	Namespace, Service string
}

// ParseSourceSpec parses "static" or "endpointslice:<namespace>/<service>".
func ParseSourceSpec(s string) (SourceSpec, error) {
	switch {
	case s == "" || s == "static":
		return SourceSpec{Kind: "static"}, nil
	case strings.HasPrefix(s, "endpointslice:"):
		ns, svc, ok := strings.Cut(strings.TrimPrefix(s, "endpointslice:"), "/")
		if !ok || ns == "" || svc == "" || strings.Contains(svc, "/") {
			return SourceSpec{}, fmt.Errorf("cluster: %q: want endpointslice:<namespace>/<service>", s)
		}
		return SourceSpec{Kind: "endpointslice", Namespace: ns, Service: svc}, nil
	case s == "cluster":
		return SourceSpec{}, errors.New("cluster: --backends-from cluster needs follower or raft membership (v0.2)")
	}
	return SourceSpec{}, fmt.Errorf("cluster: unknown membership source %q (want static or endpointslice:<ns>/<svc>)", s)
}

// EndpointSliceSource lists kudurru pod addresses from the
// discovery.k8s.io/v1 EndpointSlices of a (headless) Service. The edge's
// ServiceAccount needs get/list/watch on endpointslices in Namespace.
type EndpointSliceSource struct {
	Client    discoveryv1client.EndpointSlicesGetter
	Namespace string
	Service   string
	// PortName selects the slice port carrying DNS; default "dns". When
	// the slice has no such port, DefaultPort is used (default 5353).
	PortName    string
	DefaultPort int
	// InternalPort is the internal listener port of every pod; default
	// 8082, negative leaves InternalAddress empty.
	InternalPort int
	// Backoff is the wait before re-listing after a watch error; default
	// 1 s.
	Backoff time.Duration
}

// Name implements MembershipSource.
func (s *EndpointSliceSource) Name() string {
	return "endpointslice:" + s.Namespace + "/" + s.Service
}

// List implements MembershipSource.
func (s *EndpointSliceSource) List(ctx context.Context) ([]Endpoint, error) {
	eps, _, err := s.list(ctx)
	return eps, err
}

func (s *EndpointSliceSource) selector() string {
	return discoveryv1.LabelServiceName + "=" + s.Service
}

func (s *EndpointSliceSource) list(ctx context.Context) ([]Endpoint, string, error) {
	l, err := s.Client.EndpointSlices(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector()})
	if err != nil {
		return nil, "", fmt.Errorf("cluster: list endpointslices %s/%s: %w", s.Namespace, s.Service, err)
	}
	return s.convert(l.Items), l.ResourceVersion, nil
}

// convert flattens slices into endpoints, de-duplicating addresses that
// appear in two slices during a slice rebalance (the readier copy wins).
func (s *EndpointSliceSource) convert(slices []discoveryv1.EndpointSlice) []Endpoint {
	portName := s.PortName
	if portName == "" {
		portName = "dns"
	}
	def := s.DefaultPort
	if def == 0 {
		def = DefaultBackendPort
	}
	internal := s.InternalPort
	if internal == 0 {
		internal = DefaultInternalPort
	}
	byAddr := map[string]Endpoint{}
	for _, sl := range slices {
		if sl.AddressType == discoveryv1.AddressTypeFQDN {
			continue
		}
		port := def
		for _, p := range sl.Ports {
			if p.Name != nil && *p.Name == portName && p.Port != nil {
				port = int(*p.Port)
				break
			}
		}
		for _, e := range sl.Endpoints {
			if len(e.Addresses) == 0 {
				continue
			}
			host := e.Addresses[0] // the API defines the first address as the one consumers use
			ep := Endpoint{Address: net.JoinHostPort(host, strconv.Itoa(port))}
			if internal > 0 {
				ep.InternalAddress = net.JoinHostPort(host, strconv.Itoa(internal))
			}
			switch {
			case e.TargetRef != nil && e.TargetRef.Kind == "Pod" && e.TargetRef.Name != "":
				ep.Name = e.TargetRef.Name
			case e.Hostname != nil:
				ep.Name = *e.Hostname
			}
			// Nil conditions are "unknown" and the API says to treat
			// them as ready/serving (serving defaults to ready).
			ep.Ready = e.Conditions.Ready == nil || *e.Conditions.Ready
			ep.Serving = ep.Ready
			if e.Conditions.Serving != nil {
				ep.Serving = *e.Conditions.Serving
			}
			ep.Terminating = e.Conditions.Terminating != nil && *e.Conditions.Terminating
			if prev, ok := byAddr[ep.Address]; ok && rank(prev) >= rank(ep) {
				continue
			}
			byAddr[ep.Address] = ep
		}
	}
	out := make([]Endpoint, 0, len(byAddr))
	for _, ep := range byAddr {
		out = append(out, ep)
	}
	sortEndpoints(out)
	return out
}

func rank(e Endpoint) int {
	switch {
	case e.Ready:
		return 2
	case e.Serving:
		return 1
	}
	return 0
}

// Watch implements MembershipSource: list, watch from the list's
// resourceVersion, re-list on every event (slices are small) and after
// watch errors.
func (s *EndpointSliceSource) Watch(ctx context.Context, fn func([]Endpoint)) error {
	backoff := s.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	var last []Endpoint
	first := true
	emit := func(eps []Endpoint) {
		if first || !slices.Equal(eps, last) {
			first = false
			last = eps
			fn(eps)
		}
	}
	for {
		eps, rv, err := s.list(ctx)
		if err == nil {
			emit(eps)
			err = s.watchOnce(ctx, rv, emit)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = err // transient: logged by the caller through the lack of updates; retry
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (s *EndpointSliceSource) watchOnce(ctx context.Context, rv string, emit func([]Endpoint)) error {
	w, err := s.Client.EndpointSlices(s.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: s.selector(), ResourceVersion: rv})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-w.ResultChan():
			if !ok {
				return errors.New("cluster: endpointslice watch closed")
			}
			eps, _, err := s.list(ctx)
			if err != nil {
				return err
			}
			emit(eps)
		}
	}
}

// FilterServing returns the endpoints an edge should render: the ready
// ones, or, when none is ready (a rollout replacing every pod at once, a
// lone replica restarting), the serving ones including terminating pods
// that still answer (§3.6 step 4).
func FilterServing(eps []Endpoint) []Endpoint {
	var ready, serving []Endpoint
	for _, e := range eps {
		switch {
		case e.Ready:
			ready = append(ready, e)
		case e.Serving:
			serving = append(serving, e)
		}
	}
	if len(ready) > 0 {
		return ready
	}
	return serving
}

func sortEndpoints(eps []Endpoint) {
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Name != eps[j].Name {
			return eps[i].Name < eps[j].Name
		}
		return eps[i].Address < eps[j].Address
	})
}
