package plan

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
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
