package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type policyReadSettings struct {
	cfg *config.Config
	err error
}

func (r policyReadSettings) Settings(context.Context) (*config.Config, error) { return r.cfg, r.err }
func (policyReadSettings) WriteSettings(context.Context, *config.Config, []string) error {
	return errors.New("test read-only settings")
}

type policyReadLists struct {
	err   error
	now   []time.Time
	names []string
}

func (r *policyReadLists) PolicyListState(_ context.Context, ns, name string, now time.Time) (int, bool, error) {
	r.now = append(r.now, now)
	r.names = append(r.names, ns+"/"+name)
	return 7, true, r.err
}

func newPolicyReadAPI(t *testing.T) (*harness, *objectReadFixture, *policyReadLists) {
	t.Helper()
	on, off := true, false
	p := &public.Policy{ObjectMeta: objectMeta("apps", "p"), Spec: public.PolicySpec{
		ClientGroupRefs: []string{"kids", "missing"}, BlockListRefs: []string{"list", "missing", "disabled"}, AllowListRefs: []string{"allow"},
		BlockedServices: []string{"youtube"}, BlockedCategories: []string{"video"}, AllowedServices: []string{"vimeo"}, SafeSearch: &on,
		UpstreamGroup: "captured", Priority: 10, Schedule: &public.Schedule{Start: "00:00", End: "00:00"},
	}}
	p.Status.Conditions = []metav1.Condition{{Type: public.ConditionReady, Status: metav1.ConditionFalse, Reason: "OriginalStatus"}}
	list := &public.BlockList{ObjectMeta: objectMeta("apps", "list"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"captured.example"}}, {Enabled: &off, Inline: []string{"disabled.example"}}}}}
	list.Status.Entries = 999 // Deliberately stale desired status, never serving count.
	disabled := list.DeepCopy()
	disabled.Name = "disabled"
	disabled.Annotations = map[string]string{public.EnabledAnnotation: "false"}
	allow := list.DeepCopy()
	allow.Name, allow.Spec.Mode = "allow", "allow"
	g := &public.ClientGroup{ObjectMeta: objectMeta("apps", "kids"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}
	peer := &public.Policy{ObjectMeta: objectMeta("apps", "higher"), Spec: public.PolicySpec{Priority: 20, ClientGroupRefs: []string{"kids"}}}
	h, objects := newObjectReadFixture(t, p, list, disabled, allow, g, peer)
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Blocking.DefaultResponse = "zeroIP"
	cfg.Upstreams.Groups["captured"] = cfg.Upstreams.Groups["default"]
	r := &policyReadLists{}
	h.srv.opts.CanonicalSettings = policyReadSettings{cfg: cfg}
	h.srv.opts.PolicyListReads = r
	h.srv.opts.Catalogue.Read = func(context.Context) (*catalogue.Catalogue, error) { return catalogue.Embedded(), nil }
	return h, objects, r
}

func TestCapturedPolicyStateProjectsOriginalObjectsSettingsAndActualCounts(t *testing.T) {
	h, _, r := newPolicyReadAPI(t)
	// A competing live row must never appear in the captured projection.
	h.seed(store.ManagerAPI, &public.Policy{ObjectMeta: objectMeta("apps", "p"), Spec: public.PolicySpec{Priority: 999}},
		&public.Policy{ObjectMeta: objectMeta("apps", "live-only")})
	h.srv.opts.Catalogue.Status = func() catalogue.Status { panic("live catalog refresher consulted") }
	h.srv.opts.Catalogue.ImportStatus = h.srv.opts.Catalogue.Status
	now := time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
	clockCalls := 0
	h.srv.opts.Now = func() time.Time { clockCalls++; return now }
	var out PolicyState
	h.do(http.MethodGet, "/api/v1/policies/apps/p/state", "viewer", nil).want(http.StatusOK).json(&out)
	if out.Priority != 10 || !out.Active || out.BlockMode != "zeroIP" || !slices.Equal(out.ShadowedBy, []string{"apps/higher"}) || out.Schedule == nil || !out.Schedule.Now || out.UpstreamGroup == nil || !out.UpstreamGroup.Exists {
		t.Fatal("policy mixed live metadata/settings", out)
	}
	wantLists := []PolicyList{
		{Ref: "list", Role: "block", Mode: "block", Exists: true, Enabled: true, Ready: true, Entries: 7, SourcesEnabled: 1, SourcesTotal: 2},
		{Ref: "missing", Role: "block", Mode: "block"},
		{Ref: "disabled", Role: "block", Mode: "block", Exists: true, SourcesEnabled: 1, SourcesTotal: 2},
		{Ref: "allow", Role: "allow", Mode: "allow", Exists: true, Enabled: true, Ready: true, Entries: 7, SourcesEnabled: 1, SourcesTotal: 2},
	}
	if !slices.Equal(out.Lists, wantLists) || len(out.Groups) != 2 || out.Groups[0].Clients != 1 || out.Groups[1].Exists || len(out.Conditions) != 1 || out.Conditions[0].Reason != "OriginalStatus" {
		t.Fatal("desired metadata/status or actual list counts lost", out)
	}
	if clockCalls != 1 || !slices.Equal(r.names, []string{"apps/list", "apps/allow"}) || len(r.now) != 2 || !r.now[0].Equal(now) || !r.now[1].Equal(now) {
		t.Fatal("policy used multiple observation instants or queried missing/disabled lists", clockCalls, r)
	}
	video, _ := catalogue.Embedded().Category("video")
	if len(out.Services) != 1 || out.Services[0].Name != "YouTube" || len(out.SafeSearch.Engines) == 0 || out.Categories[0].Name != video.Name || out.Categories[0].Allowed != 1 {
		t.Fatal("captured catalog not projected", out)
	}
	h.do(http.MethodGet, "/api/v1/policies/apps/live-only/state", "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/policies/apps/p/state", "zoneadmin", nil).want(http.StatusForbidden)
}

func TestCapturedPolicyStateMissingCapabilitiesAndFailuresNeverFallback(t *testing.T) {
	for _, missing := range []string{"settings", "catalog", "lists", "settings-nil", "settings-error", "catalog-nil", "catalog-error", "lists-error", "objects-error"} {
		t.Run(missing, func(t *testing.T) {
			h, f, lists := newPolicyReadAPI(t)
			switch missing {
			case "settings":
				h.srv.opts.CanonicalSettings = nil
			case "catalog":
				h.srv.opts.Catalogue.Read = nil
			case "lists":
				h.srv.opts.PolicyListReads = nil
			case "settings-nil":
				h.srv.opts.CanonicalSettings = policyReadSettings{}
			case "settings-error":
				h.srv.opts.CanonicalSettings = policyReadSettings{err: context.Canceled}
			case "catalog-nil":
				h.srv.opts.Catalogue.Read = func(context.Context) (*catalogue.Catalogue, error) { return nil, nil }
			case "catalog-error":
				h.srv.opts.Catalogue.Read = func(context.Context) (*catalogue.Catalogue, error) { return nil, context.Canceled }
			case "lists-error":
				lists.err = store.ErrNotFound
			case "objects-error":
				f.err = context.Canceled
			}
			h.do(http.MethodGet, "/api/v1/policies/apps/p/state", "viewer", nil).want(http.StatusServiceUnavailable)
		})
	}
	h, f, _ := newPolicyReadAPI(t)
	req := h.req(http.MethodGet, "/api/v1/policies/apps/p/state", "viewer", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	h.serve(req.WithContext(ctx)).want(http.StatusServiceUnavailable)
	if f.calls != 0 {
		t.Fatal("canceled original read called capture")
	}
}

func TestCapturedPolicyStateNilCapabilityKeepsLegacy(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerAPI, &public.Policy{ObjectMeta: objectMeta("apps", "p"), Spec: public.PolicySpec{BlockedServices: []string{"youtube"}}})
	var out PolicyState
	h.do(http.MethodGet, "/api/v1/policies/apps/p/state", "viewer", nil).want(http.StatusOK).json(&out)
	if len(out.Services) != 1 || !strings.Contains(out.Services[0].Name, "YouTube") || !out.Active {
		t.Fatal("nil capability changed legacy policy state", out)
	}
}
