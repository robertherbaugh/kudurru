package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestParseStaticEndpoints(t *testing.T) {
	tests := []struct {
		spec    string
		want    []Endpoint
		wantErr bool
	}{
		{spec: "10.0.0.11:5353", want: []Endpoint{{Address: "10.0.0.11:5353", InternalAddress: "10.0.0.11:8082", Ready: true, Serving: true}}},
		{spec: "node-b=10.0.0.12, node-a=10.0.0.11:5300", want: []Endpoint{
			{Name: "node-a", Address: "10.0.0.11:5300", InternalAddress: "10.0.0.11:8082", Ready: true, Serving: true},
			{Name: "node-b", Address: "10.0.0.12:5353", InternalAddress: "10.0.0.12:8082", Ready: true, Serving: true},
		}},
		{spec: "fd00::1", want: []Endpoint{{Address: "[fd00::1]:5353", InternalAddress: "[fd00::1]:8082", Ready: true, Serving: true}}},
		{spec: "[fd00::1]:53", want: []Endpoint{{Address: "[fd00::1]:53", InternalAddress: "[fd00::1]:8082", Ready: true, Serving: true}}},
		{spec: "", wantErr: true},
		{spec: "=10.0.0.1", wantErr: true},
		{spec: "10.0.0.1:0", wantErr: true},
		{spec: "10.0.0.1,10.0.0.1:5353", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseStaticEndpoints(tt.spec, DefaultBackendPort, DefaultInternalPort)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%q: err = %v, wantErr %v", tt.spec, err, tt.wantErr)
		}
		if err == nil && !equalEndpoints(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.spec, got, tt.want)
		}
	}
}

func equalEndpoints(a, b []Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseSourceSpec(t *testing.T) {
	for in, want := range map[string]SourceSpec{
		"":                               {Kind: "static"},
		"static":                         {Kind: "static"},
		"endpointslice:kudurru/kudurru-dns": {Kind: "endpointslice", Namespace: "kudurru", Service: "kudurru-dns"},
	} {
		got, err := ParseSourceSpec(in)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v", in, got, err)
		}
	}
	for _, in := range []string{"endpointslice:", "endpointslice:ns", "endpointslice:ns/", "cluster", "dns-sd"} {
		if _, err := ParseSourceSpec(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func slice(name string, port int32, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "kudurru", Labels: map[string]string{discoveryv1.LabelServiceName: "kudurru-dns"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("metrics"), Port: ptr.To[int32](9153)}, {Name: ptr.To("dns"), Port: ptr.To(port)}},
		Endpoints:   eps,
	}
}

func pod(name, ip string, ready, serving, terminating *bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{ip},
		TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: name},
		Conditions: discoveryv1.EndpointConditions{Ready: ready, Serving: serving, Terminating: terminating},
	}
}

func TestEndpointSliceSourceList(t *testing.T) {
	yes, no := ptr.To(true), ptr.To(false)
	other := slice("other", 53, pod("x", "10.9.9.9", yes, yes, no))
	other.Labels[discoveryv1.LabelServiceName] = "other"
	cs := fake.NewClientset(
		slice("kudurru-dns-a", 5353,
			pod("kudurru-0", "10.1.0.10", yes, yes, no),
			pod("kudurru-1", "10.1.0.11", nil, nil, nil), // unknown = ready
			pod("kudurru-2", "10.1.0.12", no, yes, yes),  // terminating, still serving
		),
		slice("kudurru-dns-b", 5353,
			pod("kudurru-1", "10.1.0.11", no, no, no), // duplicate, less ready: ignored
			discoveryv1.Endpoint{Addresses: []string{"10.1.0.13"}, Hostname: ptr.To("kudurru-3")},
		),
		other,
	)
	src := &EndpointSliceSource{Client: cs.DiscoveryV1(), Namespace: "kudurru", Service: "kudurru-dns"}
	if src.Name() != "endpointslice:kudurru/kudurru-dns" {
		t.Fatalf("name %q", src.Name())
	}
	got, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Endpoint{
		{Name: "kudurru-0", Address: "10.1.0.10:5353", InternalAddress: "10.1.0.10:8082", Ready: true, Serving: true},
		{Name: "kudurru-1", Address: "10.1.0.11:5353", InternalAddress: "10.1.0.11:8082", Ready: true, Serving: true},
		{Name: "kudurru-2", Address: "10.1.0.12:5353", InternalAddress: "10.1.0.12:8082", Serving: true, Terminating: true},
		{Name: "kudurru-3", Address: "10.1.0.13:5353", InternalAddress: "10.1.0.13:8082", Ready: true, Serving: true},
	}
	if !equalEndpoints(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if f := FilterServing(got); len(f) != 3 || f[2].Name != "kudurru-3" {
		t.Errorf("FilterServing = %+v", f)
	}
	if f := FilterServing(got[2:3]); len(f) != 1 || f[0].Name != "kudurru-2" {
		t.Errorf("FilterServing with no ready endpoint = %+v, want the serving one", f)
	}
}

func TestEndpointSliceSourceWatch(t *testing.T) {
	yes, no := ptr.To(true), ptr.To(false)
	cs := fake.NewClientset(slice("kudurru-dns-a", 5353, pod("kudurru-0", "10.1.0.10", yes, yes, no)))
	src := &EndpointSliceSource{Client: cs.DiscoveryV1(), Namespace: "kudurru", Service: "kudurru-dns", Backoff: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var calls [][]Endpoint
	done := make(chan error, 1)
	go func() {
		done <- src.Watch(ctx, func(eps []Endpoint) {
			mu.Lock()
			calls = append(calls, eps)
			mu.Unlock()
		})
	}()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(calls) == 1 })
	// The fake watch starts asynchronously; keep updating until seen.
	waitFor(t, func() bool {
		_, _ = cs.DiscoveryV1().EndpointSlices("kudurru").Update(ctx,
			slice("kudurru-dns-a", 5353, pod("kudurru-0", "10.1.0.10", yes, yes, no), pod("kudurru-1", "10.1.0.11", yes, yes, no)),
			metav1.UpdateOptions{})
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 2
	})
	mu.Lock()
	if n := len(calls[len(calls)-1]); n != 2 {
		t.Errorf("last call has %d endpoints, want 2", n)
	}
	for i := 1; i < len(calls); i++ {
		if equalEndpoints(calls[i], calls[i-1]) {
			t.Errorf("call %d repeats an unchanged list", i)
		}
	}
	mu.Unlock()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("Watch returned %v", err)
	}
}

func TestStaticSourceWatch(t *testing.T) {
	src := &StaticSource{Endpoints: []Endpoint{{Address: "b:1"}, {Address: "a:1"}}}
	ctx, cancel := context.WithCancel(context.Background())
	var got []Endpoint
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if err := src.Watch(ctx, func(e []Endpoint) { got = e }); err != context.Canceled {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Address != "a:1" {
		t.Errorf("got %+v", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
