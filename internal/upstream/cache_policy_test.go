package upstream

import (
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestGroupCachePolicyAndRouteScope(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config.Upstreams{Groups: map[string]config.UpstreamGroup{
			"default": {Servers: []string{"127.0.0.1:10001"}, Cache: enabled},
			"lan":     {Servers: []string{"127.0.0.1:10002"}, Cache: enabled},
		}, Conditional: map[string]string{"corp.example.": "lan"}}
		set, err := New(ConfigFrom(cfg), Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		if !set.GroupCaches("default") || set.GroupCaches("missing") || set.GroupCaches("lan") != enabled {
			t.Fatal("group cache opt-in changed the default or accepted an unknown group")
		}
		q := new(dns.Msg)
		q.SetQuestion("host.corp.example.", dns.TypeA)
		req := &pipeline.Request{Msg: q}
		conditional, err := set.Plan(req, nil, false, 1, nil)
		if err != nil || conditional.Group() != "lan" || conditional.Cacheable() != enabled {
			t.Fatalf("conditional policy lost: %v", err)
		}
		req.UpstreamGroup = "lan"
		hinted, err := set.Plan(req, nil, false, 1, nil)
		if err != nil || hinted.Scope() != conditional.Scope() || hinted.Cacheable() != enabled {
			t.Fatalf("hinted route uses a different policy/partition: %v", err)
		}
		q.Question[0].Name = "public.example."
		req.UpstreamGroup = ""
		ordinary, err := set.Plan(req, nil, false, 1, nil)
		if err != nil || !ordinary.Cacheable() || ordinary.Scope() == conditional.Scope() {
			t.Fatalf("default cache policy or group isolation lost: %v", err)
		}
	}
}
