package servingdns_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/rewrite"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type inventoryView struct{ snapshot *inventory.Snapshot }

// Known-empty auth is proved by the exact fixture image, not a production
// compiler substitute. The test rejects any identity object it cannot compile.
type emptyAuthView struct{ base replication.Generation }
type fixtureRuntime struct{ node, route string }

type queryGate struct {
	entered, resume chan struct{}
	calls           atomic.Int64
}

func (*queryGate) Name() string { return "ingress" }
func (g *queryGate) Serve(r *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
		select {
		case <-g.resume:
		case <-r.Ctx.Done():
			return nil, r.Ctx.Err()
		}
	}
	return next(r)
}

func udpOwner(t *testing.T, ip string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	s := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(ready) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if q.Question[0].Qtype == dns.TypeA {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: 1, Ttl: 300}, A: net.ParseIP(ip)}}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = s.ActivateAndServe() }()
	<-ready
	t.Cleanup(func() { _ = s.Shutdown() })
	return pc.LocalAddr().String()
}

// This fixture prepares REAL DNS owners from a SQLite candidate plus explicitly
// selected local dependency inputs. The exact fixture has no auth identities; its auth value records this proven
// empty cut. It does not test production auth compilation or generation gates.
func TestBundleCaptureAllOldAllNewThroughCNAMEAndDNS64(t *testing.T) {
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(t.TempDir(), sqlite.FileName), sqlite.Options{Validator: v, Identity: "dns-fixture", InitialMode: replication.Primary})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	apply := func(cut string) {
		t.Helper()
		objects := []store.Object{
			&v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "local", Labels: map[string]string{"cut": cut}}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypePrimary, ZoneName: "local.example.", Nameservers: []string{"ns.local.example."}, Policy: v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow}}},
			&v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "alias", Namespace: "default"}, Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "local"}, Endpoints: []v1alpha1.Endpoint{{DNSName: "cross.local.example.", RecordType: "CNAME", Targets: []string{cut + ".remote.example."}}}}},
			&v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
			&v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default", Labels: map[string]string{"cut": cut}}, Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockTTL: metav1.Duration{Duration: map[string]time.Duration{"old": 11 * time.Second, "new": 22 * time.Second}[cut]}}},
		}
		if _, err := db.ApplyCluster(t.Context(), objects, store.ApplyOptions{Manager: store.ManagerAPI}, nil); err != nil {
			t.Fatal(err)
		}
	}
	apply("old")
	addresses := map[string]string{"old": udpOwner(t, "192.0.2.11"), "new": udpOwner(t, "192.0.2.22")}
	prefixes := map[string]string{"old": "64:ff9b::/96", "new": "2001:db8:64::/96"}
	var retiredOld, retiredNew atomic.Int64
	prepare := func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
		var dz *v1alpha1.DNSZone
		var sets []*v1alpha1.DNSRecordSet
		var groups []*v1alpha1.ClientGroup
		var policies []*v1alpha1.Policy
		for _, raw := range c.Image.Payload.Objects {
			var head struct{ Kind string }
			if err := json.Unmarshal(raw, &head); err != nil {
				return serving.Prepared{}, err
			}
			switch head.Kind {
			case "LocalUser", "APIToken", "ServiceAccount", "RoleBinding", "IdentityProvider", "PrincipalState", "Session":
				return serving.Prepared{}, errors.New("fixture auth compiler only supports a proven empty cut")
			case "DNSZone":
				dz = new(v1alpha1.DNSZone)
				if err := json.Unmarshal(raw, dz); err != nil {
					return serving.Prepared{}, err
				}
			case "DNSRecordSet":
				obj := new(v1alpha1.DNSRecordSet)
				if err := json.Unmarshal(raw, obj); err != nil {
					return serving.Prepared{}, err
				}
				sets = append(sets, obj)
			case "ClientGroup":
				obj := new(v1alpha1.ClientGroup)
				if err := json.Unmarshal(raw, obj); err != nil {
					return serving.Prepared{}, err
				}
				groups = append(groups, obj)
			case "Policy":
				obj := new(v1alpha1.Policy)
				if err := json.Unmarshal(raw, obj); err != nil {
					return serving.Prepared{}, err
				}
				policies = append(policies, obj)
			}
		}
		if err := ctx.Err(); err != nil {
			return serving.Prepared{}, err
		}
		if dz == nil {
			return serving.Prepared{}, errors.New("missing fixture zone")
		}
		cut := dz.Labels["cut"]
		z, buildErrors := zone.Build(dz, sets, zone.BuildOptions{})
		if !buildErrors.Empty() {
			return serving.Prepared{}, errors.New("zone build failed")
		}
		zs, err := zone.NewSnapshot(1, true, z)
		if err != nil {
			return serving.Prepared{}, err
		}
		ix := policy.Build(policy.Input{ClientGroups: groups, Policies: policies}, policy.Options{Inventory: true})
		response, err := responsepolicy.Compile(responsepolicy.Options{DNS64Prefix: prefixes[cut]})
		if err != nil {
			return serving.Prepared{}, err
		}
		block, err := blocking.NewView(blocking.Options{}, time.Time{}, catalogue.Embedded())
		if err != nil {
			return serving.Prepared{}, err
		}
		cv, err := cachestage.NewView(cachestage.Settings{}, false, func() bool { return true })
		if err != nil {
			return serving.Prepared{}, err
		}
		inv := inventory.New(nil)
		now := time.Now()
		if err = inv.Replace("fixture", 1, []inventory.Client{{ID: cut, IPs: []netip.Addr{netip.MustParseAddr("192.0.2.100")}, Hostname: cut + ".lan", Tags: []string{cut}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)}}); err != nil {
			return serving.Prepared{}, err
		}
		set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{addresses[cut]}}}}, upstream.Options{})
		if err != nil {
			return serving.Prepared{}, err
		}
		rv, err := upstreamstage.NewView(set, nil, false, func() bool { return true })
		if err != nil {
			_ = set.Close()
			return serving.Prepared{}, err
		}
		// The fixture's proven empty global rules are an explicit member of
		// the complete publication, just like the production control owner.
		controls, err := control.NewView(control.Options{})
		if err != nil {
			_ = set.Close()
			return serving.Prepared{}, err
		}
		components := map[string]any{"objects": c.Image, "zones": zone.Set(zs), "policy": ix, "resolution": rv, "response": response, "cache": cv, "blocking": block, "inventory": inventoryView{inv.Snapshot()}, "auth": emptyAuthView{c.Token.Generation()}, "runtime": fixtureRuntime{"dns-fixture", addresses[cut]}, "control": controls, "logging": &querylog.Captured{}}
		prepared := serving.Prepared{Current: func() bool { return true }}
		for name, value := range components {
			component := serving.Component{Name: name, Base: c.Token.Generation(), Identity: c.Token.Generation().Digest + ":" + name, Value: value}
			if name == "resolution" {
				component.Release = func() {
					_ = set.Close()
					if cut == "old" {
						retiredOld.Add(1)
					} else {
						retiredNew.Add(1)
					}
				}
			}
			prepared.Components = append(prepared.Components, component)
		}
		return prepared, nil
	}
	pub, err := serving.New(serving.Options{Source: db, Required: serving.CompleteManifest(), Prepare: prepare})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		pub.Close()
	}()
	select {
	case <-pub.Initialized():
	case <-time.After(3 * time.Second):
		t.Fatal("publisher startup")
	}
	gate := &queryGate{entered: make(chan struct{}), resume: make(chan struct{})}
	id := identify.New(nil, nil)
	id.Capture = func(r *pipeline.Request) (*policy.Index, error) {
		return servingdns.Component[*policy.Index](r, "policy")
	}
	id.CaptureInventory = func(r *pipeline.Request) (*inventory.Snapshot, error) {
		v, e := servingdns.Component[inventoryView](r, "inventory")
		return v.snapshot, e
	}
	post := &postprocess.Stage{Capture: func(r *pipeline.Request) (*responsepolicy.Settings, error) {
		return servingdns.Component[*responsepolicy.Settings](r, "response")
	}}
	auth := authoritative.New(nil)
	auth.Capture = func(r *pipeline.Request) zone.Set { v, _ := servingdns.Component[zone.Set](r, "zones"); return v }
	block, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	block.Capture = func(r *pipeline.Request) (*blocking.View, error) {
		return servingdns.Component[*blocking.View](r, "blocking")
	}
	up := &upstreamstage.Stage{Capture: func(r *pipeline.Request) (*upstreamstage.View, error) {
		return servingdns.Component[*upstreamstage.View](r, "resolution")
	}}
	cs := cachestage.New(cache.New(cache.Options{MaxEntries: 100}), cachestage.Settings{}, up)
	cs.Capture = func(r *pipeline.Request) (*cachestage.View, error) {
		return servingdns.Component[*cachestage.View](r, "cache")
	}
	chain := pipeline.MustNew(gate, id, post, auth, block, &rewrite.Stage{}, cs, up)
	chain.SetCapture(servingdns.Capture(pub))
	request := func() *pipeline.Request {
		m := new(dns.Msg)
		m.SetQuestion("cross.local.example.", dns.TypeAAAA)
		return &pipeline.Request{Ctx: t.Context(), Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.100:4000")}}
	}
	type answer struct {
		response *pipeline.Response
		err      error
	}
	oldReq := request()
	oldResult := make(chan answer, 1)
	go func() { r, e := chain.Serve(oldReq); oldResult <- answer{r, e} }()
	<-gate.entered
	waitCut := func(cut string) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for {
			l, e := pub.Capture()
			if e == nil {
				value, _ := l.Bundle().Component("objects")
				image := value.(*replication.Image)
				found := false
				for _, raw := range image.Payload.Objects {
					var obj v1alpha1.DNSZone
					if json.Unmarshal(raw, &obj) == nil && obj.Kind == "DNSZone" && obj.Labels["cut"] == cut {
						found = true
					}
				}
				l.Release()
				if found {
					break
				}
			}
			if time.Now().After(until) {
				t.Fatal("bundle not published", cut)
			}
			time.Sleep(time.Millisecond)
		}
	}
	apply("new")
	waitCut("new")
	if retiredOld.Load() != 0 {
		t.Fatal("old pools retired under paused request")
	}
	close(gate.resume)
	old := <-oldResult
	if old.err != nil {
		t.Fatal(old.err)
	}
	defer old.response.Close()
	if got := old.response.Msg.Answer[len(old.response.Msg.Answer)-1].(*dns.AAAA).AAAA.String(); got != "64:ff9b::c000:20b" {
		t.Fatal("mixed old CNAME/DNS64/resolver", got)
	}
	if old.response.Msg.Answer[0].(*dns.CNAME).Target != "old.remote.example." || policy.FromRequest(oldReq).BlockTTL != 11*time.Second {
		t.Fatal("zone/policy crossed old cut")
	}
	if !slices.Equal(oldReq.Client.Tags, []string{"old"}) || !slices.Equal(oldReq.Client.Names, []string{"old.lan"}) {
		t.Fatal("inventory crossed cut", oldReq.Client)
	}
	newReq := request()
	next, e := chain.Serve(newReq)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	if got := next.Msg.Answer[len(next.Msg.Answer)-1].(*dns.AAAA).AAAA.String(); got != "2001:db8:64::c000:216" {
		t.Fatal("mixed new owners", got)
	}
	if next.Msg.Answer[0].(*dns.CNAME).Target != "new.remote.example." || policy.FromRequest(newReq).BlockTTL != 22*time.Second {
		t.Fatal("zone/policy crossed new cut")
	}
	if !slices.Equal(newReq.Client.Tags, []string{"new"}) {
		t.Fatal("new inventory missing", newReq.Client)
	}
	cs.Wait()
	if retiredOld.Load() != 1 {
		t.Fatalf("old pool release count %d", retiredOld.Load())
	}
	// Bridge ownership is real: cloned old/current bundle leases delay release.
	lease, e := pub.Capture()
	if e != nil {
		t.Fatal(e)
	}
	wrapped := servingdns.Wrap(lease)
	cloned, e := wrapped.Clone()
	if e != nil {
		t.Fatal(e)
	}
	wrapped.Release()
	r := request()
	r.Serving = cloned
	if _, e = servingdns.Component[*policy.Index](r, "policy"); e != nil {
		t.Fatal(e)
	}
	if _, e = servingdns.Component[*policy.Index](r, "response"); !errors.Is(e, pipeline.ErrServingCapture) {
		t.Fatal("wrong type accepted", e)
	}
	cloned.Release()
	cloned.Release()
	if _, e = servingdns.Component[*policy.Index](r, "policy"); !errors.Is(e, pipeline.ErrServingCapture) {
		t.Fatal("released bridge accepted", e)
	}
	// A real transfer response owns the entire bundle, even when never iterated.
	xfrReq := request()
	xfrReq.Msg.SetAxfr("local.example.")
	xfrReq.Client.Proto = pipeline.ProtoTCP
	xfr, e := chain.Serve(xfrReq)
	if e != nil || xfr.Transfer == nil {
		t.Fatal("captured transfer", e)
	}
	defer xfr.Close()
	apply("old")
	waitCut("old")
	if retiredNew.Load() != 0 {
		t.Fatal("transfer pool retired before explicit cleanup")
	}
	xfrCopy := *xfr
	xfrCopy.Close()
	xfr.Close()
	if retiredNew.Load() != 1 {
		t.Fatal("never-iterated transfer did not release real bundle")
	}

}
