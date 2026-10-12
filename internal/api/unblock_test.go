package api

import (
	"context"
	"net/http"
	"net/netip"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

// unblockHarness seeds the default policy and group and two requests.
func unblockHarness(t *testing.T) (*harness, *unblock.Service, []*v1alpha1.UnblockRequest) {
	t.Helper()
	var svc *unblock.Service
	h := newHarness(t, func(o *Options) {
		svc = unblock.New(unblock.Options{Store: o.Store, Node: "node-a"})
		o.Unblock = svc
	})
	h.seed(store.ManagerBootstrap,
		&v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
		&v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}},
	)
	var rs []*v1alpha1.UnblockRequest
	for _, sub := range []unblock.Submission{
		{Client: netip.MustParseAddr("192.0.2.20"), Domain: "games.example", Reason: "<script>alert(1)</script>", Groups: []string{"default/default"}, Policies: []string{"default/default"}},
		{Client: netip.MustParseAddr("192.0.2.21"), Domain: "shop.example", Groups: []string{"default/default"}},
	} {
		r, err := svc.Submit(context.Background(), sub)
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	return h, svc, rs
}

func TestUnblockRequestsAPI(t *testing.T) {
	h, _, rs := unblockHarness(t)
	base := "/api/v1/unblockrequests"
	path := base + "/default/" + rs[0].Name

	// Requests carry client addresses and free text: not for viewers.
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusForbidden)
	var l UnblockRequestList
	h.do(http.MethodGet, base+"?state=Pending&limit=1", "auditor", nil).want(http.StatusOK).json(&l)
	if l.Counts.Pending != 2 || len(l.Items) != 1 || l.Continue == "" {
		t.Fatalf("list %+v", l)
	}
	var one UnblockRequest
	h.do(http.MethodGet, path, "auditor", nil).want(http.StatusOK).json(&one)
	if one.Domain != "games.example" || one.Client != "192.0.2.20" || one.Reason != "<script>alert(1)</script>" || one.State != "Pending" || one.ExpiresAt == nil {
		t.Fatalf("get %+v", one)
	}
	h.do(http.MethodGet, base+"/default/ur-nope", "auditor", nil).want(http.StatusNotFound)

	// Deciding needs unblockrequests.decide (operator, admin).
	h.do(http.MethodPost, path+"/approve", "auditor", map[string]any{}).want(http.StatusForbidden)
	h.do(http.MethodPost, path+"/approve", "operator", map[string]any{"duration": "soon"}).want(http.StatusBadRequest)
	h.do(http.MethodPost, path+"/approve", "operator", map[string]any{"scope": "client"}).want(http.StatusBadRequest)
	h.do(http.MethodPost, path+"/approve", "operator", map[string]any{"scope": "group", "group": "nobody"}).want(http.StatusUnprocessableEntity)
	var res UnblockDecisionResult
	h.do(http.MethodPost, path+"/approve", "operator", map[string]any{"scope": "everyone", "match": "subdomains", "duration": "1h", "note": "ok"}).want(http.StatusOK).json(&res)
	if res.Request.State != "Approved" || res.Request.Allow == nil || res.Request.Allow.Entry != "*.games.example" || res.Request.Allow.ExpiresAt == nil ||
		res.Request.DecidedBy != "User:operator" || !slices.Equal(res.Attached, []string{"default/default"}) {
		t.Fatalf("approve %+v", res)
	}
	bl := &v1alpha1.BlockList{}
	if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: unblock.EveryoneList}, bl); err != nil || len(bl.Spec.Sources[0].Entries) != 1 || bl.Spec.Sources[0].Entries[0].Value != "*.games.example" || bl.Spec.Sources[0].Entries[0].Origin != v1alpha1.OriginUnblock {
		t.Fatalf("ui-allow %+v %v", bl.Spec, err)
	}
	h.do(http.MethodPost, path+"/approve", "operator", map[string]any{}).want(http.StatusConflict)
	h.do(http.MethodDelete, path, "admin", nil).want(http.StatusConflict)

	// Deny revokes it.
	h.do(http.MethodPost, path+"/deny", "operator", map[string]any{"note": "not on school nights"}).want(http.StatusOK).json(&res)
	if res.Request.State != "Denied" || res.Request.Note != "not on school nights" || len(res.Request.History) != 1 {
		t.Fatalf("deny %+v", res.Request)
	}
	if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: unblock.EveryoneList}, bl); err == nil {
		t.Fatalf("ui-allow still exists after the revocation: %+v", bl.Spec)
	}
	h.do(http.MethodDelete, path, "operator", nil).want(http.StatusNoContent)
	h.do(http.MethodGet, path, "auditor", nil).want(http.StatusNotFound)

	var actions []string
	for _, ev := range h.audit.Events() {
		if ev.Resource.Kind == v1alpha1.KindUnblockRequest && ev.Result == "success" {
			actions = append(actions, ev.Action)
		}
	}
	if want := []string{"unblockrequests.approve", "unblockrequests.deny", "unblockrequests.delete"}; !slices.Equal(actions, want) {
		t.Fatalf("audit %v, want %v", actions, want)
	}
}

// Without Options.Unblock the routes read the store.
func TestUnblockRequestsWithoutIndex(t *testing.T) {
	h := newHarness(t)
	svc := unblock.New(unblock.Options{Store: h.st})
	r, err := svc.Submit(context.Background(), unblock.Submission{Client: netip.MustParseAddr("192.0.2.30"), Domain: "x.example"})
	if err != nil {
		t.Fatal(err)
	}
	var l UnblockRequestList
	h.do(http.MethodGet, "/api/v1/unblockrequests", "admin", nil).want(http.StatusOK).json(&l)
	if len(l.Items) != 1 || l.Items[0].Name != r.Name || l.Counts.Pending != 1 {
		t.Fatalf("list %+v", l)
	}
}
