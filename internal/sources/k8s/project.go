package k8s

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// family is what a DNSZone's spec.external enables.
type family int

const (
	familyIngress family = iota
	familyGateway
	familyService
)

// sourceKind is one Kubernetes kind the adapter reads.
type sourceKind struct {
	// label is the zone.ExternalSourceLabel value of derived record sets.
	label  string
	kind   string
	gvr    schema.GroupVersionResource
	family family
}

func (k *sourceKind) apiVersion() string { return k.gvr.GroupVersion().String() }

const gatewayGroup = "gateway.networking.k8s.io"

// sourceKinds are read in this order. Gateway API kinds are read as
// unstructured objects: the repository does not depend on the Gateway API
// Go module, and only a few fields are needed.
var sourceKinds = []*sourceKind{
	{label: "ingress", kind: "Ingress", gvr: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, family: familyIngress},
	{label: "httproute", kind: "HTTPRoute", gvr: schema.GroupVersionResource{Group: gatewayGroup, Version: "v1", Resource: "httproutes"}, family: familyGateway},
	{label: "grpcroute", kind: "GRPCRoute", gvr: schema.GroupVersionResource{Group: gatewayGroup, Version: "v1", Resource: "grpcroutes"}, family: familyGateway},
	{label: "tlsroute", kind: "TLSRoute", gvr: schema.GroupVersionResource{Group: gatewayGroup, Version: "v1alpha2", Resource: "tlsroutes"}, family: familyGateway},
	{label: "service", kind: "Service", gvr: schema.GroupVersionResource{Version: "v1", Resource: "services"}, family: familyService},
}

var gatewayResource = schema.GroupVersionResource{Group: gatewayGroup, Version: "v1", Resource: "gateways"}

// Annotation keys (external-dns compatible). The current prefix wins over
// the legacy alpha prefix when both are set.
const (
	annotationPrefix       = "external-dns.kubernetes.io/"
	legacyAnnotationPrefix = "external-dns.alpha.kubernetes.io/"
)

func annotation(a map[string]string, key string) (string, bool) {
	if v, ok := a[annotationPrefix+key]; ok {
		return v, true
	}
	v, ok := a[legacyAnnotationPrefix+key]
	return v, ok
}

// source is the stripped observation of one source object: its identity
// and the names and targets it publishes. Nothing else of the object is
// retained after a list.
type source struct {
	kind                     *sourceKind
	namespace, name, uid, rv string
	labels                   map[string]string
	deleting                 bool
	ttl                      int64
	targets                  map[string][]string // name -> targets
	invalid                  error
}

// gateway is the stripped observation of one Gateway.
type gateway struct {
	addresses []string
	listeners map[string]string // sectionName -> hostname ("" when unset)
	target    []string          // the target annotation, if set
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".") }

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseTTL accepts external-dns TTL values: whole seconds or a Go duration.
func parseTTL(v string) (int64, error) {
	v = strings.TrimSpace(v)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		d, derr := time.ParseDuration(v)
		if derr != nil {
			return 0, fmt.Errorf("ttl annotation %q is neither seconds nor a duration", v)
		}
		n = int64(d / time.Second)
	}
	if n < 1 || n > 2147483647 {
		return 0, fmt.Errorf("ttl annotation %q is outside 1-2147483647 seconds", v)
	}
	return n, nil
}

func strings2(obj map[string]any, path ...string) []string {
	raw, _, _ := unstructured.NestedSlice(obj, path...)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func maps2(obj map[string]any, path ...string) []map[string]any {
	raw, _, _ := unstructured.NestedSlice(obj, path...)
	var out []map[string]any
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

// loadBalancerTargets reads status.loadBalancer.ingress (Ingress, Service).
func loadBalancerTargets(obj map[string]any) []string {
	var out []string
	for _, in := range maps2(obj, "status", "loadBalancer", "ingress") {
		if ip := str(in, "ip"); ip != "" {
			out = append(out, ip)
		} else if h := str(in, "hostname"); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func stripGateway(u *unstructured.Unstructured) gateway {
	g := gateway{listeners: map[string]string{}}
	for _, a := range maps2(u.Object, "status", "addresses") {
		if v := str(a, "value"); v != "" {
			g.addresses = append(g.addresses, v)
		}
	}
	for _, l := range maps2(u.Object, "spec", "listeners") {
		g.listeners[str(l, "name")] = str(l, "hostname")
	}
	if v, ok := annotation(u.GetAnnotations(), "target"); ok {
		g.target = splitList(v)
	}
	return g
}

// strip projects one source object into the names it publishes and their
// targets. gateways maps namespace/name to the observed Gateways (route
// kinds only).
func strip(k *sourceKind, u *unstructured.Unstructured, gateways map[string]gateway) *source {
	s := &source{kind: k, namespace: u.GetNamespace(), name: u.GetName(), uid: string(u.GetUID()), rv: u.GetResourceVersion(),
		labels: maps.Clone(u.GetLabels()), deleting: u.GetDeletionTimestamp() != nil, targets: map[string][]string{}}
	a := u.GetAnnotations()
	if v, ok := annotation(a, "ttl"); ok {
		ttl, err := parseTTL(v)
		if err != nil {
			s.invalid = err
			return s
		}
		s.ttl = ttl
	}
	var extra []string
	if v, ok := annotation(a, "hostname"); ok {
		extra = splitList(v)
	}
	var override []string
	if v, ok := annotation(a, "target"); ok {
		override = splitList(v)
	}
	add := func(hosts, targets []string) {
		if len(override) > 0 {
			targets = override
		}
		for _, h := range hosts {
			if h = normHost(h); h != "" {
				s.targets[h] = append(s.targets[h], targets...)
			}
		}
	}
	switch k.family {
	case familyIngress:
		hosts := extra
		for _, r := range maps2(u.Object, "spec", "rules") {
			hosts = append(hosts, str(r, "host"))
		}
		for _, t := range maps2(u.Object, "spec", "tls") {
			hosts = append(hosts, strings2(t, "hosts")...)
		}
		add(hosts, loadBalancerTargets(u.Object))
	case familyService:
		// external-dns publishes a Service only when it names its hosts.
		if str(u.Object, "spec", "type") == "LoadBalancer" {
			add(extra, loadBalancerTargets(u.Object))
		}
	case familyGateway:
		hostnames := strings2(u.Object, "spec", "hostnames")
		accepted := acceptedParents(u)
		for _, ref := range maps2(u.Object, "spec", "parentRefs") {
			group, kind := str(ref, "group"), str(ref, "kind")
			if (group != "" && group != gatewayGroup) || (kind != "" && kind != "Gateway") {
				continue
			}
			ns := str(ref, "namespace")
			if ns == "" {
				ns = s.namespace
			}
			key := ns + "/" + str(ref, "name")
			// Only a parent that accepted the route publishes it: a route
			// cannot claim names through a Gateway that refuses it.
			if !accepted[key] {
				continue
			}
			g, ok := gateways[key]
			if !ok {
				continue
			}
			hosts := slices.Clone(hostnames)
			if len(hosts) == 0 {
				section := str(ref, "sectionName")
				for name, h := range g.listeners {
					if h != "" && (section == "" || section == name) {
						hosts = append(hosts, h)
					}
				}
			}
			targets := g.addresses
			if len(g.target) > 0 {
				targets = g.target
			}
			add(append(hosts, extra...), targets)
		}
	}
	return s
}

// acceptedParents returns namespace/name of every Gateway in the route's
// status.parents whose Accepted condition is True.
func acceptedParents(u *unstructured.Unstructured) map[string]bool {
	out := map[string]bool{}
	for _, p := range maps2(u.Object, "status", "parents") {
		ref, _, _ := unstructured.NestedMap(p, "parentRef")
		if ref == nil {
			continue
		}
		if kind := str(ref, "kind"); kind != "" && kind != "Gateway" {
			continue
		}
		ns := str(ref, "namespace")
		if ns == "" {
			ns = u.GetNamespace()
		}
		for _, c := range maps2(p, "conditions") {
			if str(c, "type") == "Accepted" && str(c, "status") == "True" {
				out[ns+"/"+str(ref, "name")] = true
			}
		}
	}
	return out
}

// endpoints turns one name's targets into endpoints: A and AAAA for
// addresses, otherwise one CNAME to the first host name in sorted order
// (a CNAME has exactly one target).
func endpoints(name string, targets []string, ttl int64) ([]v1alpha1.Endpoint, error) {
	var v4, v6, hosts []string
	for _, t := range targets {
		if ip, err := netip.ParseAddr(strings.TrimSpace(t)); err == nil {
			if ip.Unmap().Is4() {
				v4 = append(v4, ip.Unmap().String())
			} else {
				v6 = append(v6, ip.String())
			}
			continue
		}
		if h := normHost(t); h != "" {
			hosts = append(hosts, h)
		}
	}
	var out []v1alpha1.Endpoint
	for _, set := range []struct {
		typ string
		ips []string
	}{{"A", v4}, {"AAAA", v6}} {
		if len(set.ips) > 0 {
			slices.Sort(set.ips)
			out = append(out, v1alpha1.Endpoint{DNSName: name, RecordType: set.typ, Targets: slices.Compact(set.ips), RecordTTL: ttl})
		}
	}
	if len(out) == 0 && len(hosts) > 0 {
		slices.Sort(hosts)
		out = append(out, v1alpha1.Endpoint{DNSName: name, RecordType: "CNAME", Targets: hosts[:1], RecordTTL: ttl})
	}
	if len(out) == 0 {
		return nil, errors.New("no targets")
	}
	return out, nil
}
