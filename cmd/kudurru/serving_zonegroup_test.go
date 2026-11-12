package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestServingObjectCommandResolvesZoneGroupInTransaction: on the complete
// SQLite runtime the command transaction states a zone attribute's group from
// its own DNSZone read and discards the caller's value (D385). A forged group
// cannot reach a zone outside the token's zonegroup scope, an omitted group
// does not hide a member zone, and removing a zone's label denies the next
// write through the actual router.
func TestServingObjectCommandResolvesZoneGroupInTransaction(t *testing.T) {
	f := newServingCommandFixture(t)
	id, _ := tokens.Parse(f.token)
	var token internalv1.APIToken
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
		t.Fatal(err)
	}
	token.Spec.Scopes = []string{"zone-admin@zonegroup:prod"}
	if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	zone := func(name, group string) *public.DNSZone {
		z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: name + ".test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + name + ".test."}}}
		if group != "" {
			z.Labels = map[string]string{public.ZoneGroupLabel: group}
		}
		return z
	}
	for _, z := range []*public.DNSZone{zone("member", "prod"), zone("outside", "")} {
		if _, err := f.s.opts.Store.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })

	// Direct command admission with stated groups the transaction must ignore.
	// A refusal before admission (503: the newest generation is not yet
	// published) runs nothing; ask again until the command is decided.
	attempt := func(zoneName, stated string) int {
		t.Helper()
		key := store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "in-" + zoneName}
		deadline := time.Now().Add(10 * time.Second)
		for {
			var result error
			ran := false
			f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ran = true
				result = f.s.ObjectCommand(r.Context(), key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
					attrs := []rbac.Attributes{{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: zoneName}, Namespace: key.Namespace, Name: key.Name, ZoneGroup: stated}}
					if err := authorize(ctx, attrs); err != nil {
						return err
					}
					_, err := tx.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zoneName}, Endpoints: []public.Endpoint{{DNSName: "www", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.1"}}}}}, store.ApplyOptions{Manager: store.ManagerAPI})
					return err
				})
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			// The middleware refuses a request it cannot admit on a current
			// generation without calling the handler: nothing ran.
			status := http.StatusServiceUnavailable
			var e *api.Error
			switch {
			case !ran:
			case result == nil:
				status = http.StatusOK
			case errors.As(currentCommandAPIError(result), &e):
				status = e.Status
			default:
				t.Fatal("command answered an unexpected error:", result)
			}
			if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
				return status
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if status := attempt("outside", "prod"); status != http.StatusForbidden {
		t.Fatal("a stated group reached a zone outside the token's zone group:", status)
	}
	var absent public.DNSRecordSet
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "in-outside"}, &absent); err == nil {
		t.Fatal("refused command committed")
	}
	if status := attempt("member", ""); status != http.StatusOK {
		t.Fatal("transaction did not resolve the member zone's group:", status)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })

	// The actual router: member writes pass, the label removal denies.
	srv := servingObjectAPI(t, f, nil)
	// A request admitted before the newest generation is published is refused
	// with generation_pending before any effect (the command path words the
	// same refusal "current writable serving generation unavailable"); these
	// whole-object PUTs are asked again until they are served.
	send := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			w := servingObjectRequest(t, f, srv, http.MethodPut, path, body, "")
			pending := strings.Contains(w.Body.String(), "generation_pending") || strings.Contains(w.Body.String(), "current writable serving generation unavailable")
			if w.Code != http.StatusServiceUnavailable || !pending || time.Now().After(deadline) {
				return w
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "routed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "member"}, Endpoints: []public.Endpoint{{DNSName: "api", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.2"}}}}}
	if w := send("/api/v1/objects/dnsrecordsets/default/routed", rs); w.Code != http.StatusCreated {
		t.Fatal("member zone write refused", w.Code, w.Body)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	if w := send("/api/v1/objects/dnszones/member", zone("member", "")); w.Code != http.StatusForbidden {
		t.Fatal("a zonegroup token moved its zone out of the group", w.Code, w.Body)
	}
	if _, err := f.s.opts.Store.Apply(t.Context(), zone("member", ""), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.3"}
	if w := send("/api/v1/objects/dnsrecordsets/default/routed", rs); w.Code != http.StatusForbidden {
		t.Fatal("write allowed after the zone left the group", w.Code, w.Body)
	}
	if status := attempt("member", "prod"); status != http.StatusForbidden {
		t.Fatal("a stated group outlived the label removal in the transaction:", status)
	}
}
