package servingbuild

import (
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSharedCacheOriginalConfiguredInputs(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*config.Config)
	}{
		{"uniform", "", func(*config.Config) {}},
		{"default-limiter", "rate-limit", func(c *config.Config) { c.RateLimit.Enabled = true }},
		{"ptr", "async-ptr", func(c *config.Config) { c.ClientNames.ReversePTR.Enabled = true }},
		{"dhcp", "dhcp-runtime", func(c *config.Config) { c.DHCP.Enabled = true }},
		{"dhcp-scopes", "dhcp-runtime", func(c *config.Config) { c.DHCP.Scopes = []config.DHCPScope{{Name: "original"}} }},
		{"network-acl", "client-acl", func(c *config.Config) { c.Control.AllowedNetworks = []string{"192.0.2.0/24"} }},
		{"ecs-forward", "ecs", func(c *config.Config) { c.Upstreams.ECS.Mode = "forward" }},
		{"ecs-add", "ecs", func(c *config.Config) { c.Upstreams.ECS.Mode = "add" }},
		{"ecs-strip", "", func(c *config.Config) { c.Upstreams.ECS.Mode = "strip" }},
		{"rebinding-bypass", "rebinding-client-scope", func(c *config.Config) {
			c.Response.Rebinding.Enabled = true
			c.Response.Rebinding.BypassNetworks = []string{"192.0.2.0/24"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.FromFlat(config.Defaults())
			if err != nil {
				t.Fatal(err)
			}
			cfg.RateLimit.Enabled = false
			tc.change(cfg)
			lists, err := blocking.NewSnapshot(1)
			if err != nil {
				t.Fatal(err)
			}
			objects, err := zonecache.Prepare(zonecache.PrepareOptions{Lists: lists}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			owners := &semanticOwners{config: cfg, policy: policy.Build(policy.Input{}, policy.Options{}), objects: objects}
			if reason := sharedCacheBypass(owners); reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
	if sharedCacheBypass(nil) != "input-unavailable" {
		t.Fatal("missing original inputs qualified")
	}
}

func TestSharedCacheCapturedZoneAndCompiledRuleInputs(t *testing.T) {
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RateLimit.Enabled = false
	for _, access := range []public.AccessMode{"", public.AccessAllow, public.AccessDeny, public.AccessAllowOnlyPrivate, public.AccessAllowOnlyZoneNameServers, public.AccessUseNetworkACL, public.AccessZoneNameServersAndACL, "unknown"} {
		z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "forward"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: "forward.example.", Policy: public.ZonePolicy{QueryAccess: access}}}
		lists, err := blocking.NewSnapshot(1)
		if err != nil {
			t.Fatal(err)
		}
		objects, err := zonecache.Prepare(zonecache.PrepareOptions{Lists: lists}, []store.Object{z}, nil)
		if err != nil {
			t.Fatal(err)
		}
		owners := &semanticOwners{config: cfg, policy: policy.Build(policy.Input{}, policy.Options{}), objects: objects}
		want := "zone-client-acl"
		if access == "" || access == public.AccessAllow || access == public.AccessDeny {
			want = ""
		}
		if reason := sharedCacheBypass(owners); reason != want {
			t.Fatalf("access %s reason=%s want=%s", access, reason, want)
		}
		// Prepare copied the original. Editing desired input cannot repair it.
		z.Spec.Policy.QueryAccess = public.AccessAllow
		if reason := sharedCacheBypass(owners); reason != want {
			t.Fatal("desired zone replaced original query ACL")
		}
	}
	if zoneQueriesClientIndependent([]*public.DNSZone{{Spec: public.DNSZoneSpec{Policy: public.ZonePolicy{QueryAccess: public.AccessAllow, QueryACL: []string{"192.0.2.0/24"}}}}}) {
		t.Fatal("specified network ACL qualified")
	}
	// Transfer/notify credentials grant no query rights. They neither create
	// a client-dependent query nor repair a restricted query access mode.
	if !zoneQueriesClientIndependent([]*public.DNSZone{{Spec: public.DNSZoneSpec{Policy: public.ZonePolicy{QueryAccess: public.AccessAllow, TransferTSIGKeyRefs: []string{"transfer-only"}}}}}) {
		t.Fatal("transfer key confused with query identity")
	}
	l, err := blocking.Compile(blocking.Spec{Name: "original", Mode: blocking.ModeBlock, Sources: []blocking.Source{blocking.LinesSource("inline", parser.FormatAdblock, []string{"||same.example^$client=192.0.2.1"})}}, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lists, err := blocking.NewSnapshot(1, l)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := zonecache.Prepare(zonecache.PrepareOptions{Lists: lists}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	owners := &semanticOwners{config: cfg, policy: policy.Build(policy.Input{}, policy.Options{}), objects: objects}
	if sharedCacheBypass(owners) != "rule-client-scope" {
		t.Fatal("original compiled client rule qualified")
	}
}

func TestEdgeCacheOriginalEligibilityProjection(t *testing.T) {
	f := newFixture(t)
	edge, err := NewEdgeCache(f.pub, "original-node", func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	held, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	before, err := edge.State(t.Context())
	if err != nil || before.Available || before.UnavailableReason != "rate-limit" {
		t.Fatal("default limiter silently qualified")
	}
	s := settings("192.0.2.11")
	s.Spec.Settings["ratelimit"] = extensions.JSON{Raw: []byte(`{"enabled":false}`)}
	if _, err := f.store.Apply(t.Context(), s, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	after, err := edge.State(t.Context())
	if err != nil || !after.Available || after.UnavailableReason != "" || after.Publication <= before.Publication {
		t.Fatal("uniform original publication did not qualify")
	}
	old, err := edge.project(held, f.now)
	if err != nil || old.Available || old.UnavailableReason != "rate-limit" {
		t.Fatal("fresh config repaired held original eligibility")
	}
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.example.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"192.0.2.1/32"}}}}
	if _, err := f.store.Apply(t.Context(), z, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	denied, err := edge.Zones(t.Context())
	if err != nil || denied.Resolver == nil || denied.Resolver.Available || denied.Resolver.UnavailableReason != "zone-client-acl" {
		t.Fatal("captured forward-zone ACL qualified resolver pool")
	}
}
