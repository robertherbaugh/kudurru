package servingbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func convenienceReadHTTP(t *testing.T, f *fixture, publisher *serving.Publisher, objects apihttp.ObjectReadSource, now func() time.Time, mutate ...func(*apihttp.Options)) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	a, err := servinghttp.New(servinghttp.Options{Publisher: publisher, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	bodies, err := NewSourceBodyReads(f.builder.opts.Lists)
	if err != nil {
		t.Fatal(err)
	}
	opts := apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host, Authenticate: a.Middleware, Authorizer: a, CrossOrigin: cop, ObjectReads: objects, CanonicalSettings: policyReadConfig{host: f.deps.Host}, SourceBodyReads: bodies, Now: now, License: license.Unrestricted()}
	for _, modify := range mutate {
		modify(&opts)
	}
	api, err := apihttp.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(api.Handler())
	t.Cleanup(s.Close)
	return s
}

func convenienceDomainSubject(exception bool, marker string) *public.BlockList {
	b := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "ui-allow", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Type: public.EntryExact, Value: "alpha.example", Comment: marker}, {Type: public.EntryExact, Value: "beta.example", Comment: marker}}}}}}
	if exception {
		b.Name, b.Spec.Mode = "external", "block"
		b.Spec.Sources = []public.ListSource{{Name: marker, Format: "domains", Inline: []string{"alpha.example", "beta.example"}, Exclude: []string{"alpha.example", "beta.example"}}}
	}
	return b
}

func convenienceDomainPage(t *testing.T, result objectReadResponse) apihttp.DomainEntries {
	t.Helper()
	body := diagnosticWant(t, result.diagnosticResponse, http.StatusOK)
	var out apihttp.DomainEntries
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Items == nil || out.Lists == nil || out.Unattached == nil {
		t.Fatal("incomplete domain response", body, err)
	}
	return out
}

func TestCapturedConvenienceDomainHTTPHeldCreateEditDeleteRecreate(t *testing.T) {
	for _, exception := range []bool{false, true} {
		for _, change := range []string{"create", "edit", "delete", "recreate"} {
			t.Run(fmt.Sprintf("exception=%t/%s", exception, change), func(t *testing.T) {
				f := newFixture(t)
				list := convenienceDomainSubject(exception, "original")
				key := store.KeyOf(list)
				var old store.Object
				if change != "create" {
					var err error
					old, err = f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem})
					if err != nil {
						t.Fatal(err)
					}
				}
				policy := &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: "default"}, Spec: public.PolicySpec{AllowListRefs: []string{list.Name}}}
				if exception {
					policy.Spec.BlockListRefs, policy.Spec.AllowListRefs = policy.Spec.AllowListRefs, nil
				}
				if _, err := f.store.Apply(t.Context(), policy, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
				f.waitCurrent(t)
				h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 2), resume: make(chan struct{})}
				h.remaining.Store(2)
				server := convenienceReadHTTP(t, f, f.pub, h, nil)
				cleanupHeldObjectReads(t, h)
				id := domainlist.ID("default", public.EntryExact, "alpha.example")
				if exception {
					id = domainlist.ExceptionID("default", list.Name, "original", "alpha.example")
				}
				paths := []string{"domains?action=allow&limit=1", "allowlist/" + id}
				results := make([]chan objectReadResponse, len(paths))
				for i, path := range paths {
					results[i] = make(chan objectReadResponse, 1)
					go func() { results[i] <- objectReadGET(t.Context(), server, f.token, path) }()
				}
				for range paths {
					objectReadWait(t, h)
				}
				if change == "delete" || change == "recreate" {
					if err := f.store.Delete(t.Context(), key, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
						t.Fatal(err)
					}
				}
				if change != "delete" {
					current, err := f.store.Apply(t.Context(), convenienceDomainSubject(exception, "successor"), store.ApplyOptions{Manager: store.ManagerSystem})
					if err != nil {
						t.Fatal(err)
					}
					if change == "recreate" && current.GetUID() == old.GetUID() {
						t.Fatal("fixture did not recreate UID")
					}
				}
				if err := f.store.Delete(t.Context(), store.KeyOf(policy), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
				policy.Name = "successor"
				policy.ResourceVersion, policy.UID = "", ""
				if _, err := f.store.Apply(t.Context(), policy, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
				f.waitCurrent(t)
				close(h.resume)
				held := convenienceDomainPage(t, <-results[0])
				if change == "create" {
					if len(held.Items) != 0 {
						t.Fatal("held empty image observed creation", held)
					}
					diagnosticWant(t, (<-results[1]).diagnosticResponse, http.StatusNotFound)
				} else {
					if len(held.Items) != 1 || held.Continue == "" || !slices.Equal(held.Items[0].Policies, []string{"default/original"}) {
						t.Fatal("held page mixed policy generation", held)
					}
					detail := diagnosticWant(t, (<-results[1]).diagnosticResponse, http.StatusOK)
					if !strings.Contains(detail, "original") || strings.Contains(detail, "successor") {
						t.Fatal("held detail adopted successor", detail)
					}
					diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "domains?action=allow&limit=1&continue="+url.QueryEscape(held.Continue)).diagnosticResponse, http.StatusConflict)
				}
				fresh := convenienceDomainPage(t, objectReadGET(t.Context(), server, f.token, "allowlist"))
				if change == "delete" {
					if len(fresh.Items) != 0 {
						t.Fatal("fresh page retained deleted entries", fresh)
					}
				} else if len(fresh.Items) != 2 || !slices.Equal(fresh.Items[0].Policies, []string{"default/successor"}) {
					t.Fatal("fresh page lost successor", fresh)
				}
			})
		}
	}
}

func convenienceAuditor(t *testing.T, f *fixture) {
	t.Helper()
	id, ok := tokens.Parse(f.token)
	if !ok {
		t.Fatal("invalid fixture token")
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var token internalv1.APIToken
		if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
			return err
		}
		token.Spec.Scopes = []string{"auditor"}
		if _, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "audit", Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "auditor", Subjects: []public.BindingSubject{{Kind: "Group", Name: "staff"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	f.waitCurrent(t)
}

func conveniencePendingExpiry(t *testing.T, f *fixture, duration string) {
	t.Helper()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var s public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &s); err != nil {
			return err
		}
		s.Spec.Settings["unblockRequests"] = extensions.JSON{Raw: []byte(`{"pendingExpiry":"` + duration + `"}`)}
		_, err := tx.Apply(ctx, &s, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
}

// A protected test dependency owner changes entitlement without changing the
// canonical generation. Existing captures keep copied policy and absolute dates.
type convenienceLicenseOwner struct {
	mu      sync.RWMutex
	policy  servingauth.LicensePolicy
	version atomic.Uint64
}

func (o *convenienceLicenseOwner) capture(lower func(context.Context, *replication.Image) (Dependencies, error)) func(context.Context, *replication.Image) (Dependencies, error) {
	return func(ctx context.Context, image *replication.Image) (Dependencies, error) {
		d, err := lower(ctx, image)
		if err != nil {
			return d, err
		}
		o.mu.RLock()
		d.Auth.License = o.policy
		version := o.version.Load()
		o.mu.RUnlock()
		current := d.Current
		d.Current = func() bool { return current() && o.version.Load() == version }
		d.Identity = fmt.Sprintf("%s/license/%d", d.Identity, version)
		return d, nil
	}
}
func (o *convenienceLicenseOwner) replace(policy servingauth.LicensePolicy) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.policy = policy
	o.version.Add(1)
}

func convenienceLicensedPublisher(t *testing.T, f *fixture, policy servingauth.LicensePolicy) (*serving.Publisher, *convenienceLicenseOwner) {
	t.Helper()
	o := &convenienceLicenseOwner{policy: policy}
	o.version.Store(1)
	opts := f.builder.opts
	opts.Capture = o.capture(opts.Capture)
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return catalogReadStartPublisher(t, f.store, b.Prepare), o
}

func convenienceWaitLicense(t *testing.T, p *serving.Publisher, now time.Time, want bool) serving.Identity {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := p.Subscribe()
		lease, err := p.CaptureHTTP(t.Context())
		if err == nil {
			value, ok := lease.Bundle().Component("auth")
			if !ok {
				t.Fatal("missing auth")
			}
			allowed, err := value.(*servingauth.View).UnblockRequestsAllowed(now)
			id := lease.Bundle().Identity()
			lease.Release()
			if err == nil && allowed == want {
				return id
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("license publication stayed pending")
		}
	}
}

func convenienceUnblockSubject(marker string, at time.Time) *public.UnblockRequest {
	return &public.UnblockRequest{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "default"}, Spec: public.UnblockRequestSpec{Domain: marker + ".example", RequestedAt: &metav1.Time{Time: at}, Reason: marker}}
}

func TestCapturedConvenienceUnblockHTTPHeldCreateEditDeleteRecreate(t *testing.T) {
	for _, change := range []string{"create", "edit", "delete", "recreate"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			convenienceAuditor(t, f)
			policy := servingauth.LicensePolicy{RBAC: true, Requests: true, RequestsFrom: f.now.Add(-time.Hour), RequestsUntil: f.now.Add(time.Hour)}
			p, _ := convenienceLicensedPublisher(t, f, policy)
			conveniencePendingExpiry(t, f, "2h")
			request := convenienceUnblockSubject("original", f.now.Truncate(time.Second))
			var old store.Object
			if change != "create" {
				var err error
				old, err = f.store.Apply(t.Context(), request, store.ApplyOptions{Manager: store.ManagerSystem})
				if err != nil {
					t.Fatal(err)
				}
			}
			convenienceWaitLicense(t, p, f.now, true)
			h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 2), resume: make(chan struct{})}
			h.remaining.Store(2)
			server := convenienceReadHTTP(t, f, p, h, nil, func(o *apihttp.Options) {
				clone := *o.Config
				o.Config = &clone
				o.Config.UnblockRequests.PendingExpiry = config.Duration(72 * time.Hour)
			})
			cleanupHeldObjectReads(t, h)
			paths := []string{"unblockrequests", "unblockrequests/default/subject"}
			results := make([]chan objectReadResponse, 2)
			for i, path := range paths {
				results[i] = make(chan objectReadResponse, 1)
				go func() { results[i] <- objectReadGET(t.Context(), server, f.token, path) }()
			}
			for range paths {
				objectReadWait(t, h)
			}
			if change == "delete" || change == "recreate" {
				if err := f.store.Delete(t.Context(), store.KeyOf(request), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
			}
			if change != "delete" {
				current, err := f.store.Apply(t.Context(), convenienceUnblockSubject("successor", f.now), store.ApplyOptions{Manager: store.ManagerSystem})
				if err != nil {
					t.Fatal(err)
				}
				if change == "recreate" && current.GetUID() == old.GetUID() {
					t.Fatal("fixture did not recreate UID")
				}
			}
			conveniencePendingExpiry(t, f, "4h")
			convenienceWaitLicense(t, p, f.now, true)
			close(h.resume)
			for i := range paths {
				result := <-results[i]
				if change == "create" && i == 1 {
					diagnosticWant(t, result.diagnosticResponse, http.StatusNotFound)
					continue
				}
				body := diagnosticWant(t, result.diagnosticResponse, http.StatusOK)
				if strings.Contains(body, "successor") {
					t.Fatal("held request adopted successor", body)
				}
				if change == "create" {
					if !strings.Contains(body, `"pending":0`) {
						t.Fatal("held count used live creation", body)
					}
					continue
				}
				var row apihttp.UnblockRequest
				if i == 0 {
					var list apihttp.UnblockRequestList
					if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Items) != 1 || list.Counts.Pending != 1 {
						t.Fatal("held list", list, err)
					}
					row = list.Items[0]
				} else if err := json.Unmarshal([]byte(body), &row); err != nil {
					t.Fatal(err)
				}
				if row.Domain != "original.example" || row.ResourceVersion != old.GetResourceVersion() || row.ExpiresAt == nil || !row.ExpiresAt.Equal(request.Spec.RequestedAt.Add(2*time.Hour)) {
					t.Fatal("held row mixed settings or object generation", row)
				}
			}
			fresh := objectReadGET(t.Context(), server, f.token, paths[1])
			if change == "delete" {
				diagnosticWant(t, fresh.diagnosticResponse, http.StatusNotFound)
			} else {
				body := diagnosticWant(t, fresh.diagnosticResponse, http.StatusOK)
				if !strings.Contains(body, "successor") {
					t.Fatal("fresh request retained old object", body)
				}
			}
		})
	}
}

func TestCapturedConvenienceUnblockHTTPOriginalFiniteLicenseRenewal(t *testing.T) {
	for _, observation := range []string{"before-from", "at-from", "before-until", "at-until", "after-until"} {
		t.Run(observation, func(t *testing.T) {
			f := newFixture(t)
			convenienceAuditor(t, f)
			from, until := f.now.Add(-time.Minute), f.now.Add(time.Minute)
			policy := servingauth.LicensePolicy{RBAC: true, Requests: true, RequestsFrom: from, RequestsUntil: until}
			p, owner := convenienceLicensedPublisher(t, f, policy)
			if _, err := f.store.Apply(t.Context(), convenienceUnblockSubject("original", f.now), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			originalID := convenienceWaitLicense(t, p, f.now, true)
			var clock atomic.Int64
			clock.Store(from.UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 3), resume: make(chan struct{})}
			h.remaining.Store(3)
			server := convenienceReadHTTP(t, f, p, h, now)
			cleanupHeldObjectReads(t, h)
			paths := []string{"unblockrequests", "unblockrequests/default/subject", "unblockrequests/default/missing"}
			done := make([]chan objectReadResponse, len(paths))
			for i, path := range paths {
				done[i] = make(chan objectReadResponse, 1)
				go func() { done[i] <- objectReadGET(t.Context(), server, f.token, path) }()
			}
			for range paths {
				objectReadWait(t, h)
			}
			policy.RequestsFrom, policy.RequestsUntil = from.Add(-time.Hour), until.Add(time.Hour)
			owner.replace(policy)
			p.Wake()
			successor := convenienceWaitLicense(t, p, until, true)
			if !originalID.Base.Same(successor.Base) || originalID == successor {
				t.Fatal("renewal did not publish same-generation successor")
			}
			observed, want := from, http.StatusOK
			switch observation {
			case "before-from":
				observed, want = from.Add(-time.Nanosecond), http.StatusPaymentRequired
			case "before-until":
				observed = until.Add(-time.Nanosecond)
			case "at-until":
				observed, want = until, http.StatusPaymentRequired
			case "after-until":
				observed, want = until.Add(time.Second), http.StatusPaymentRequired
			}
			clock.Store(observed.UnixNano())
			close(h.resume)
			for i, path := range paths {
				heldStatus, freshStatus := want, http.StatusOK
				if strings.HasSuffix(path, "/missing") {
					freshStatus = http.StatusNotFound
					if heldStatus == http.StatusOK {
						heldStatus = http.StatusNotFound
					}
				}
				diagnosticWant(t, (<-done[i]).diagnosticResponse, heldStatus)
				diagnosticWant(t, objectReadGET(t.Context(), server, f.token, path).diagnosticResponse, freshStatus)
			}
		})
	}
}

func TestCapturedConvenienceHTTPPaginationAndMissingCapabilities(t *testing.T) {
	f := newFixture(t)
	convenienceAuditor(t, f)
	policy := servingauth.LicensePolicy{RBAC: true, Requests: true, RequestsFrom: f.now.Add(-time.Hour), RequestsUntil: f.now.Add(time.Hour)}
	p, owner := convenienceLicensedPublisher(t, f, policy)
	for _, name := range []string{"a", "b"} {
		r := convenienceUnblockSubject(name, f.now)
		r.Name = name
		if _, err := f.store.Apply(t.Context(), r, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Apply(t.Context(), convenienceDomainSubject(false, "original"), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	originalID := convenienceWaitLicense(t, p, f.now, true)
	server := convenienceReadHTTP(t, f, p, NewObjectReads(), nil)
	var page apihttp.UnblockRequestList
	body := diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "unblockrequests?state=Pending&limit=1").diagnosticResponse, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &page); err != nil || page.Continue == "" || page.Counts.Pending != 2 || len(page.Items) != 1 {
		t.Fatal("initial captured page", page, err)
	}
	for _, query := range []string{"state=Denied&limit=1", "state=Pending&limit=2"} {
		diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "unblockrequests?"+query+"&continue="+url.QueryEscape(page.Continue)).diagnosticResponse, http.StatusConflict)
	}
	var next apihttp.UnblockRequestList
	body = diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "unblockrequests?state=Pending&limit=1&continue="+url.QueryEscape(page.Continue)).diagnosticResponse, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &next); err != nil || len(next.Items) != 1 || next.Items[0].Name == page.Items[0].Name || next.Continue != "" {
		t.Fatal("pagination skipped or duplicated rows", next, err)
	}
	// A renewal replaces the whole publication at the same canonical base.
	// Both domain and request pagination must refuse its predecessor's cursor.
	domains := convenienceDomainPage(t, objectReadGET(t.Context(), server, f.token, "domains?limit=1"))
	policy.RequestsUntil = policy.RequestsUntil.Add(time.Hour)
	owner.replace(policy)
	p.Wake()
	current := convenienceWaitLicense(t, p, f.now.Add(90*time.Minute), true)
	if !current.Base.Same(originalID.Base) || current == originalID {
		t.Fatal("dependency-only replacement missing")
	}
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "unblockrequests?state=Pending&limit=1&continue="+url.QueryEscape(page.Continue)).diagnosticResponse, http.StatusConflict)
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "domains?limit=1&continue="+url.QueryEscape(domains.Continue)).diagnosticResponse, http.StatusConflict)
	for _, fault := range []string{"objects", "settings", "identity"} {
		t.Run(fault, func(t *testing.T) {
			s := convenienceReadHTTP(t, f, p, NewObjectReads(), nil, func(o *apihttp.Options) {
				switch fault {
				case "objects":
					o.ObjectReads = nil
				case "settings":
					o.CanonicalSettings = nil
				case "identity":
					o.ObjectReads = objectOnlyConvenienceReads{NewObjectReads()}
				}
			})
			for _, path := range []string{"unblockrequests", "unblockrequests/default/a"} {
				diagnosticWant(t, objectReadGET(t.Context(), s, f.token, path).diagnosticResponse, http.StatusServiceUnavailable)
			}
			if fault != "settings" {
				diagnosticWant(t, objectReadGET(t.Context(), s, f.token, "domains").diagnosticResponse, http.StatusServiceUnavailable)
			}
		})
	}
}

type objectOnlyConvenienceReads struct{ apihttp.ObjectReadSource }
