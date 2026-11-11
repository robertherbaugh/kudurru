package primaryzone

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	// A replacement that makes the record set invalid as a whole is
	// withheld and reported, as zone.ServedRecordSets leaves it out, while
	// the rest of the zone is committed: refusing the whole projection
	// would freeze every later change of the zone (D373).
	rs.Spec.Endpoints[0].Targets = []string{"invalid"}
	result, err := c.Project(context.Background(), taken, in, expires.Time)
	if err != nil {
		t.Fatalf("an invalid replacement kept the zone from its commit: %v", err)
	}
	for _, d := range result.Diagnostics {
		if d.SourceID == store.KeyOf(rs).String() && (d.Outcome != "withheld" || !strings.HasPrefix(d.Reason, public.ReasonInvalid+": ")) {
			t.Fatalf("invalid replacement reported %+v", d)
		}
	}
	replaced := prepareView(t, result.Aggregate, taken.Head)
	if built(t, replaced).Lookup("new.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.3" {
		t.Fatal("the rest of the zone was not committed with the invalid replacement")
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

func TestCompilerPreservesPrimaryWithoutConfiguredNameservers(t *testing.T) {
	z := desiredZone()
	z.Spec.Nameservers = nil
	in := &zonestate.DesiredInputs{Zone: z, Complete: true, RecordSets: []*public.DNSRecordSet{desiredSet("web", endpoint("web", "192.0.2.9"))}}
	c := NewCompiler(CompilerOptions{})
	v := compile(t, c, blank(z), in, fixedNow)
	model := built(t, v)
	if model.Lookup("example.", dns.TypeSOA).Kind != zone.Success || model.Lookup("example.", dns.TypeNS).Kind != zone.NoData || model.Lookup("web.example.", dns.TypeA).Kind != zone.Success {
		t.Fatal("nameserver-free primary lost SOA, NODATA or ordinary records")
	}
	// The same committed bytes must survive durable encode/decode and restore.
	restored := prepareView(t, v.Aggregate, v.Head)
	if built(t, restored).Lookup("web.example.", dns.TypeA).Kind != zone.Success {
		t.Fatal("restored nameserver-free primary unavailable")
	}
	// Configured nameservers still require committed NS records, never a
	// synthetic replacement in the materializer or an unchecked incomplete cut.
	z.Spec.Nameservers = []string{"ns.example."}
	config, err := json.Marshal(z)
	if err != nil {
		t.Fatal(err)
	}
	v.Aggregate.Config = config
	if _, err := zonestate.Prepare(v.Aggregate, v.Head, zonestate.Limits{}); !errors.Is(err, zonestate.ErrInvalid) {
		t.Fatal("missing configured NS accepted by codec", err)
	}
	if _, err := BuildCommitted(t.Context(), v); err == nil {
		t.Fatal("missing configured NS accepted by materializer")
	}
}

// An endpoint the zone builder refuses (a CNAME next to other data, a CNAME
// or NS at the apex) is left out of the commit and reported on its record
// set, as zone.Build does. 0.1.0 stored such record sets and served the rest
// of the zone; the first commit after an upgrade has no committed head to
// fall back on, so refusing the whole projection would never serve the zone
// (D373).
func TestCompilerWithholdsEndpointsTheBuilderRefuses(t *testing.T) {
	z := desiredZone()
	ok := desiredSet("ok", endpoint("www", "192.0.2.10"))
	ok.CreationTimestamp = metav1.NewTime(fixedNow.Add(-time.Hour))
	cname := desiredSet("cname-and-a", public.Endpoint{DNSName: "www.example.", RecordType: "CNAME", Targets: []string{"other.example."}}, endpoint("kept", "192.0.2.11"))
	apexCNAME := desiredSet("apex-cname", public.Endpoint{DNSName: "example.", RecordType: "CNAME", Targets: []string{"other.example."}})
	apexNS := desiredSet("apex-ns", public.Endpoint{DNSName: "example.", RecordType: "NS", Targets: []string{"ns2.example."}})
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{ok, cname, apexCNAME, apexNS}, Complete: true}
	result, err := NewCompiler(CompilerOptions{}).Project(context.Background(), blank(z), in, fixedNow)
	if err != nil {
		t.Fatal("a refused endpoint kept the zone from its commit:", err)
	}
	outcomes := map[string]zonestate.SourceDiagnostic{}
	for _, d := range result.Diagnostics {
		outcomes[d.SourceID] = d
	}
	for name, want := range map[string]string{"ok": "served", "cname-and-a": "served", "apex-cname": "withheld", "apex-ns": "withheld"} {
		d := outcomes[store.KeyOf(desiredSet(name)).String()]
		if d.Outcome != want {
			t.Errorf("%s: outcome %q (%q), want %q", name, d.Outcome, d.Reason, want)
		}
		if want == "withheld" && (len(d.Reason) == 0 || len(d.Reason) > maxWithheldReason) {
			t.Errorf("%s: reason %q", name, d.Reason)
		}
	}
	model := built(t, prepareView(t, result.Aggregate, nil))
	if a := model.Lookup("www.example.", dns.TypeA).Answer; len(a) != 1 || a[0].(*dns.A).A.String() != "192.0.2.10" {
		t.Fatalf("www.example. A: %v", a)
	}
	if a := model.Lookup("kept.example.", dns.TypeA).Answer; len(a) != 1 {
		t.Fatalf("the accepted endpoint of the record set was not served: %v", a)
	}
	if a := model.Lookup("example.", dns.TypeCNAME).Answer; len(a) != 0 {
		t.Fatalf("an apex CNAME was served: %v", a)
	}
	if a := model.Lookup("example.", dns.TypeNS).Answer; len(a) != 1 || a[0].(*dns.NS).Ns != "ns.example." {
		t.Fatalf("apex NS: %v", a)
	}
}

func TestWithheldReasonIsBounded(t *testing.T) {
	long := withheldReason("Invalid", strings.Repeat("é", 600))
	if len(long) > maxWithheldReason || !utf8.ValidString(long) || !strings.HasSuffix(long, "...") {
		t.Fatalf("%d bytes %q", len(long), long)
	}
	if got := withheldReason("Invalid", "short"); got != "Invalid: short" {
		t.Fatal(got)
	}
}

// Every way the zone builder can refuse a record set or one of its
// endpoints, after an upgrade (no committed head) and on a running node (a
// committed head): the refused data is left out and reported on its record
// set with a reason, the rest of the zone is committed and served, and a
// later change is committed too (D373). The table follows the refusals in
// their source order: RecordSetError and ParseEndpoint against the root
// (the record set is invalid as a whole and not selected), ParseEndpoint
// against the zone origin (outside the zone), and Builder.addParsed (apex
// NS and CNAME, Duplicated, CNAME next to other data, Occluded). Each case
// is also refused by zone.Build or zone.ServedRecordSets, the 0.1.0 path,
// so the table holds builder refusals only.
func TestCompilerWithholdsEveryRefusalOfTheBuilder(t *testing.T) {
	a := func(name string, targets ...string) public.Endpoint {
		return public.Endpoint{DNSName: name, RecordType: "A", Targets: targets}
	}
	typed := func(name, typ string, targets ...string) public.Endpoint {
		return public.Endpoint{DNSName: name, RecordType: typ, Targets: targets}
	}
	ttl := func(ep public.Endpoint, ttl int64) public.Endpoint { ep.RecordTTL = ttl; return ep }
	cases := []struct {
		name string
		eps  []public.Endpoint
		// outcome is the projection: withheld, or duplicated for a name an
		// earlier record set holds.
		outcome string
		// gone is a name and type that must not be served.
		gone  string
		gtype uint16
	}{
		// The record set is invalid as a whole (RecordSetError).
		{"owner-empty", []public.Endpoint{a("", "192.0.2.50")}, "withheld", "", 0},
		{"owner-whitespace", []public.Endpoint{a("bad name.example.", "192.0.2.50")}, "withheld", "", 0},
		{"owner-long-label", []public.Endpoint{a(strings.Repeat("x", 64)+".example.", "192.0.2.50")}, "withheld", "", 0},
		{"owner-dollar", []public.Endpoint{a("$dollar.example.", "192.0.2.50")}, "withheld", "", 0},
		{"type-unknown", []public.Endpoint{typed("bogus.example.", "BOGUS", "x")}, "withheld", "", 0},
		{"type-number-zero", []public.Endpoint{typed("t0.example.", "TYPE0", `\# 0`)}, "withheld", "", 0},
		{"type-forbidden", []public.Endpoint{typed("soa.example.", "TYPE6", `\# 0`)}, "withheld", "", 0},
		{"ttl-negative", []public.Endpoint{ttl(a("ttl.example.", "192.0.2.50"), -1)}, "withheld", "ttl.example.", dns.TypeA},
		{"ttl-too-large", []public.Endpoint{ttl(a("ttl2.example.", "192.0.2.50"), 1<<32)}, "withheld", "ttl2.example.", dns.TypeA},
		{"no-targets", []public.Endpoint{a("empty.example.")}, "withheld", "empty.example.", dns.TypeA},
		{"cname-two-targets", []public.Endpoint{typed("alias.example.", "CNAME", "a.example.", "b.example.")}, "withheld", "alias.example.", dns.TypeCNAME},
		{"dname-two-targets", []public.Endpoint{typed("tree.example.", "DNAME", "a.example.", "b.example.")}, "withheld", "tree.example.", dns.TypeDNAME},
		{"target-empty", []public.Endpoint{a("blank.example.", " ")}, "withheld", "blank.example.", dns.TypeA},
		{"target-two-lines", []public.Endpoint{typed("lines.example.", "TXT", "one\ntwo")}, "withheld", "lines.example.", dns.TypeTXT},
		{"a-not-an-address", []public.Endpoint{a("notip.example.", "not-an-ip"), a("notip-ok.example.", "192.0.2.51")}, "withheld", "notip-ok.example.", dns.TypeA},
		{"aaaa-ipv4", []public.Endpoint{typed("six.example.", "AAAA", "192.0.2.52")}, "withheld", "six.example.", dns.TypeAAAA},
		{"mx-without-preference", []public.Endpoint{typed("mail.example.", "MX", "mx.example.")}, "withheld", "mail.example.", dns.TypeMX},
		{"cname-root", []public.Endpoint{typed("root-alias.example.", "CNAME", ".")}, "withheld", "root-alias.example.", dns.TypeCNAME},
		{"dname-root", []public.Endpoint{typed("root-tree.example.", "DNAME", ".")}, "withheld", "root-tree.example.", dns.TypeDNAME},
		{"ns-root", []public.Endpoint{typed("deleg.example.", "NS", ".")}, "withheld", "deleg.example.", dns.TypeNS},
		{"ptr-root", []public.Endpoint{typed("ptr.example.", "PTR", ".")}, "withheld", "ptr.example.", dns.TypePTR},
		{"no-presentation-form", []public.Endpoint{typed("lp.example.", "TYPE107", `\# 2 0000`)}, "withheld", "lp.example.", 107},
		{"two-a-one-name", []public.Endpoint{a("twice.example.", "192.0.2.53"), a("twice.example.", "192.0.2.54")}, "withheld", "twice.example.", dns.TypeA},
		{"same-endpoint-twice", []public.Endpoint{a("same.example.", "192.0.2.55"), a("same.example.", "192.0.2.55")}, "withheld", "same.example.", dns.TypeA},
		{"two-cnames-one-name", []public.Endpoint{typed("cn.example.", "CNAME", "a.example."), typed("cn.example.", "CNAME", "b.example.")}, "withheld", "cn.example.", dns.TypeCNAME},
		// Valid in the root, refused in this zone (ParseEndpoint at the origin).
		{"outside-zone", []public.Endpoint{a("www.elsewhere.", "192.0.2.56")}, "withheld", "", 0},
		// Builder.addParsed.
		{"apex-ns", []public.Endpoint{typed("example.", "NS", "ns2.example.")}, "withheld", "", 0},
		{"apex-cname", []public.Endpoint{typed("example.", "CNAME", "other.example.")}, "withheld", "example.", dns.TypeCNAME},
		{"identity-of-an-earlier-set", []public.Endpoint{a("www.example.", "192.0.2.57")}, "duplicated", "", 0},
		{"cname-next-to-an-earlier-a", []public.Endpoint{typed("www.example.", "CNAME", "other.example.")}, "duplicated", "www.example.", dns.TypeCNAME},
		{"cname-and-a-in-one-set", []public.Endpoint{typed("pair.example.", "CNAME", "other.example."), a("pair.example.", "192.0.2.58")}, "served", "pair.example.", dns.TypeA},
		{"a-and-cname-in-one-set", []public.Endpoint{a("pair2.example.", "192.0.2.59"), typed("pair2.example.", "CNAME", "other.example.")}, "served", "pair2.example.", dns.TypeCNAME},
		{"below-a-dname", []public.Endpoint{typed("moved.example.", "DNAME", "target.test."), a("x.moved.example.", "192.0.2.60")}, "served", "x.moved.example.", dns.TypeA},
	}
	z := desiredZone()
	ok := desiredSet("ok", endpoint("www", "192.0.2.10"))
	ok.CreationTimestamp = metav1.NewTime(fixedNow.Add(-time.Hour))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := desiredSet("bad", tc.eps...)
			// The 0.1.0 path refuses the same data.
			sets := []*public.DNSRecordSet{ok, bad}
			_, errs := zone.Build(z, zone.ServedRecordSets(z, sets), zone.BuildOptions{Now: fixedNow})
			if zone.RecordSetValid(bad) && len(errs.RecordSets["dns/bad"]) == 0 {
				t.Fatalf("zone.Build accepts the record set: the case is not a refusal")
			}
			c := NewCompiler(CompilerOptions{})
			// After an upgrade: no committed head.
			in := &zonestate.DesiredInputs{Zone: z, RecordSets: sets, Complete: true}
			result, err := c.Project(context.Background(), blank(z), in, fixedNow)
			if err != nil {
				t.Fatal("the refusal kept the zone from its first commit:", err)
			}
			var got zonestate.SourceDiagnostic
			for _, d := range result.Diagnostics {
				if d.SourceID == store.KeyOf(bad).String() {
					got = d
				}
			}
			if got.Outcome != tc.outcome || (tc.outcome != "served" && got.Reason == "") || len(got.Reason) > maxWithheldReason {
				t.Fatalf("outcome %q reason %q, want %q with a reason", got.Outcome, got.Reason, tc.outcome)
			}
			first := prepareView(t, result.Aggregate, nil)
			model := built(t, first)
			if a := model.Lookup("www.example.", dns.TypeA).Answer; len(a) != 1 || a[0].(*dns.A).A.String() != "192.0.2.10" {
				t.Fatalf("the rest of the zone is not served: www.example. A %v", a)
			}
			if tc.gone != "" {
				if l := model.Lookup(tc.gone, tc.gtype); len(l.Answer) != 0 && l.Answer[0].Header().Rrtype == tc.gtype {
					t.Fatalf("refused data served: %v", l.Answer)
				}
			}
			// A running node: the refused record set is written next to a
			// committed head, and a later change is committed with it.
			c = NewCompiler(CompilerOptions{})
			head := compile(t, c, blank(z), &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{ok}, Complete: true}, fixedNow)
			later := desiredSet("later", endpoint("later", "192.0.2.61"))
			next := compile(t, c, head, &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{ok, bad, later}, Complete: true}, fixedNow.Add(time.Second))
			if a := built(t, next).Lookup("later.example.", dns.TypeA).Answer; len(a) != 1 {
				t.Fatalf("a change next to the refused record set was not committed: %v", a)
			}
		})
	}
}
