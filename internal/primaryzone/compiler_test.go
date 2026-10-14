package primaryzone

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var fixedNow = time.Unix(1800000000, 0)

func desiredZone() *public.DNSZone {
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "zone-uid", ResourceVersion: "zone-opaque", CreationTimestamp: metav1.NewTime(fixedNow)}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."}, DefaultTTL: 300, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}, Status: public.DNSZoneStatus{Serial: 10}}
	store.SetManagedBy(z, "api")
	return z
}
func desiredSet(name string, eps ...public.Endpoint) *public.DNSRecordSet {
	rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dns", UID: types.UID(name + "-uid"), ResourceVersion: name + "-opaque", CreationTimestamp: metav1.NewTime(fixedNow)}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "example"}, Endpoints: eps}}
	store.SetManagedBy(rs, "api")
	return rs
}
func endpoint(name string, target string) public.Endpoint {
	return public.Endpoint{DNSName: name + ".example.", RecordType: "A", Targets: []string{target}}
}
func blank(z *public.DNSZone) *zonestate.View {
	return &zonestate.View{Expected: zonestate.Expected{Zone: zonekinds.ZoneID{ClusterID: "test", ObjectName: z.Name, UID: string(z.UID), Apex: "example."}}}
}
func prepareView(t testing.TB, a *zonestate.Aggregate, base *zonekinds.ZoneState) *zonestate.View {
	t.Helper()
	p, err := zonestate.Prepare(a, base, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := zonestate.VerifyPrepared(p, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	p.Head.ResourceVersion = "durable-opaque"
	return &zonestate.View{Head: p.Head, Aggregate: decoded, Expected: zonestate.Expected{Zone: p.Head.Spec.Zone, ResourceVersion: p.Head.ResourceVersion, CommitSequence: p.Head.Spec.CommitSequence, RootDigest: p.Head.Spec.RootDigest, AuthEpoch: p.Head.Spec.AuthEpoch}}
}
func compile(t testing.TB, c *Compiler, base *zonestate.View, in *zonestate.DesiredInputs, now time.Time) *zonestate.View {
	t.Helper()
	result, err := c.Project(context.Background(), base, in, now)
	if err != nil {
		t.Fatal(err)
	}
	return prepareView(t, result.Aggregate, base.Head)
}
func built(t testing.TB, v *zonestate.View) *zone.Zone {
	t.Helper()
	model, err := BuildCommitted(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestCompilerActualPrecedenceSelectionAndInactiveReservations(t *testing.T) {
	z := desiredZone()
	early := desiredSet("a", endpoint("web", "192.0.2.1"), endpoint("sleep", "192.0.2.8"))
	early.Spec.Endpoints[1].Kudurru = &public.EndpointExt{Disabled: true}
	late := desiredSet("b", endpoint("web", "192.0.2.2"), endpoint("sleep", "192.0.2.9"), endpoint("other", "192.0.2.3"), endpoint("private", "192.0.2.6"))
	late.Spec.Endpoints[3].Kudurru = &public.EndpointExt{View: "public"}
	variant := desiredSet("c", endpoint("web", "192.0.2.4"))
	variant.Spec.Endpoints[0].SetIdentifier = "other"
	variant.Spec.Endpoints[0].RecordTTL = 60
	z.Spec.RecordSetSelector = &public.RecordSetSelector{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"serve": "yes"}}}
	early.Labels = map[string]string{"serve": "yes"}
	late.Labels = early.Labels
	variant.Labels = early.Labels
	excluded := desiredSet("excluded", endpoint("excluded", "192.0.2.5"))
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{late, variant, excluded, early}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	v := compile(t, c, blank(z), in, fixedNow)
	model := built(t, v)
	answer := model.Lookup("web.example.", dns.TypeA).Answer
	if len(answer) != 2 || answer[0].Header().Ttl != 60 {
		t.Fatalf("oldest/setIdentifier precedence %v", answer)
	}
	for _, name := range []string{"sleep.example.", "private.example.", "excluded.example."} {
		if built(t, v).Lookup(name, dns.TypeA).Kind != zone.NXDomain {
			t.Fatalf("inactive leak %s", name)
		}
	}
	if len(v.Aggregate.Reservations) != 2 {
		t.Fatalf("inactive reservations %v", v.Aggregate.Reservations)
	}
	token, _ := SourceToken(excluded)
	target := zonestate.DesiredTarget{Zone: v.Head.Spec.Zone, Source: token, Operation: "apply"}
	materializer, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if err := materializer.Load(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if got := materializer.Desired(target); got.Outcome != "withheld" || !got.Published {
		t.Fatalf("selector ack %+v", got)
	}
	// A zone-wide selector change replaces every affected static fragment once.
	z.Spec.RecordSetSelector = nil
	z.ResourceVersion = "selector-change"
	newer := compile(t, c, v, in, fixedNow.Add(time.Second))
	if newer.Head.Spec.Serial == v.Head.Spec.Serial || built(t, newer).Lookup("excluded.example.", dns.TypeA).Kind != zone.Success {
		t.Fatal("selector replacement")
	}
}

func TestCompilerPreservesDynamicAndAcknowledgesOpaqueEquivalentRevisions(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("source", endpoint("static", "192.0.2.1"))
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	v := compile(t, c, blank(z), in, fixedNow)
	rr, _ := dns.NewRR("dynamic.example. 45 IN TYPE65200 \\# 3 AbCdEF")
	r, err := zonestate.FromRR(rr, zonestate.Provenance{Key: "key.", CredentialVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	a := *v.Aggregate
	a.Records = append(append([]zonestate.Record(nil), a.Records...), r)
	for i, record := range a.Records {
		rr, _, _ := dns.UnpackRR(record.Wire, 0)
		if soa, ok := rr.(*dns.SOA); ok {
			soa.Serial++
			a.Records[i], _ = zonestate.FromRR(soa, record.Provenance)
		}
	}
	v = prepareView(t, &a, v.Head)
	original, _ := SourceToken(rs)
	rs.ResourceVersion = "aaa-not-ordered"
	rs.Status.ObservedGeneration = 987
	same := compile(t, c, v, in, fixedNow.Add(time.Second))
	if same.Head.Spec.RootDigest != v.Head.Spec.RootDigest {
		t.Fatal("status-only churn committed new aggregate")
	}
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if err := m.Load(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	targetToken, _ := SourceToken(rs)
	target := zonestate.DesiredTarget{Zone: v.Head.Spec.Zone, Source: targetToken, Operation: "apply"}
	observed := m.Desired(target)
	if observed.Outcome != "committedEquivalent" || observed.ObservedResourceVersion != original.ResourceVersion || observed.Target.Source.ResourceVersion != rs.ResourceVersion {
		t.Fatalf("opaque RV truth %+v", observed)
	}
	in.Targets = []zonestate.DesiredTarget{target}
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
	rs.ResourceVersion = "changed"
	newer := compile(t, c, v, in, fixedNow.Add(time.Second))
	if built(t, newer).Lookup("dynamic.example.", 65200).Answer[0].(*dns.RFC3597).Rdata != "abcdef" {
		t.Fatal("lost dynamic wire")
	}
	if err := m.Load(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	if got := m.Desired(target); got.Outcome != "superseded" {
		t.Fatalf("changed intent ack %+v", got)
	}
	in.RecordSets = nil
	pruned := compile(t, c, newer, in, fixedNow.Add(2*time.Second))
	if built(t, pruned).Lookup("static.example.", dns.TypeA).Kind != zone.NXDomain || built(t, pruned).Lookup("dynamic.example.", 65200).Kind != zone.Success {
		t.Fatal("prune lost unrelated data")
	}
	if err := m.Load(context.Background(), pruned); err != nil {
		t.Fatal(err)
	}
	deletionToken, _ := SourceToken(rs)
	deletion := zonestate.DesiredTarget{Zone: pruned.Head.Spec.Zone, Source: deletionToken, Operation: "delete"}
	if got := m.Desired(deletion); got.Outcome != "committed" {
		t.Fatalf("delete receipt %+v", got)
	}
}

func TestCompilerDynamicCollisionRequiresExplicitTakeoverAndExpiryKeepsReservation(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("source", endpoint("lease", "192.0.2.1"))
	expires := metav1.NewTime(fixedNow.Add(time.Minute))
	rs.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &expires}
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	v := compile(t, c, blank(z), in, fixedNow)
	if !v.Aggregate.Expiry.Equal(expires.Time) {
		t.Fatal("committed deadline lost")
	}
	expired := compile(t, c, v, in, expires.Time)
	if !expired.Aggregate.Expiry.IsZero() || expired.Head.Spec.Serial == v.Head.Spec.Serial || len(expired.Aggregate.Reservations) != 1 || built(t, expired).Lookup("lease.example.", dns.TypeA).Kind != zone.NXDomain {
		t.Fatal("expiry removal/serial/reservation")
	}
	rr, _ := dns.NewRR("new.example. 60 IN A 192.0.2.9")
	r, _ := zonestate.FromRR(rr, zonestate.Provenance{Key: "key.", CredentialVersion: "v1"})
	a := *expired.Aggregate
	a.Records = append(append([]zonestate.Record(nil), a.Records...), r)
	for i, r := range a.Records {
		rr, _, _ := dns.UnpackRR(r.Wire, 0)
		if soa, ok := rr.(*dns.SOA); ok {
			soa.Serial++
			a.Records[i], _ = zonestate.FromRR(soa, r.Provenance)
		}
	}
	dynamic := prepareView(t, &a, expired.Head)
	in.RecordSets = append(in.RecordSets, desiredSet("collision", endpoint("new", "192.0.2.3")))
	if _, err := c.Project(context.Background(), dynamic, in, expires.Time); !errors.Is(err, zonestate.ErrConflict) {
		t.Fatalf("ownership conflict %v", err)
	}
	takeover := NewCompiler(CompilerOptions{Takeover: func(context.Context, *zonestate.View, *zonestate.DesiredInputs) (bool, error) { return true, nil }})
	taken := compile(t, takeover, dynamic, in, expires.Time)
	if built(t, taken).Lookup("new.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.3" {
		t.Fatal("audited takeover")
	}
	rs.Spec.Endpoints[0].Targets = []string{"invalid"}
	if _, err := c.Project(context.Background(), taken, in, expires.Time); !errors.Is(err, zonestate.ErrInvalid) {
		t.Fatalf("invalid replacement pruned: %v", err)
	}
}

func TestIntentDigestRelevantMetadataAndConfig(t *testing.T) {
	z := desiredZone()
	first, _ := IntentDigest(z)
	z.ResourceVersion = "status"
	z.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	z.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "status"}}
	same, _ := IntentDigest(z)
	if first != same {
		t.Fatal("irrelevant metadata digest")
	}
	z.Labels = map[string]string{"selection": "changed"}
	changed, _ := IntentDigest(z)
	if changed == same {
		t.Fatal("label intent ignored")
	}
	if data, err := json.Marshal(z); err != nil || len(data) == 0 {
		t.Fatal(err)
	}
}

func TestCompilerPolicyVersionBarrier(t *testing.T) {
	z := desiredZone()
	in := &zonestate.DesiredInputs{Zone: z, Complete: true, Policy: &zonestate.Policy{Enabled: true, AuthEpoch: 1, Credentials: []dynamicupdate.Credential{{Key: "key.", Version: "v1"}}, Grants: []dynamicupdate.Grant{{Key: "key.", Owner: "*.example."}}}}
	c := NewCompiler(CompilerOptions{})
	v := compile(t, c, blank(z), in, fixedNow)
	in.Policy.Credentials[0].Version = "v2"
	next := compile(t, c, v, in, fixedNow)
	if next.Head.Spec.AuthEpoch != v.Head.Spec.AuthEpoch+1 || next.Head.Spec.Serial != v.Head.Spec.Serial || next.Aggregate.Policy.Credentials[0].Version != "v2" {
		t.Fatal("policy-only credential fence")
	}
}

func TestCompilerInitialSerialUsesConfirmedContent(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("source", endpoint("host", "192.0.2.1"))
	old, errs := zone.Build(z, []*public.DNSRecordSet{rs}, zone.BuildOptions{Now: fixedNow})
	if !errs.Empty() {
		t.Fatal(errs)
	}
	z.Status.ContentHash = old.ContentHash()
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	same := compile(t, c, blank(z), in, fixedNow)
	if same.Head.Spec.Serial != z.Status.Serial {
		t.Fatal("confirmed migration serial advanced")
	}
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
	changed := compile(t, c, blank(z), in, fixedNow)
	if !dynamicupdate.RFC1982Greater(changed.Head.Spec.Serial, z.Status.Serial) {
		t.Fatal("changed migration data reused serial")
	}
}
