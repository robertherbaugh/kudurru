package plan

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func fixture() Input {
	return Input{Owner: kinds.Owner{ClusterID: "cluster", Namespace: "dns", Name: "public", UID: "uid-1"}, Destination: kinds.Destination{Provider: "test", Account: "account", Zone: "example.com"}, ConfigDigest: kinds.Digest([]byte("config")), CredentialDigest: kinds.Digest([]byte("credential")), SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true}
}
func endpoint(name, typ string, targets ...string) public.Endpoint {
	return public.Endpoint{DNSName: name, RecordType: typ, Targets: targets, RecordTTL: 300}
}
func owned(in Input, e public.Endpoint, id string) RemoteRecord {
	return RemoteRecord{Endpoint: e, IDs: []string{id}, Version: "v1", Owner: RemoteOwner{ClusterID: in.Owner.ClusterID, IntegrationUID: in.Owner.UID}}
}
func build(t testing.TB, in Input) *Plan {
	t.Helper()
	p, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestPlanDeterminismAndPrivateCopies(t *testing.T) {
	in := fixture()
	in.Desired = []public.Endpoint{endpoint("A.EXAMPLE.COM", "a", "192.0.2.2", "192.0.2.1"), endpoint("c.example.com.", "CNAME", "TARGET.EXAMPLE.COM"), endpoint("a.example.com.", "A", "192.0.2.3")}
	in.Current = []RemoteRecord{owned(in, endpoint("old.example.com.", "A", "192.0.2.10"), "id-old"), owned(in, endpoint("a.example.com.", "A", "192.0.2.9"), "id-a")}
	original, _ := json.Marshal(in)
	p := build(t, in)
	slices.Reverse(in.Desired)
	slices.Reverse(in.Current)
	q := build(t, in)
	if !reflect.DeepEqual(p, q) {
		t.Fatalf("non-deterministic plans: %+v / %+v", p, q)
	}
	if len(p.Operations) != 3 || p.Operations[0].Action != "delete" || p.Operations[1].Action != "update" || p.Operations[2].Action != "create" {
		t.Fatalf("operations=%+v", p.Operations)
	}
	if !slices.Equal(p.Operations[1].After.Targets, []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}) {
		t.Fatal(p.Operations[1].After)
	}
	for i := range p.Operations {
		o := p.Intent(i, time.Unix(100, 0))
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
		if o.Spec.After != nil {
			o.Spec.After.Targets[0] = "mutated"
		}
	}
	p.Operations[1].After.Targets[0] = "mutated"
	slices.Reverse(in.Desired)
	slices.Reverse(in.Current)
	after, _ := json.Marshal(in)
	if string(after) != string(original) {
		t.Fatal("planner changed caller data")
	}
	if p.Intent(-1, time.Now()) != nil || p.Intent(4, time.Now()) != nil {
		t.Fatal("invalid intent index")
	}
}
func TestPreviewBindsObservations(t *testing.T) {
	baseline := fixture()
	baseline.Desired = []public.Endpoint{endpoint("a.example.com.", "A", "192.0.2.1")}
	baseline.Current = []RemoteRecord{owned(baseline, baseline.Desired[0], "id")}
	base := build(t, baseline).Digest
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"config", func(i *Input) { i.ConfigDigest = kinds.Digest([]byte("new")) }},
		{"credential", func(i *Input) { i.CredentialDigest = kinds.Digest([]byte("new")) }},
		{"source", func(i *Input) { i.SourceDigest = kinds.Digest([]byte("new")) }},
		{"version", func(i *Input) { i.Current[0].Version = "v2" }},
		{"id", func(i *Input) { i.Current[0].IDs = []string{"new"} }},
		{"content", func(i *Input) { i.Desired[0].Targets = []string{"192.0.2.2"} }},
		{"ownership", func(i *Input) { i.Current[0].Owner.IntegrationUID = "other" }},
		{"policy", func(i *Input) { i.Policy = PolicyCreateOnly }},
		{"ledger", func(i *Input) {
			id := kinds.RecordIdentity{Owner: i.Owner, Destination: i.Destination, Key: key(i.Desired[0])}
			i.Ledger = []*kinds.IntegrationLedger{{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(id), ResourceVersion: "rv-1"}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			b, _ := json.Marshal(baseline)
			if err := json.Unmarshal(b, &in); err != nil {
				t.Fatal(err)
			}
			tc.change(&in)
			if build(t, in).Digest == base {
				t.Fatal("stale preview digest retained")
			}
		})
	}
	in := baseline
	id := kinds.RecordIdentity{Owner: in.Owner, Destination: in.Destination, Key: key(in.Desired[0])}
	l := &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(id), ResourceVersion: "rv-a"}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1}}
	in.Ledger = []*kinds.IntegrationLedger{l}
	a := build(t, in).Digest
	l.ResourceVersion = "rv-b"
	if build(t, in).Digest == a {
		t.Fatal("ledger CAS not bound")
	}
}
func TestPolicies(t *testing.T) {
	for _, tc := range []struct {
		policy Policy
		want   []string
	}{{PolicySync, []string{"delete", "update", "create"}}, {PolicyUpsertOnly, []string{"update", "create"}}, {PolicyCreateOnly, []string{"create"}}} {
		t.Run(string(tc.policy), func(t *testing.T) {
			in := fixture()
			in.Policy = tc.policy
			in.Desired = []public.Endpoint{endpoint("new.example.com.", "A", "192.0.2.1"), endpoint("changed.example.com.", "A", "192.0.2.2")}
			in.Current = []RemoteRecord{owned(in, endpoint("old.example.com.", "A", "192.0.2.1"), "old"), owned(in, endpoint("changed.example.com.", "A", "192.0.2.1"), "changed")}
			p := build(t, in)
			var got []string
			for _, o := range p.Operations {
				got = append(got, o.Action)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}
func TestOwnershipAndAdoption(t *testing.T) {
	for _, tc := range []struct {
		name     string
		owner    RemoteOwner
		adopt    bool
		external string
		want     string
		conflict bool
	}{
		{name: "same UID", owner: RemoteOwner{ClusterID: "cluster", IntegrationUID: "uid-1"}, want: "update"},
		{name: "recreated UID", owner: RemoteOwner{ClusterID: "cluster", IntegrationUID: "old-uid"}, adopt: true, conflict: true},
		{name: "foreign cluster", owner: RemoteOwner{ClusterID: "foreign", IntegrationUID: "uid-1"}, adopt: true, conflict: true},
		{name: "unowned", conflict: true},
		{name: "adopt unowned", adopt: true, want: "adopt"},
		{name: "external legacy", owner: RemoteOwner{ExternalDNSOwner: "legacy"}, external: "legacy", want: "adopt"},
		{name: "wrong external", owner: RemoteOwner{ExternalDNSOwner: "other"}, external: "legacy", adopt: true, conflict: true},
		{name: "cluster-only marker", owner: RemoteOwner{ClusterID: "cluster"}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			in.AdoptUnowned = tc.adopt
			in.AdoptExternalDNSOwner = tc.external
			in.Desired = []public.Endpoint{endpoint("a.example.com.", "A", "192.0.2.2")}
			in.Current = []RemoteRecord{{Endpoint: endpoint("a.example.com.", "A", "192.0.2.1"), IDs: []string{"id"}, Owner: tc.owner}}
			p := build(t, in)
			if (len(p.Conflicts) > 0) != tc.conflict {
				t.Fatalf("conflicts=%v", p.Conflicts)
			}
			if tc.want != "" && (len(p.Operations) != 1 || p.Operations[0].Action != tc.want) {
				t.Fatalf("operations=%v", p.Operations)
			}
			if tc.conflict && len(p.Operations) != 0 {
				t.Fatal("modified foreign record")
			}
		})
	}
	in := fixture()
	in.Desired = []public.Endpoint{endpoint("a.example.com.", "A", "192.0.2.2")}
	in.Current = []RemoteRecord{{Endpoint: endpoint("a.example.com.", "A", "192.0.2.1"), IDs: []string{"id"}, Owner: RemoteOwner{ClusterID: "cluster"}}}
	id := kinds.RecordIdentity{Owner: in.Owner, Destination: in.Destination, Key: key(in.Desired[0])}
	in.Ledger = []*kinds.IntegrationLedger{{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(id), ResourceVersion: "1"}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1, ContentHash: kinds.EndpointDigest(&in.Current[0].Endpoint), ProviderIDs: []string{"id"}}}}
	p := build(t, in)
	if len(p.Operations) != 1 || p.Operations[0].Action != "update" {
		t.Fatal("matching legacy ledger IDs did not prove ownership")
	}
	in.Ledger[0].Spec.ProviderIDs = []string{"different"}
	if len(build(t, in).Operations) != 0 {
		t.Fatal("wrong ID proves ownership")
	}
}
func TestCNAMEAcrossTypesAndSets(t *testing.T) {
	in := fixture()
	a := endpoint("a.example.com.", "A", "192.0.2.1")
	a.SetIdentifier = "blue"
	c := endpoint("a.example.com.", "CNAME", "target.example.com.")
	c.SetIdentifier = "red"
	in.Desired = []public.Endpoint{a, c}
	in.Current = []RemoteRecord{owned(in, a, "id")}
	p := build(t, in)
	if len(p.Operations) != 0 || len(p.Conflicts) == 0 {
		t.Fatal("coexistence changed remote state")
	}
	in.Desired = []public.Endpoint{c}
	p = build(t, in)
	if len(p.Operations) != 2 || p.Operations[0].Action != "delete" || p.Operations[1].Action != "create" {
		t.Fatal("sync cannot transition CNAME", p)
	}
	in.Policy = PolicyUpsertOnly
	p = build(t, in)
	if len(p.Operations) != 0 || len(p.Conflicts) == 0 {
		t.Fatal("upsert allowed CNAME without deletion")
	}
	in.Policy = PolicySync
	in.Current[0].Owner.IntegrationUID = "other"
	if len(build(t, in).Operations) != 0 {
		t.Fatal("CNAME overwritten foreign other type")
	}
	in.Desired = nil
	in.Current = append(in.Current, owned(in, endpoint("a.example.com.", "TXT", "\"owned\""), "txt"))
	if len(build(t, in).Operations) != 0 {
		t.Fatal("deleted at contested owner")
	}
}
func TestInvalidAndIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		want   error
	}{
		{"desired incomplete", func(i *Input) { i.DesiredComplete = false }, ErrIncomplete},
		{"current incomplete", func(i *Input) { i.CurrentComplete = false }, ErrIncomplete},
		{"owner", func(i *Input) { i.Owner.UID = "" }, ErrInvalid},
		{"pseudo RR", func(i *Input) { i.Desired[0].RecordType = "OPT" }, ErrInvalid},
		{"bad target", func(i *Input) { i.Desired[0].Targets = []string{"not-an-ip"} }, ErrInvalid},
		{"conflicting TTL", func(i *Input) { e := i.Desired[0]; e.RecordTTL++; i.Desired = append(i.Desired, e) }, ErrInvalid},
		{"multiple cname", func(i *Input) { i.Desired[0] = endpoint("a.example.com.", "CNAME", "one.", "two.") }, ErrInvalid},
		{"record bound", func(i *Input) {
			i.MaxRecords = 1
			i.Desired = append(i.Desired, endpoint("b.example.com.", "A", "192.0.2.1"))
		}, ErrCapacity},
		{"duplicate provider ID", func(i *Input) {
			i.Current = []RemoteRecord{owned(*i, i.Desired[0], "id"), owned(*i, endpoint("b.example.com.", "A", "192.0.2.1"), "id")}
		}, ErrInvalid},
		{"missing provider ID", func(i *Input) { i.Current = []RemoteRecord{{Endpoint: i.Desired[0]}} }, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			in.Desired = []public.Endpoint{endpoint("a.example.com.", "A", "192.0.2.1")}
			tc.mutate(&in)
			if _, err := Build(in); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	if n, err := Normalize(endpoint("a.example.com.", "TYPE65280", "\\# 2 ABCD")); err != nil || n.Targets[0] != "\\# 2 abcd" {
		t.Fatalf("unknown RR preservation: %+v %v", n, err)
	}
}
func BenchmarkBuild1000(b *testing.B) {
	in := fixture()
	for i := 0; i < 1000; i++ {
		e := endpoint("host"+time.Unix(int64(i), 0).Format("150405")+".example.com.", "A", "192.0.2.1")
		in.Desired = append(in.Desired, e)
		in.Current = append(in.Current, owned(in, e, e.DNSName))
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Build(in); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMixedPoliciesAndRemovedSourceRetention(t *testing.T) {
	in := fixture()
	e := endpoint("retained.example.com.", "A", "192.0.2.1")
	id := kinds.RecordIdentity{Owner: in.Owner, Destination: in.Destination, Key: key(e)}
	in.Current = []RemoteRecord{owned(in, e, "retained")}
	in.Ledger = []*kinds.IntegrationLedger{{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(id), ResourceVersion: "rv-1"}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1, ContentHash: kinds.EndpointDigest(&e), ProviderIDs: []string{"retained"}, Policy: "upsert-only"}}}
	in.Desired = []public.Endpoint{endpoint("created.example.com.", "A", "192.0.2.2")}
	p := build(t, in)
	if len(p.Operations) != 1 || p.Operations[0].Action != "create" {
		t.Fatal("sync deleted a removed upsert-only source", p.Operations)
	}
	in.RecordPolicies = []RecordPolicy{{key(e), PolicySync}}
	p = build(t, in)
	if len(p.Operations) != 2 || p.Operations[0].Action != "delete" {
		t.Fatal("current policy override not honored")
	}
	before := p.Digest
	in.RecordPolicies = append(in.RecordPolicies, RecordPolicy{key(in.Desired[0]), PolicyCreateOnly})
	p = build(t, in)
	if p.Digest == before {
		t.Fatal("policy override not in preview")
	}
	o := p.Intent(1, time.Unix(100, 0))
	if o.Spec.Policy != "create-only" {
		t.Fatal("intent omitted effective policy")
	}
	// A retained policy preventing removal must also prevent CNAME conflicts.
	in.RecordPolicies = nil
	in.Desired = []public.Endpoint{endpoint(e.DNSName, "CNAME", "target.example.com.")}
	p = build(t, in)
	if len(p.Operations) != 0 || len(p.Conflicts) == 0 {
		t.Fatal("alias created over retained A")
	}
}

func TestPendingLedgerAndWireEncodingFailClosed(t *testing.T) {
	in := fixture()
	e := endpoint("a.example.com.", "A", "192.0.2.1")
	in.Desired = []public.Endpoint{e}
	id := kinds.RecordIdentity{Owner: in.Owner, Destination: in.Destination, Key: key(e)}
	in.Ledger = []*kinds.IntegrationLedger{{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(id), ResourceVersion: "rv"}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1, Pending: &kinds.Pending{OperationID: kinds.Digest([]byte("pending")), PlanDigest: kinds.Digest([]byte("old plan"))}}}}
	if _, err := Build(in); !errors.Is(err, ErrIncomplete) {
		t.Fatal("pending recovery produced a new plan", err)
	}
	// Invalid DNSKEY material must not reach a provider mutation.
	if _, err := Normalize(endpoint("a.example.com.", "DNSKEY", "257 3 8 invalid!")); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid DNSKEY accepted", err)
	}
}

func ledgerRow(in Input, e public.Endpoint, policy string, ids ...string) *kinds.IntegrationLedger {
	id := kinds.RecordIdentity{Owner: in.Owner, Destination: in.Destination, Key: key(e)}
	return &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(id), ResourceVersion: "rv-" + e.DNSName + e.RecordType}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1, ContentHash: kinds.EndpointDigest(&e), ProviderIDs: ids, Policy: policy}}
}
func mustNormalize(t testing.TB, e public.Endpoint) public.Endpoint {
	t.Helper()
	n, err := Normalize(e)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An ordinary plan's digest and operation IDs are durable: operation IDs name
// stored intents and a preview digest authorizes a reviewed sync. Adding the
// cleanup mode (D367) must not change them. The values were recorded from the
// planner before the mode existed.
func TestOrdinaryPlanDigestUnchangedByCleanupMode(t *testing.T) {
	in := fixture()
	e := endpoint("retained.example.com.", "A", "192.0.2.1")
	in.Current = []RemoteRecord{owned(in, e, "retained"), owned(in, endpoint("old.example.com.", "A", "192.0.2.10"), "id-old"), {Endpoint: endpoint("foreign.example.com.", "TXT", "\"x\""), IDs: []string{"f"}}}
	row := ledgerRow(in, e, "upsert-only", "retained")
	row.ResourceVersion = "rv-1"
	in.Ledger = []*kinds.IntegrationLedger{row}
	in.Desired = []public.Endpoint{endpoint("created.example.com.", "A", "192.0.2.2")}
	p := build(t, in)
	if p.Digest != "743c2351dcdf6294d76601f6500051582f0ab23aee2504b638b46ddef458924f" {
		t.Fatalf("ordinary plan digest changed: %s", p.Digest)
	}
	if len(p.Operations) != 2 || p.Operations[0].ID != "4e38e8b30f94d0a463ad08b37884f629ae66f55f6b6b1619751d7677a650ebb0" || p.Operations[1].ID != "4f4cdf9bbb55827057b2c51aa495087f69215023b2d05db406a5a96fa608261e" {
		t.Fatalf("ordinary operation IDs changed: %+v", p.Operations)
	}
	if p.Cleanup || p.Retained != nil {
		t.Fatal("ordinary plan reports cleanup state")
	}
}

// cleanupFixture is one destination holding every ownership class next to
// each other: proven rows under each retained policy, a legacy marker proven
// by the ledger, markers the ledger does not prove, and rows of others.
func cleanupFixture(t testing.TB) (Input, map[string]public.Endpoint) {
	t.Helper()
	in := fixture()
	in.Cleanup = true
	e := map[string]public.Endpoint{
		"sync":       endpoint("sync.example.com.", "A", "192.0.2.1"),
		"upsert":     endpoint("upsert.example.com.", "A", "192.0.2.2"),
		"createonly": endpoint("createonly.example.com.", "A", "192.0.2.3"),
		"legacy":     endpoint("legacy.example.com.", "A", "192.0.2.4"),
		"multi":      endpoint("multi.example.com.", "A", "192.0.2.5", "192.0.2.6"),
		"cloned":     endpoint("cloned.example.com.", "A", "192.0.2.7"),
		"extra":      endpoint("extra.example.com.", "A", "192.0.2.8"),
		"noledger":   endpoint("noledger.example.com.", "TXT", "\"marker only\""),
		"recreated":  endpoint("recreated.example.com.", "A", "192.0.2.9"),
		"foreign":    endpoint("foreign.example.com.", "A", "198.51.100.1"),
		"unowned":    endpoint("unowned.example.com.", "A", "198.51.100.2"),
		"heritage":   endpoint("heritage.example.com.", "A", "198.51.100.3"),
		"heritageT":  endpoint("heritage.example.com.", "TXT", "\"heritage=external-dns,external-dns/owner=legacy\""),
		"contested":  endpoint("contested.example.com.", "A", "192.0.2.10"),
		"contestedT": endpoint("contested.example.com.", "TXT", "\"someone else\""),
	}
	multiA, multiB := endpoint("multi.example.com.", "A", "192.0.2.5"), endpoint("multi.example.com.", "A", "192.0.2.6")
	extraOurs, extraClone := e["extra"], endpoint("extra.example.com.", "A", "192.0.2.80")
	in.Current = []RemoteRecord{
		owned(in, e["sync"], "id-sync"),
		owned(in, e["upsert"], "id-upsert"),
		owned(in, e["createonly"], "id-createonly"),
		{Endpoint: e["legacy"], IDs: []string{"id-legacy"}, Owner: RemoteOwner{ClusterID: in.Owner.ClusterID}},
		owned(in, multiA, "id-multi-a"), owned(in, multiB, "id-multi-b"),
		// Our marker on an ID the ledger never recorded for this RRset.
		owned(in, e["cloned"], "id-cloned-copy"),
		// One proven row and one copied row in the same RRset.
		owned(in, extraOurs, "id-extra"), owned(in, extraClone, "id-extra-copy"),
		// Our marker and no ledger row at all.
		owned(in, e["noledger"], "id-noledger"),
		// A previous incarnation of the same name, another cluster, nobody, and
		// an external-dns pair.
		{Endpoint: e["recreated"], IDs: []string{"id-recreated"}, Owner: RemoteOwner{ClusterID: in.Owner.ClusterID, IntegrationUID: "uid-0"}},
		{Endpoint: e["foreign"], IDs: []string{"id-foreign"}, Owner: RemoteOwner{ClusterID: "other-cluster", IntegrationUID: in.Owner.UID}},
		{Endpoint: e["unowned"], IDs: []string{"id-unowned"}},
		{Endpoint: e["heritage"], IDs: []string{"id-heritage"}, Owner: RemoteOwner{ExternalDNSOwner: "legacy"}},
		{Endpoint: e["heritageT"], IDs: []string{"id-heritage-txt"}, Owner: RemoteOwner{ExternalDNSOwner: "legacy"}},
		// A proven row whose name someone else also uses.
		owned(in, e["contested"], "id-contested"),
		{Endpoint: e["contestedT"], IDs: []string{"id-contested-txt"}},
	}
	in.Ledger = []*kinds.IntegrationLedger{
		ledgerRow(in, e["sync"], "sync", "id-sync"),
		ledgerRow(in, e["upsert"], "upsert-only", "id-upsert"),
		ledgerRow(in, e["createonly"], "create-only", "id-createonly"),
		ledgerRow(in, e["legacy"], "sync", "id-legacy"),
		ledgerRow(in, mustNormalize(t, e["multi"]), "sync", "id-multi-a", "id-multi-b"),
		ledgerRow(in, e["cloned"], "sync", "id-cloned-original"),
		ledgerRow(in, e["extra"], "sync", "id-extra"),
		ledgerRow(in, e["contested"], "sync", "id-contested"),
	}
	return in, e
}

func TestCleanupDeletesOnlyMarkerAndLedgerProvenRows(t *testing.T) {
	in, e := cleanupFixture(t)
	original, _ := json.Marshal(in)
	p := build(t, in)
	if !p.Cleanup || p.Unchanged != 0 {
		t.Fatalf("cleanup=%v unchanged=%d", p.Cleanup, p.Unchanged)
	}
	deleted := map[string][]string{}
	for i, o := range p.Operations {
		if o.Action != "delete" || o.Policy != PolicySync || o.After != nil || o.Before == nil {
			t.Fatalf("cleanup planned %s under %q: %+v", o.Action, o.Policy, o)
		}
		deleted[o.Key.Name+" "+o.Key.Type] = o.ProviderIDs
		intent := p.Intent(i, time.Unix(100, 0))
		if err := intent.Validate(); err != nil || intent.Spec.Policy != "sync" || intent.Spec.Action != "delete" {
			t.Fatalf("cleanup intent: %v %+v", err, intent.Spec)
		}
	}
	want := map[string][]string{
		"sync.example.com. A":       {"id-sync"},
		"upsert.example.com. A":     {"id-upsert"},
		"createonly.example.com. A": {"id-createonly"},
		"legacy.example.com. A":     {"id-legacy"},
		"multi.example.com. A":      {"id-multi-a", "id-multi-b"},
	}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted=%v\n   want=%v", deleted, want)
	}
	retained := map[string]string{}
	for _, r := range p.Retained {
		retained[r.Key.Name+" "+r.Key.Type] = r.Reason
	}
	if !reflect.DeepEqual(retained, map[string]string{
		"cloned.example.com. A":     RetainedUnproven,
		"extra.example.com. A":      RetainedUnproven,
		"noledger.example.com. TXT": RetainedUnproven,
		"contested.example.com. A":  RetainedConflict,
	}) {
		t.Fatalf("retained=%v", retained)
	}
	// Rows of others are reported as conflicts exactly as in ordinary planning
	// and appear in no operation and no retained entry.
	conflicts := map[string]bool{}
	for _, c := range p.Conflicts {
		conflicts[c.Name+" "+c.Type] = true
	}
	for _, name := range []string{"recreated", "foreign", "unowned", "heritage", "heritageT", "contestedT"} {
		k := key(mustNormalize(t, e[name]))
		if !conflicts[k.Name+" "+k.Type] || deleted[k.Name+" "+k.Type] != nil || retained[k.Name+" "+k.Type] != "" {
			t.Fatalf("%s: conflict=%v deleted=%v retained=%q", name, conflicts[k.Name+" "+k.Type], deleted[k.Name+" "+k.Type], retained[k.Name+" "+k.Type])
		}
	}
	for _, o := range p.Operations {
		for _, id := range o.ProviderIDs {
			if !slices.Contains([]string{"id-sync", "id-upsert", "id-createonly", "id-legacy", "id-multi-a", "id-multi-b"}, id) {
				t.Fatalf("cleanup would delete provider ID %q", id)
			}
		}
	}
	// Reordered observations give the same plan, digest and operation IDs.
	slices.Reverse(in.Current)
	slices.Reverse(in.Ledger)
	q := build(t, in)
	if !reflect.DeepEqual(p, q) {
		t.Fatalf("non-deterministic cleanup plans:\n%+v\n%+v", p, q)
	}
	slices.Reverse(in.Current)
	slices.Reverse(in.Ledger)
	if after, _ := json.Marshal(in); string(after) != string(original) {
		t.Fatal("planner changed caller data")
	}
}

func TestCleanupOverridesRetainedPolicyOnlyInCleanupMode(t *testing.T) {
	in, _ := cleanupFixture(t)
	cleanup := build(t, in)
	// The same observations as an ordinary plan with nothing desired: the
	// ledger's retained upsert-only and create-only policies still protect
	// their rows, and a marker alone is still ownership.
	in.Cleanup = false
	ordinary := build(t, in)
	got := map[string]bool{}
	for _, o := range ordinary.Operations {
		if o.Action != "delete" {
			t.Fatalf("ordinary plan with nothing desired planned %s", o.Action)
		}
		got[o.Key.Name] = true
	}
	if got["upsert.example.com."] || got["createonly.example.com."] {
		t.Fatalf("ordinary plan ignored a retained policy: %v", got)
	}
	if !got["sync.example.com."] || !got["cloned.example.com."] || !got["noledger.example.com."] {
		t.Fatalf("ordinary ownership changed: %v", got)
	}
	if ordinary.Cleanup || ordinary.Retained != nil {
		t.Fatal("ordinary plan reports cleanup state")
	}
	if ordinary.Digest == cleanup.Digest {
		t.Fatal("the cleanup flag is not part of the plan digest")
	}
	ids := map[string]bool{}
	for _, o := range ordinary.Operations {
		ids[o.ID] = true
	}
	for _, o := range cleanup.Operations {
		if ids[o.ID] {
			t.Fatalf("cleanup and ordinary plans share operation ID %s for %s", o.ID, o.Key.Name)
		}
	}
	// A destination-wide restriction is overridden too, and only in cleanup:
	// ordinarily it protects every row without a retained ledger policy.
	for _, policy := range []Policy{PolicyUpsertOnly, PolicyCreateOnly} {
		in.Policy = policy
		for _, o := range build(t, in).Operations {
			if slices.Contains([]string{"noledger.example.com.", "upsert.example.com.", "createonly.example.com."}, o.Key.Name) {
				t.Fatalf("ordinary %s plan deleted %s", policy, o.Key.Name)
			}
		}
		in.Cleanup = true
		restricted := build(t, in)
		if !reflect.DeepEqual(restricted, cleanup) {
			t.Fatalf("cleanup under destination policy %s differs: %d operations", policy, len(restricted.Operations))
		}
		in.Cleanup = false
	}
}

// The cleanup flag itself is part of the plan digest (D367). The observations
// here make both modes plan the very same operation: one marker-and-ledger
// proven row under policy sync with nothing desired, nothing retained. Only
// the flag differs, so a digest or operation ID shared between the two would
// let an ordinary preview, or its stored intent, stand in for a cleanup.
func TestCleanupFlagAloneChangesDigestAndOperationIDs(t *testing.T) {
	in := fixture()
	in.Policy = PolicySync
	e := endpoint("only.example.com.", "A", "192.0.2.1")
	in.Current = []RemoteRecord{owned(in, e, "id-only")}
	in.Ledger = []*kinds.IntegrationLedger{ledgerRow(in, e, "sync", "id-only")}
	ordinary := build(t, in)
	in.Cleanup = true
	cleanup := build(t, in)
	if !cleanup.Cleanup || ordinary.Cleanup || len(cleanup.Retained) != 0 || len(cleanup.Conflicts) != 0 {
		t.Fatalf("fixture: ordinary=%+v cleanup=%+v", ordinary, cleanup)
	}
	// Apart from the flag, the digest and the operation ID derived from it,
	// the two plans are the same plan.
	if len(ordinary.Operations) != 1 || len(cleanup.Operations) != 1 {
		t.Fatalf("fixture: %d ordinary and %d cleanup operations, want one delete each", len(ordinary.Operations), len(cleanup.Operations))
	}
	same := *cleanup
	same.Cleanup, same.Digest = false, ordinary.Digest
	same.Operations = slices.Clone(cleanup.Operations)
	same.Operations[0].ID = ordinary.Operations[0].ID
	if ordinary.Operations[0].Action != "delete" || !reflect.DeepEqual(&same, ordinary) {
		t.Fatalf("fixture: the two modes plan different work:\nordinary %+v\n cleanup %+v", ordinary, cleanup)
	}
	if ordinary.Digest == cleanup.Digest {
		t.Fatalf("the cleanup flag is not part of the plan digest: both %s", ordinary.Digest)
	}
	if ordinary.Operations[0].ID == cleanup.Operations[0].ID {
		t.Fatalf("cleanup and ordinary plans share operation ID %s", cleanup.Operations[0].ID)
	}
	if ordinary.Intent(0, time.Unix(1, 0)).Name == cleanup.Intent(0, time.Unix(1, 0)).Name {
		t.Fatal("cleanup and ordinary intents of the same delete share a durable name")
	}
}

func TestCleanupConflictsMixedSetsAndAdoption(t *testing.T) {
	in := fixture()
	in.Cleanup = true
	a := endpoint("shared.example.com.", "A", "192.0.2.1")
	b := endpoint("shared.example.com.", "A", "192.0.2.2")
	txt := endpoint("shared.example.com.", "TXT", "\"ours\"")
	in.Current = []RemoteRecord{owned(in, a, "id-a"), {Endpoint: b, IDs: []string{"id-b"}}, owned(in, txt, "id-txt")}
	in.Ledger = []*kinds.IntegrationLedger{ledgerRow(in, a, "sync", "id-a"), ledgerRow(in, txt, "sync", "id-txt")}
	// One RRset holds our row and an unowned row: the name is a conflict, and
	// neither it nor our sibling TXT at that name is touched.
	p := build(t, in)
	if len(p.Operations) != 0 || len(p.Conflicts) != 1 {
		t.Fatalf("operations=%v conflicts=%v", p.Operations, p.Conflicts)
	}
	if !reflect.DeepEqual(p.Retained, []Retained{{key(a), RetainedConflict}, {key(txt), RetainedConflict}}) {
		t.Fatalf("retained=%+v", p.Retained)
	}
	// Configured adoption keeps its meaning for classification (the unowned
	// row is no conflict), but a cleanup never adopts or deletes it, and it
	// still cannot delete a record set it only partly owns.
	in.AdoptUnowned = true
	p = build(t, in)
	if len(p.Conflicts) != 0 || len(p.Operations) != 1 || p.Operations[0].Key != key(txt) || !slices.Equal(p.Operations[0].ProviderIDs, []string{"id-txt"}) {
		t.Fatalf("operations=%+v conflicts=%v", p.Operations, p.Conflicts)
	}
	if !reflect.DeepEqual(p.Retained, []Retained{{key(a), RetainedMixed}}) {
		t.Fatalf("retained=%+v", p.Retained)
	}
	// A destination with nothing of ours is clean: no operations, nothing
	// retained, whatever else lives there.
	in.AdoptUnowned = false
	in.Current = []RemoteRecord{{Endpoint: b, IDs: []string{"id-b"}}}
	in.Ledger = nil
	p = build(t, in)
	if len(p.Operations) != 0 || p.Retained != nil || len(p.Conflicts) != 1 {
		t.Fatalf("clean destination: %+v", p)
	}
}

func TestCleanupRefusesDesiredStatePendingRowsAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		want   error
	}{
		{"desired records", func(i *Input) { i.Desired = []public.Endpoint{endpoint("new.example.com.", "A", "192.0.2.1")} }, ErrInvalid},
		{"record policies", func(i *Input) {
			i.RecordPolicies = []RecordPolicy{{key(endpoint("sync.example.com.", "A", "192.0.2.1")), PolicyUpsertOnly}}
		}, ErrInvalid},
		{"invalid destination policy", func(i *Input) { i.Policy = "erase" }, ErrInvalid},
		{"incomplete remote snapshot", func(i *Input) { i.CurrentComplete = false }, ErrIncomplete},
		{"incomplete desired capture", func(i *Input) { i.DesiredComplete = false }, ErrIncomplete},
		{"pending ledger row", func(i *Input) {
			i.Ledger[0].Spec.Pending = &kinds.Pending{OperationID: kinds.Digest([]byte("pending")), PlanDigest: kinds.Digest([]byte("plan")), BeforeHash: i.Ledger[0].Spec.ContentHash}
		}, ErrIncomplete},
		{"ledger of another destination", func(i *Input) { i.Ledger[0].Spec.Record.Destination.Zone = "other.example" }, ErrInvalid},
		{"record bound", func(i *Input) { i.MaxRecords = 3 }, ErrCapacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, _ := cleanupFixture(t)
			tc.mutate(&in)
			p, err := Build(in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if p != nil {
				t.Fatal("a refused cleanup produced a partial plan")
			}
		})
	}
	// One durable operation names at most kinds.MaxTargets provider IDs, and
	// so does the ledger row that proves them. A record set with more rows
	// under this owner's marker is therefore never proven: it is retained and
	// reported, not deleted and not a reason to plan nothing else.
	in, _ := cleanupFixture(t)
	wide := endpoint("wide.example.com.", "A", "192.0.2.200")
	var ids []string
	for n := range kinds.MaxTargets + 1 {
		id := "id-wide-" + strconv.Itoa(n)
		ids = append(ids, id)
		in.Current = append(in.Current, owned(in, wide, id))
	}
	in.Ledger = append(in.Ledger, ledgerRow(in, wide, "sync", ids[:kinds.MaxTargets]...))
	p := build(t, in)
	for _, o := range p.Operations {
		if o.Key.Name == "wide.example.com." || len(o.ProviderIDs) > kinds.MaxTargets {
			t.Fatalf("cleanup planned an unprovable delete of %d IDs at %s", len(o.ProviderIDs), o.Key.Name)
		}
	}
	if len(p.Operations) != 5 || !slices.Contains(p.Retained, Retained{key(wide), RetainedUnproven}) {
		t.Fatalf("operations=%d retained=%+v", len(p.Operations), p.Retained)
	}
	// The same rows under an ordinary plan exceed the operation bound.
	in.Cleanup = false
	if _, err := Build(in); !errors.Is(err, ErrCapacity) {
		t.Fatalf("ordinary plan over the operation bound: %v", err)
	}
}
