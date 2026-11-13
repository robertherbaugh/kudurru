package k8s

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func object(kind *sourceKind, annotations map[string]any, body map[string]any) *unstructured.Unstructured {
	obj := map[string]any{"apiVersion": kind.apiVersion(), "kind": kind.kind,
		"metadata": map[string]any{"name": "web", "namespace": "apps", "uid": "uid-1", "resourceVersion": "7", "annotations": annotations}}
	for k, v := range body {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func kindOf(label string) *sourceKind {
	for _, k := range sourceKinds {
		if k.label == label {
			return k
		}
	}
	panic(label)
}

func lb(entries ...map[string]any) map[string]any {
	var in []any
	for _, e := range entries {
		in = append(in, e)
	}
	return map[string]any{"loadBalancer": map[string]any{"ingress": in}}
}

func TestStripIngressHostsTargetsAndAnnotations(t *testing.T) {
	k := kindOf("ingress")
	s := strip(k, object(k, map[string]any{
		"external-dns.alpha.kubernetes.io/hostname": "Extra.Example.com., other.example.com",
		"external-dns.kubernetes.io/ttl":            "2m",
		"external-dns.alpha.kubernetes.io/ttl":      "30",
	}, map[string]any{
		"spec": map[string]any{
			"rules": []any{map[string]any{"host": "web.example.com"}, map[string]any{"host": ""}},
			"tls":   []any{map[string]any{"hosts": []any{"secure.example.com"}}},
		},
		"status": lb(map[string]any{"ip": "192.0.2.10"}, map[string]any{"hostname": "lb.cloud.example"}),
	}), nil)
	if s.invalid != nil || s.ttl != 120 {
		t.Fatalf("ttl %d (the current prefix wins), invalid %v", s.ttl, s.invalid)
	}
	want := map[string][]string{
		"web.example.com": {"192.0.2.10", "lb.cloud.example"}, "secure.example.com": {"192.0.2.10", "lb.cloud.example"},
		"extra.example.com": {"192.0.2.10", "lb.cloud.example"}, "other.example.com": {"192.0.2.10", "lb.cloud.example"},
	}
	if !reflect.DeepEqual(s.targets, want) {
		t.Fatalf("targets %v", s.targets)
	}
	// The target annotation replaces the status addresses.
	s = strip(k, object(k, map[string]any{"external-dns.kubernetes.io/target": "198.51.100.1,2001:db8::1"}, map[string]any{
		"spec": map[string]any{"rules": []any{map[string]any{"host": "web.example.com"}}}, "status": lb(map[string]any{"ip": "192.0.2.10"}),
	}), nil)
	if !reflect.DeepEqual(s.targets["web.example.com"], []string{"198.51.100.1", "2001:db8::1"}) {
		t.Fatalf("override %v", s.targets)
	}
	// An unusable TTL makes the whole source invalid.
	for _, ttl := range []string{"soon", "0", "-5", "4294967296"} {
		if s := strip(k, object(k, map[string]any{"external-dns.kubernetes.io/ttl": ttl}, nil), nil); s.invalid == nil {
			t.Errorf("ttl %q accepted", ttl)
		}
	}
}

func TestStripServiceNeedsLoadBalancerAndHostname(t *testing.T) {
	k := kindOf("service")
	annotated := map[string]any{"external-dns.kubernetes.io/hostname": "svc.example.com"}
	for name, c := range map[string]struct {
		annotations map[string]any
		typ         string
		want        int
	}{
		"load balancer with hostname": {annotated, "LoadBalancer", 1},
		"cluster IP with hostname":    {annotated, "ClusterIP", 0},
		"load balancer without name":  {nil, "LoadBalancer", 0},
	} {
		s := strip(k, object(k, c.annotations, map[string]any{"spec": map[string]any{"type": c.typ}, "status": lb(map[string]any{"ip": "192.0.2.20"})}), nil)
		if len(s.targets) != c.want {
			t.Errorf("%s: %v", name, s.targets)
		}
	}
}

func TestStripRouteUsesAcceptingGateways(t *testing.T) {
	k := kindOf("httproute")
	gateways := map[string]gateway{
		"infra/public": {addresses: []string{"192.0.2.30"}, listeners: map[string]string{"https": "*.apps.example.com", "plain": ""}},
		"apps/local":   {addresses: []string{"192.0.2.31"}, target: []string{"edge.example.net"}},
	}
	accepted := func(ns, name, status string) any {
		return map[string]any{"parentRef": map[string]any{"name": name, "namespace": ns}, "conditions": []any{map[string]any{"type": "Accepted", "status": status}}}
	}
	route := func(hostnames []any, parents []any, statuses ...any) *unstructured.Unstructured {
		return object(k, nil, map[string]any{"spec": map[string]any{"hostnames": hostnames, "parentRefs": parents}, "status": map[string]any{"parents": statuses}})
	}
	public := map[string]any{"name": "public", "namespace": "infra"}
	local := map[string]any{"name": "local"}

	s := strip(k, route([]any{"shop.example.com"}, []any{public, local}, accepted("infra", "public", "True"), accepted("apps", "local", "True")), gateways)
	if !reflect.DeepEqual(s.targets, map[string][]string{"shop.example.com": {"192.0.2.30", "edge.example.net"}}) {
		t.Fatalf("two accepting parents: %v", s.targets)
	}
	// A parent that has not accepted the route publishes nothing.
	s = strip(k, route([]any{"shop.example.com"}, []any{public}, accepted("infra", "public", "False")), gateways)
	if len(s.targets) != 0 {
		t.Fatalf("refused route published %v", s.targets)
	}
	// Without hostnames the route takes its listener's (sectionName narrows).
	withSection := map[string]any{"name": "public", "namespace": "infra", "sectionName": "https"}
	s = strip(k, route(nil, []any{withSection}, accepted("infra", "public", "True")), gateways)
	if !reflect.DeepEqual(s.targets, map[string][]string{"*.apps.example.com": {"192.0.2.30"}}) {
		t.Fatalf("listener hostnames: %v", s.targets)
	}
	// A parent of another kind or an unknown Gateway is ignored.
	other := map[string]any{"name": "public", "namespace": "infra", "kind": "Service", "group": ""}
	missing := map[string]any{"name": "gone"}
	s = strip(k, route([]any{"shop.example.com"}, []any{other, missing}, accepted("infra", "public", "True"), accepted("apps", "gone", "True")), gateways)
	if len(s.targets) != 0 {
		t.Fatalf("ignored parents published %v", s.targets)
	}
}

func TestEndpointsFromTargets(t *testing.T) {
	got, err := endpoints("web.example.com", []string{"192.0.2.2", "2001:db8::1", "192.0.2.1", "192.0.2.2", "lb.example.net"}, 60)
	if err != nil {
		t.Fatal(err)
	}
	want := []v1alpha1.Endpoint{
		{DNSName: "web.example.com", RecordType: "A", Targets: []string{"192.0.2.1", "192.0.2.2"}, RecordTTL: 60},
		{DNSName: "web.example.com", RecordType: "AAAA", Targets: []string{"2001:db8::1"}, RecordTTL: 60},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses: %+v", got)
	}
	got, _ = endpoints("web.example.com", []string{"z.example.net", "A.example.net."}, 0)
	if len(got) != 1 || got[0].RecordType != "CNAME" || !reflect.DeepEqual(got[0].Targets, []string{"a.example.net"}) {
		t.Fatalf("host names: %+v", got)
	}
	if _, err := endpoints("web.example.com", nil, 0); err == nil {
		t.Fatal("no targets made endpoints")
	}
}

func TestZoneForLongestUnambiguousApex(t *testing.T) {
	z := func(name, apex string) *v1alpha1.DNSZone {
		return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.DNSZoneSpec{ZoneName: apex}}
	}
	zones := []*v1alpha1.DNSZone{z("outer", "example.com."), z("inner", "apps.example.com."), z("dup-a", "dup.example.com."), z("dup-b", "DUP.example.com")}
	for name, want := range map[string]string{"web.apps.example.com": "inner", "apps.example.com": "inner", "www.example.com": "outer", "x.dup.example.com": "", "example.org": ""} {
		got := zoneFor(name, zones)
		if (got == nil && want != "") || (got != nil && got.Name != want) {
			t.Errorf("%s: %v, want %q", name, got, want)
		}
	}
}
