package rbac

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSARVerbAndAttributes(t *testing.T) {
	for _, tc := range []struct {
		a                    Attributes
		res, verb, ns, named string
	}{
		{Attributes{Resource: "zones", Verb: "read"}, "dnszones", "list", "", ""},
		{Attributes{Resource: "zones", Verb: "read", Name: "home"}, "dnszones", "get", "", "home"},
		{Attributes{Resource: "records", Verb: "write", Scope: Scope{Kind: ScopeZone, Name: "home"}}, "dnsrecordsets", "update", "", "home"},
		{Attributes{Resource: "blocklists", Verb: "write", Namespace: "dns"}, "blocklists", "create", "dns", ""},
		{Attributes{Resource: "querylog", Verb: "read"}, "querylogs", "list", "", ""},
		{Attributes{Resource: "blocking", Verb: "toggle"}, "blocking", "toggle", "", ""},
		{Attributes{Resource: "cache", Verb: "flush"}, "caches", "flush", "", ""},
		{Attributes{Resource: "audit", Verb: "export"}, "audits", "export", "", ""},
		{Attributes{Resource: "cluster", Verb: "read"}, "clusters", "list", "", ""},
	} {
		ra := ResourceAttributes(tc.a)
		if ra.Group != SARGroup || ra.Resource != tc.res || ra.Verb != tc.verb || ra.Namespace != tc.ns || ra.Name != tc.named {
			t.Errorf("%+v -> %+v", tc.a, ra)
		}
	}
	p := mustPerms(t)
	for res := range p.Resources {
		if _, ok := SARResources[res]; !ok {
			t.Errorf("permissions.yaml resource %q has no SAR resource name", res)
		}
	}
}

func TestSARAuthorizerCachesAndFailsOpenToBindings(t *testing.T) {
	cs := fake.NewClientset()
	var calls atomic.Int32
	var fail atomic.Bool
	cs.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		calls.Add(1)
		if fail.Load() {
			return true, nil, errors.New("apiserver down")
		}
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		ra := sar.Spec.ResourceAttributes
		allowed := sar.Spec.User == "system:serviceaccount:mon:grafana" && ra.Resource == "stats" && ra.Verb == "list" &&
			len(sar.Spec.Groups) > 0 && sar.Spec.UID == "uid-1"
		sar.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: "test"}
		return true, sar, nil
	})
	now := time.Unix(1_800_000_000, 0)
	az, err := NewSARAuthorizer(SAROptions{Client: cs.AuthorizationV1(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sa := Principal{Kind: KindKubernetesServiceAccount, Name: "system:serviceaccount:mon:grafana", UID: "uid-1", Groups: []string{"system:serviceaccounts"}}
	stats := Attributes{Resource: "stats", Verb: "read"}

	if d, _ := az.Authorize(ctx, Principal{Name: "alice"}, stats); d.Allowed || calls.Load() != 0 {
		t.Fatalf("non-k8s principal: %+v calls=%d", d, calls.Load())
	}
	d, err := az.Authorize(ctx, sa, stats)
	if err != nil || !d.Allowed || d.Path != PathSAR {
		t.Fatalf("allowed: %+v %v", d, err)
	}
	if d, _ := az.Authorize(ctx, sa, stats); !d.Allowed || calls.Load() != 1 {
		t.Fatalf("cached: %+v calls=%d", d, calls.Load())
	}
	if d, _ := az.Authorize(ctx, sa, Attributes{Resource: "settings", Verb: "write"}); d.Allowed || d.Missing == nil {
		t.Fatalf("denied: %+v", d)
	}
	// The allow verdict expires after 2 minutes.
	now = now.Add(DefaultSARAllowTTL + time.Second)
	fail.Store(true)
	d, err = az.Authorize(ctx, sa, stats)
	if err != nil || d.Allowed || d.Reason != "SubjectAccessReview unavailable" {
		t.Fatalf("outage: %+v %v", d, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls %d, want a new review after expiry", calls.Load())
	}
	// Chained with bindings, an outage still lets bindings decide (D22).
	src, err := NewBindingSource(mustPerms(t), []Binding{{Role: "viewer", Scope: Scope{Kind: ScopeCluster},
		Subjects: []Subject{{Kind: SubjectServiceAccount, Name: "grafana", Namespace: "mon"}}}}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(EngineOptions{Bindings: src, SAR: az})
	if err != nil {
		t.Fatal(err)
	}
	sa.ServiceAccount = "mon/grafana"
	if d, _ := e.Authorize(ctx, sa, stats); !d.Allowed || d.Path != PathBinding {
		t.Fatalf("engine during outage: %+v", d)
	}
}
