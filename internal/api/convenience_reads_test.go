package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/unblock"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type convenienceReadFixture struct {
	requests []*public.UnblockRequest
	*objectReadFixture
	publication uint64
	dependency  string
	identityErr error
	entitlement func(time.Time) (bool, error)
}

func (f *convenienceReadFixture) ObjectReadIdentity(context.Context) (serving.Identity, error) {
	return serving.Identity{Base: f.base, Publication: f.publication, Dependencies: f.dependency}, f.identityErr
}
func (f *convenienceReadFixture) UnblockRequestsAllowed(_ context.Context, now time.Time) (bool, error) {
	return f.entitlement(now)
}

func (f *convenienceReadFixture) UnblockSnapshot(context.Context) ([]*public.UnblockRequest, replication.Generation, error) {
	out := make([]*public.UnblockRequest, len(f.requests))
	for i, r := range f.requests {
		out[i] = r.DeepCopy()
	}
	return out, f.base, f.err
}

func newConvenienceReadFixture(t *testing.T, objects ...store.Object) (*harness, *convenienceReadFixture) {
	t.Helper()
	h, original := newObjectReadFixture(t)
	f := &convenienceReadFixture{objectReadFixture: original, publication: 7, dependency: "original-dependency", entitlement: func(time.Time) (bool, error) { return true, nil }}
	h.srv.opts.ObjectReads = f
	prepareConvenienceReadFixture(t, f, objects...)
	h.srv.opts.CanonicalSettings = policyReadSettings{cfg: testConfig(t)}
	return h, f
}

func prepareConvenienceReadFixture(t *testing.T, f *convenienceReadFixture, objects ...store.Object) {
	t.Helper()
	compiled, err := blocking.Compile(blocking.Spec{Name: "default/captured-block", Mode: blocking.ModeBlock, Sources: []blocking.Source{blocking.LinesSource("original", parser.FormatDomains, []string{"alpha.example"})}}, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := blocking.Compile(blocking.Spec{Name: "default/captured-allow", Mode: blocking.ModeAllow, Sources: []blocking.Source{blocking.LinesSource("original", parser.FormatDomains, []string{"alpha.example"})}}, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lists, err := blocking.NewSnapshot(1, compiled, allowed)
	if err != nil {
		t.Fatal(err)
	}
	var dns []store.Object
	f.requests = nil
	for _, o := range objects {
		if r, ok := o.(*public.UnblockRequest); ok {
			f.requests = append(f.requests, r.DeepCopy())
		} else {
			dns = append(dns, o)
		}
	}
	snap, err := zonecache.Prepare(zonecache.PrepareOptions{Lists: lists}, dns, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.objects = snap.Objects
}

func convenienceList(name string) *public.BlockList {
	return &public.BlockList{ObjectMeta: objectMeta("default", name), Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Type: public.EntryExact, Value: "alpha.example", Comment: "original"}, {Type: public.EntryExact, Value: "beta.example"}}}}}}
}

func TestCapturedDomainReadsCanonicalProjectionAndCursor(t *testing.T) {
	list := convenienceList("ui-allow-kids")
	p := &public.Policy{ObjectMeta: objectMeta("default", "original"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids"}, AllowListRefs: []string{list.Name}}}
	managed := &public.Policy{ObjectMeta: objectMeta("default", "managed"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids"}}}
	store.SetManagedBy(managed, "file:original.yaml")
	h, f := newConvenienceReadFixture(t, list, p, managed)
	h.seed(store.ManagerAPI, convenienceList("ui-allow"))
	var page DomainEntries
	h.do(http.MethodGet, "/api/v1/domains?action=allow&scope=kids&limit=1", "viewer", nil).want(http.StatusOK).json(&page)
	if page.Total != 2 || page.Matched != 2 || len(page.Items) != 1 || page.Continue == "" || len(page.Lists) != 1 || len(page.Unattached) != 1 || page.Unattached[0].Policy != "default/managed" {
		t.Fatal("projection changed or read live lists", page)
	}
	e := page.Items[0]
	if e.Comment != "original" || e.Everyone || !slices.Equal(e.Groups, []string{"default/kids"}) || !slices.Equal(e.Policies, []string{"default/original"}) || !slices.Equal(e.Overrides, []DomainOverride{{List: "default/captured-block", Rule: "alpha.example"}}) {
		t.Fatal("domain view mixed generations", e)
	}
	var detail DomainEntry
	h.do(http.MethodGet, "/api/v1/allowlist/"+e.ID, "viewer", nil).want(http.StatusOK).json(&detail)
	if detail.Comment != e.Comment || !slices.Equal(detail.Overrides, e.Overrides) {
		t.Fatal("detail projection differs", detail)
	}
	base := "/api/v1/domains?action=allow&scope=kids&limit=1&continue=" + url.QueryEscape(page.Continue)
	var second DomainEntries
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusOK).json(&second)
	if len(second.Items) != 1 || second.Items[0].Value != "beta.example" || second.Continue != "" || second.Total != 2 {
		t.Fatal("incorrect next page", second)
	}
	for _, query := range []string{"action=deny&scope=kids&limit=1", "action=allow&scope=everyone&limit=1", "action=allow&scope=kids&limit=2", "action=allow&scope=kids&limit=1&q=alpha", "action=allow&scope=kids&limit=1&namespace=default", "action=allow&scope=kids&limit=1&kind=entry", "action=allow&scope=kids&limit=1&type=exact", "action=allow&scope=kids&limit=1&enabled=true"} {
		h.do(http.MethodGet, "/api/v1/domains?"+query+"&continue="+url.QueryEscape(page.Continue), "viewer", nil).want(http.StatusConflict)
	}
	f.base.Revision++
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusConflict)
	f.base.Revision--
	f.publication++
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusConflict)
	f.publication--
	f.dependency = "replacement"
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusConflict)
	for _, raw := range []string{"bad!", encodeCursor("1"), strings.Repeat("a", maxObjectReadCursor+1)} {
		h.do(http.MethodGet, "/api/v1/allowlist?continue="+raw, "viewer", nil).want(http.StatusBadRequest)
	}
}

func TestCapturedExceptionIdentityPoliciesAndExactBody(t *testing.T) {
	bl := &public.BlockList{ObjectMeta: objectMeta("default", "external"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Format: "domains", Inline: []string{"alpha.example"}, Exclude: []string{"alpha.example"}}}}}
	store.SetManagedBy(bl, "file:original.yaml")
	p := &public.Policy{ObjectMeta: objectMeta("default", "original"), Spec: public.PolicySpec{BlockListRefs: []string{bl.Name}}}
	h, f := newConvenienceReadFixture(t, bl, p)
	path := "/api/v1/allowlist/" + domainlist.ExceptionID("default", bl.Name, "0", "alpha.example")
	var e DomainEntry
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&e)
	if !e.Enabled || e.Lists[0].Source != "original" || !e.ReadOnly || !slices.Equal(e.Policies, []string{"default/original"}) || len(e.StillMatchedBy) != 1 || e.StillMatchedBy[0].List != "default/captured-block" {
		t.Fatal("active exception identity mixed", e)
	}
	bl.Spec.Sources[0].Exclude = nil
	prepareConvenienceReadFixture(t, f, bl, p)
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(fresh(&e))
	if e.Enabled || !e.ReadOnly || !slices.Equal(e.Policies, []string{"default/original"}) {
		t.Fatal("inactive exception lost original policies/owner", e)
	}
	body := "alpha.example\n"
	bl.Spec.Sources[0].Inline = nil
	bl.Spec.Sources[0].URL = "https://original.example/list"
	bl.Status.Sources = []public.SourceStatus{{Name: "original", Digest: sourceTestDigest(body)}}
	prepareConvenienceReadFixture(t, f, bl, p)
	var liveOpens atomic.Int64
	h.srv.opts.ListBodies = countingBodies{opens: &liveOpens}
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusServiceUnavailable)
	bodies := &sourceBodyFixture{open: func(context.Context, string, string, string) (io.ReadCloser, string, int64, error) {
		return io.NopCloser(strings.NewReader(body)), sourceTestDigest(body), int64(len(body)), nil
	}}
	h.srv.opts.SourceBodyReads = bodies
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK)
	if liveOpens.Load() != 0 {
		t.Fatal("opened live body")
	}
	if bodies.calls != 1 {
		t.Fatal("did not open original receipt")
	}
	h.do(http.MethodGet, "/api/v1/allowlist/"+domainlist.ExceptionID("default", bl.Name, "original", "absent.example"), "viewer", nil).want(http.StatusNotFound)
	bl.Spec.Sources[0].Name = "replacement"
	prepareConvenienceReadFixture(t, f, bl, p)
	h.do(http.MethodGet, "/api/v1/allowlist/"+domainlist.ExceptionID("default", bl.Name, "original", "alpha.example"), "viewer", nil).want(http.StatusNotFound)
}

func convenienceRequest(name string, state public.UnblockRequestState) *public.UnblockRequest {
	return &public.UnblockRequest{ObjectMeta: objectMeta("default", name), Spec: public.UnblockRequestSpec{Domain: name + ".example", RequestedAt: &metav1.Time{Time: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)}}, Status: public.UnblockRequestStatus{State: state}}
}

func TestCapturedUnblockRowsCountsExpiryLicenseAndCursor(t *testing.T) {
	a, b, c := convenienceRequest("a", public.UnblockPending), convenienceRequest("b", public.UnblockPending), convenienceRequest("c", public.UnblockDenied)
	h, f := newConvenienceReadFixture(t, a, b, c)
	h.seed(store.ManagerAPI, convenienceRequest("live", public.UnblockApproved))
	cfg := testConfig(t)
	cfg.UnblockRequests.PendingExpiry = config.Duration(2 * time.Hour)
	h.srv.opts.CanonicalSettings = policyReadSettings{cfg: cfg}
	h.srv.opts.Config.UnblockRequests.PendingExpiry = config.Duration(72 * time.Hour)
	h.srv.opts.License = licenseManager(t, 0) // A competing unlicensed live manager cannot deny original permission.
	from := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	until := from.Add(time.Hour)
	now := from
	h.srv.opts.Now = func() time.Time { return now }
	f.entitlement = func(now time.Time) (bool, error) { return !now.Before(from) && now.Before(until), nil }
	var first UnblockRequestList
	h.do(http.MethodGet, "/api/v1/unblockrequests?state=Pending&limit=1", "auditor", nil).want(http.StatusOK).json(&first)
	if len(first.Items) != 1 || first.Items[0].Name != "a" || first.Counts.Pending != 2 || first.Counts.Denied != 1 || first.Counts.Approved != 0 || first.Continue == "" || first.Items[0].ExpiresAt == nil || !first.Items[0].ExpiresAt.Equal(from.Add(2*time.Hour)) {
		t.Fatal("read live rows/count/settings", first)
	}
	var detail UnblockRequest
	h.do(http.MethodGet, "/api/v1/unblockrequests/default/a", "auditor", nil).want(http.StatusOK).json(&detail)
	if detail.ExpiresAt == nil || !detail.ExpiresAt.Equal(*first.Items[0].ExpiresAt) {
		t.Fatal("detail used live expiry", detail)
	}
	path := "/api/v1/unblockrequests?state=Pending&limit=1&continue=" + url.QueryEscape(first.Continue)
	var next UnblockRequestList
	h.do(http.MethodGet, path, "auditor", nil).want(http.StatusOK).json(&next)
	if len(next.Items) != 1 || next.Items[0].Name != "b" || next.Continue != "" {
		t.Fatal("incorrect next request page", next)
	}
	for _, query := range []string{"state=Denied&limit=1", "state=Pending&limit=2", "limit=1"} {
		h.do(http.MethodGet, "/api/v1/unblockrequests?"+query+"&continue="+url.QueryEscape(first.Continue), "auditor", nil).want(http.StatusConflict)
	}
	f.publication++
	h.do(http.MethodGet, path, "auditor", nil).want(http.StatusConflict)
	f.publication--
	h.do(http.MethodGet, "/api/v1/unblockrequests/default/live", "auditor", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/unblockrequests", "viewer", nil).want(http.StatusForbidden)
	h.srv.opts.License = license.Unrestricted() // Renewal never extends the original finite window.
	for _, observed := range []time.Time{from.Add(-time.Nanosecond), until, until.Add(time.Hour)} {
		now = observed
		for _, p := range []string{"unblockrequests", "unblockrequests/default/a"} {
			h.do(http.MethodGet, "/api/v1/"+p, "auditor", nil).want(http.StatusPaymentRequired)
		}
	}
	now = until.Add(-time.Nanosecond)
	h.do(http.MethodGet, "/api/v1/unblockrequests", "auditor", nil).want(http.StatusOK)
	f.entitlement = func(time.Time) (bool, error) { return false, errors.New("missing original auth owner") }
	h.do(http.MethodGet, "/api/v1/unblockrequests", "auditor", nil).want(http.StatusServiceUnavailable)
}

func TestCapturedConvenienceReadsMissingCapabilitiesNeverFallback(t *testing.T) {
	list, request := convenienceList("ui-allow"), convenienceRequest("a", public.UnblockPending)
	h, f := newConvenienceReadFixture(t, list, request)
	h.seed(store.ManagerAPI, list, request)
	paths := []string{"domains", "allowlist", "denylist", "allowlist/" + domainlist.ID("default", public.EntryExact, "alpha.example"), "unblockrequests", "unblockrequests/default/a"}
	for _, fault := range []string{"missing-objects", "missing-identity", "identity-error", "missing-manifest", "object-error"} {
		t.Run(fault, func(_ *testing.T) {
			oldObjects, oldReader := f.objects, h.srv.opts.ObjectReads
			defer func() { f.objects, h.srv.opts.ObjectReads, f.identityErr, f.err = oldObjects, oldReader, nil, nil }()
			switch fault {
			case "missing-objects":
				h.srv.opts.ObjectReads = nil
			case "missing-identity":
				h.srv.opts.ObjectReads = f.objectReadFixture
			case "identity-error":
				f.identityErr = context.Canceled
			case "missing-manifest":
				f.objects = zonecache.ObjectSet{}
			case "object-error":
				f.err = context.Canceled
			}
			for _, p := range paths {
				h.do(http.MethodGet, "/api/v1/"+p, "admin", nil).want(http.StatusServiceUnavailable)
			}
		})
	}
	snap, err := zonecache.Prepare(zonecache.PrepareOptions{}, []store.Object{list}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.objects = snap.Objects
	for _, p := range paths[:4] {
		h.do(http.MethodGet, "/api/v1/"+p, "admin", nil).want(http.StatusServiceUnavailable)
	}
	prepareConvenienceReadFixture(t, f, list, request)
	h.srv.opts.CanonicalSettings = nil
	for _, p := range paths[4:] {
		h.do(http.MethodGet, "/api/v1/"+p, "admin", nil).want(http.StatusServiceUnavailable)
	}
}

// Optional capabilities are independent: identity alone cannot imply workflow
// rows or an entitlement owner, even when the live license allows requests.
type identityOnlyConvenienceReads struct {
	ObjectReadSource
	convenienceReadIdentity
}

func TestCapturedUnblockMissingOwnersAndDeadlineDuringProjection(t *testing.T) {
	h, f := newConvenienceReadFixture(t, convenienceRequest("a", public.UnblockPending))
	h.srv.opts.License = license.Unrestricted()
	paths := []string{"unblockrequests", "unblockrequests/default/a", "unblockrequests/default/missing"}
	h.srv.opts.ObjectReads = identityOnlyConvenienceReads{f, f}
	for _, path := range paths {
		h.do(http.MethodGet, "/api/v1/"+path, "auditor", nil).want(http.StatusServiceUnavailable)
	}
	h.srv.opts.ObjectReads = f
	for _, path := range paths {
		calls := 0
		f.entitlement = func(time.Time) (bool, error) { calls++; return calls == 1, nil }
		h.do(http.MethodGet, "/api/v1/"+path, "auditor", nil).want(http.StatusPaymentRequired)
		if calls != 2 {
			t.Fatal("response did not recheck original deadline", calls)
		}
	}
	f.entitlement = func(time.Time) (bool, error) { return true, nil }
	h.srv.opts.CanonicalSettings = policyReadSettings{err: context.Canceled}
	for _, path := range paths {
		h.do(http.MethodGet, "/api/v1/"+path, "auditor", nil).want(http.StatusServiceUnavailable)
	}
}

func TestCapturedConvenienceReadsPreserveExactCommandResponses(t *testing.T) {
	h, st := domainCommandHarness(t, nil)
	_, reads := newConvenienceReadFixture(t, convenienceList("ui-allow"), convenienceRequest("request", public.UnblockPending))
	h.srv.opts.ObjectReads = reads
	h.srv.opts.CanonicalSettings = policyReadSettings{cfg: testConfig(t)}
	var domains DomainWriteResult
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", DomainEntriesCreate{Values: []string{"committed.example"}, Comment: "command result"}).want(http.StatusOK).json(&domains)
	if len(domains.Items) != 1 || domains.Items[0].Value != "committed.example" || domains.Items[0].Comment != "command result" {
		t.Fatal("write response adopted captured read image", domains)
	}
	request := convenienceRequest("request", public.UnblockPending)
	request.Spec.Domain = "committed.example"
	zoneSeed(t, st, unblock.Manager, request)
	h.srv.opts.UnblockCommand = func(ctx context.Context, target unblock.Target, decision unblock.Decision) (*unblock.Outcome, error) {
		var result *unblock.Outcome
		err := st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var err error
			result, err = unblock.ExecuteCurrent(ctx, tx, target, decision, time.Now())
			return err
		})
		return result, err
	}
	var result UnblockDecisionResult
	h.do(http.MethodPost, "/api/v1/unblockrequests/default/request/approve", "operator", UnblockApproval{}).want(http.StatusOK).json(&result)
	if result.Request.Domain != "committed.example" || result.Request.State != "Approved" || result.Request.Allow == nil {
		t.Fatal("decision response adopted captured pending row", result)
	}
	var committed public.UnblockRequest
	if err := st.Get(t.Context(), store.KeyOf(request), &committed); err != nil || committed.ResourceVersion != result.Request.ResourceVersion {
		t.Fatal("decision response lost exact commit version", err, result.Request.ResourceVersion)
	}
}

func TestCapturedDomainDenyViewsAndLiveUnblockIndex(t *testing.T) {
	deny := convenienceList("ui-deny-kids")
	deny.Spec.Mode = "block"
	allow := &public.BlockList{ObjectMeta: objectMeta("default", "feed"), Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "original", Inline: []string{"alpha.example"}, Exclude: []string{"alpha.example"}}}}}
	policy := &public.Policy{ObjectMeta: objectMeta("default", "original"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{deny.Name}, AllowListRefs: []string{allow.Name}}}
	h, _ := newConvenienceReadFixture(t, deny, allow, policy, convenienceRequest("captured", public.UnblockPending))
	var page DomainEntries
	h.do(http.MethodGet, "/api/v1/denylist?kind=entry&scope=kids&q=alpha", "viewer", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.Items[0].Action != "deny" || !slices.Equal(page.Items[0].Overrides, []DomainOverride{{List: "default/captured-allow", Rule: "alpha.example"}}) {
		t.Fatal("deny view lost captured allow matches", page)
	}
	var exception DomainEntry
	h.do(http.MethodGet, "/api/v1/denylist/"+domainlist.ExceptionID("default", "feed", "original", "alpha.example"), "viewer", nil).want(http.StatusOK).json(&exception)
	if exception.Action != "deny" || !slices.Equal(exception.Policies, []string{"default/original"}) || !slices.Equal(exception.StillMatchedBy, []DomainOverride{{List: "default/captured-allow", Rule: "alpha.example"}}) {
		t.Fatal("deny exception mixed captured mode", exception)
	}
	h.seed(store.ManagerAPI, convenienceRequest("live-index", public.UnblockDenied))
	svc := unblock.New(unblock.Options{Store: h.st})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	select {
	case <-svc.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("live index did not sync")
	}
	h.srv.opts.Unblock = svc
	var requests UnblockRequestList
	h.do(http.MethodGet, "/api/v1/unblockrequests", "auditor", nil).want(http.StatusOK).json(&requests)
	if len(requests.Items) != 1 || requests.Items[0].Name != "captured" || requests.Counts.Pending != 1 || requests.Counts.Denied != 0 {
		t.Fatal("read live synced index", requests)
	}
}

func TestCapturedExceptionInactiveWildcardAndRegexIdentity(t *testing.T) {
	for _, test := range []struct {
		format            public.ListFormat
		value, line, kind string
	}{
		{"adblock", "*.alpha.example", "||alpha.example^", "wildcard"},
		{"regex", `^ads[0-9]+\.example$`, `^ads[0-9]+\.example$`, "regex"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			bl := &public.BlockList{ObjectMeta: objectMeta("default", "feed"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Format: test.format, Inline: []string{test.line}}}}}
			h, _ := newConvenienceReadFixture(t, bl)
			var e DomainEntry
			h.do(http.MethodGet, "/api/v1/allowlist/"+domainlist.ExceptionID("default", "feed", "original", test.value), "viewer", nil).want(http.StatusOK).json(&e)
			if e.Enabled || e.Type != test.kind || e.Value != test.value {
				t.Fatal("inactive exception lost parser identity", e)
			}
		})
	}
}
