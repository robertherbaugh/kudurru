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

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func TestSARVerbAndAttributes(t *testing.T) {
	for _, tc := range []struct {
		a                    rbac.Attributes
		res, verb, ns, named string
	}{
		{rbac.Attributes{Resource: "zones", Verb: "read"}, "dnszones", "list", "", ""},
		{rbac.Attributes{Resource: "zones", Verb: "read", Name: "home"}, "dnszones", "get", "", "home"},
		{rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}, "dnsrecordsets", "update", "", "home"},
		{rbac.Attributes{Resource: "blocklists", Verb: "write", Namespace: "dns"}, "blocklists", "create", "dns", ""},
		{rbac.Attributes{Resource: "querylog", Verb: "read"}, "querylogs", "list", "", ""},
		{rbac.Attributes{Resource: "blocking", Verb: "toggle"}, "blocking", "toggle", "", ""},
		{rbac.Attributes{Resource: "cache", Verb: "flush"}, "caches", "flush", "", ""},
		{rbac.Attributes{Resource: "audit", Verb: "export"}, "audits", "export", "", ""},
		{rbac.Attributes{Resource: "cluster", Verb: "read"}, "clusters", "list", "", ""},
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
	sa := rbac.Principal{Kind: KindKubernetesServiceAccount, Name: "system:serviceaccount:mon:grafana", UID: "uid-1", Groups: []string{"system:serviceaccounts"}}
	stats := rbac.Attributes{Resource: "stats", Verb: "read"}

	if d, _ := az.Authorize(ctx, rbac.Principal{Name: "alice"}, stats); d.Allowed || calls.Load() != 0 {
		t.Fatalf("non-k8s principal: %+v calls=%d", d, calls.Load())
	}
	d, err := az.Authorize(ctx, sa, stats)
	if err != nil || !d.Allowed || d.Path != rbac.PathSAR {
		t.Fatalf("allowed: %+v %v", d, err)
	}
	if d, _ := az.Authorize(ctx, sa, stats); !d.Allowed || calls.Load() != 1 {
		t.Fatalf("cached: %+v calls=%d", d, calls.Load())
	}
	if d, _ := az.Authorize(ctx, sa, rbac.Attributes{Resource: "settings", Verb: "write"}); d.Allowed || d.Missing == nil {
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
	src, err := NewBindingSource(mustPerms(t), []rbac.Binding{{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster},
		Subjects: []rbac.Subject{{Kind: rbac.SubjectServiceAccount, Name: "grafana", Namespace: "mon"}}}}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e, err := rbac.NewEngine(rbac.EngineOptions{Permissions: src.Permissions(), Bindings: src, SAR: az})
	if err != nil {
		t.Fatal(err)
	}
	sa.ServiceAccount = "mon/grafana"
	if d, _ := e.Authorize(ctx, sa, stats); !d.Allowed || d.Path != rbac.PathBinding {
		t.Fatalf("engine during outage: %+v", d)
	}
}
