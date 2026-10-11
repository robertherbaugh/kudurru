package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestEndpointSliceResolve(t *testing.T) {
	yes, no := ptr.To(true), ptr.To(false)
	cs := fake.NewClientset(slice("kudurru-dns-a", 5353,
		pod("kudurru-0", "10.1.0.10", yes, yes, no),
		pod("kudurru-1", "10.1.0.11", no, no, no), // a leader that is not ready still resolves
	))
	src := &EndpointSliceSource{Client: cs.DiscoveryV1(), Namespace: "kudurru", Service: "kudurru-dns"}
	dns := func(_ context.Context, id string) (string, error) { return id + ".kudurru-headless:8082", nil }
	ctx := context.Background()

	resolve := EndpointSliceResolve(src, dns)
	for id, want := range map[string]string{
		"kudurru-1": "10.1.0.11:8082",
		"kudurru-9": "kudurru-9.kudurru-headless:8082", // not in the slice: the DNS name
	} {
		if got, err := resolve(ctx, id); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", id, got, err, want)
		}
	}
	if _, err := EndpointSliceResolve(src, nil)(ctx, "kudurru-9"); err == nil {
		t.Error("without a fallback an unknown pod is an error")
	}
}

// TestElectorMembershipCacheTTL: a resolved leader address is re-resolved
// after CacheTTL, so a leader pod restarted under the same name is found
// at its new IP.
func TestElectorMembershipCacheTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	addrs := []string{"10.0.0.1:8082", "10.0.0.2:8082"}
	calls := 0
	m := &ElectorMembership{
		SelfName: "a", Elector: &stubElector{leader: "b"}, CacheTTL: 30 * time.Second, Now: func() time.Time { return now },
		Resolve: func(context.Context, string) (string, error) {
			calls++
			if calls > len(addrs) {
				return "", errors.New("no more")
			}
			return addrs[calls-1], nil
		},
	}
	ctx := context.Background()
	for range 3 {
		if l, err := m.Leader(ctx); err != nil || l.Address != "10.0.0.1:8082" {
			t.Fatalf("cached: %+v %v", l, err)
		}
	}
	now = now.Add(31 * time.Second)
	if l, err := m.Leader(ctx); err != nil || l.Address != "10.0.0.2:8082" || calls != 2 {
		t.Fatalf("after the TTL: %+v %v calls=%d", l, err, calls)
	}
}
