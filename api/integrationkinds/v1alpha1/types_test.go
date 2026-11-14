package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func fixtureRecord() RecordIdentity {
	return RecordIdentity{Owner: Owner{ClusterID: "cluster", Namespace: "dns", Name: "public", UID: "one"}, Destination: Destination{Provider: "cloudflare", Account: "account", Zone: "zone"}, Key: RecordKey{Name: "www.example.", Type: "A"}}
}
func fixtureOperation() *IntegrationOperation {
	id := fixtureRecord()
	after := &public.Endpoint{DNSName: id.Key.Name, RecordType: id.Key.Type, Targets: []string{"192.0.2.1"}, RecordTTL: 60}
	spec := IntegrationOperationSpec{FormatVersion: FormatVersion, Record: id, OperationID: Digest([]byte("operation")), PlanDigest: Digest([]byte("plan")), ConfigDigest: Digest([]byte("config")), CredentialDigest: Digest([]byte("keyed fingerprint")), Action: "create", After: after, AfterHash: EndpointDigest(after), CreatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	return &IntegrationOperation{ObjectMeta: metav1.ObjectMeta{Name: OperationName(spec.OperationID)}, Spec: spec}
}

func TestPrivateKindRegistrationAndDeepCopies(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	op := fixtureOperation()
	op.Labels = map[string]string{"original": "value"}
	op.Spec.After.Labels = map[string]string{"record": "value"}
	op.Spec.After.ProviderSpecific = []public.ProviderSpecific{{Name: "cloudflare/proxied", Value: "false"}}
	op.Spec.After.Kudurru = &public.EndpointExt{ExpiresAt: &metav1.Time{Time: op.Spec.CreatedAt}}
	op.Spec.Receipt = &Receipt{ProviderIDs: []string{"id"}, Targets: []TargetResult{{Target: "192.0.2.1", ProviderID: "id"}}}
	cp := op.DeepCopy()
	cp.Labels["original"] = "changed"
	cp.Spec.After.Targets[0] = "192.0.2.2"
	cp.Spec.After.Labels["record"] = "changed"
	cp.Spec.After.ProviderSpecific[0].Value = "true"
	cp.Spec.After.Kudurru.ExpiresAt.Time = cp.Spec.CreatedAt.Add(time.Hour)
	cp.Spec.Receipt.ProviderIDs[0] = "changed"
	cp.Spec.Receipt.Targets[0].Target = "changed"
	if op.Labels["original"] != "value" || op.Spec.After.Targets[0] != "192.0.2.1" || op.Spec.After.Labels["record"] != "value" || op.Spec.After.ProviderSpecific[0].Value != "false" || !op.Spec.After.Kudurru.ExpiresAt.Time.Equal(op.Spec.CreatedAt) || op.Spec.Receipt.ProviderIDs[0] != "id" || op.Spec.Receipt.Targets[0].Target != "192.0.2.1" {
		t.Fatal("mutable private state escaped through copy")
	}
	ledger := &IntegrationLedger{Spec: IntegrationLedgerSpec{ProviderIDs: []string{"id"}, Pending: &Pending{OperationID: "original"}}}
	lc := ledger.DeepCopy()
	lc.Spec.ProviderIDs[0] = "changed"
	lc.Spec.Pending.OperationID = "changed"
	if ledger.Spec.ProviderIDs[0] != "id" || ledger.Spec.Pending.OperationID != "original" {
		t.Fatal("ledger copy aliases")
	}
	budget := &IntegrationBudget{Spec: IntegrationBudgetSpec{Reservations: []BudgetReservation{{Cost: 1}}}}
	bc := budget.DeepCopy()
	bc.Spec.Reservations[0].Cost = 2
	if budget.Spec.Reservations[0].Cost != 1 {
		t.Fatal("budget copy aliases")
	}
	delivery := &IntegrationDelivery{Spec: IntegrationDeliverySpec{Payload: json.RawMessage(`{"ok":true}`)}}
	dc := delivery.DeepCopy()
	dc.Spec.Payload[0] = '!'
	if delivery.Spec.Payload[0] != '{' {
		t.Fatal("delivery copy aliases")
	}
	for _, obj := range []runtime.Object{op, ledger, budget, delivery, &IntegrationLedgerList{Items: []IntegrationLedger{*ledger}}, &IntegrationOperationList{Items: []IntegrationOperation{*op}}, &IntegrationBudgetList{Items: []IntegrationBudget{*budget}}, &IntegrationDeliveryList{Items: []IntegrationDelivery{*delivery}}} {
		gvks, _, err := scheme.ObjectKinds(obj)
		if err != nil || len(gvks) != 1 || gvks[0].GroupVersion() != public.GroupVersion {
			t.Fatal("private kind missing from scheme", gvks, err)
		}
		if obj.DeepCopyObject() == obj {
			t.Fatal("runtime copy is original")
		}
	}
	for _, kind := range Kinds {
		if !IsInternal(kind) {
			t.Fatal("manifest omitted private kind")
		}
	}
	if IsInternal("Integration") {
		t.Fatal("public configuration misclassified as private")
	}
}

func TestPrivateOperationValidationAndReceipts(t *testing.T) {
	base := fixtureOperation()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*IntegrationOperation){
		func(o *IntegrationOperation) { o.Name = "other" }, func(o *IntegrationOperation) { o.Namespace = "dns" }, func(o *IntegrationOperation) { o.Spec.Record.Owner.UID = "" }, func(o *IntegrationOperation) { o.Spec.FormatVersion++ }, func(o *IntegrationOperation) { o.Spec.After.Targets[0] = "not an address" }, func(o *IntegrationOperation) { o.Spec.After.DNSName = "elsewhere.example." }, func(o *IntegrationOperation) { o.Spec.Record.Key.Type = "ANY" }, func(o *IntegrationOperation) { o.Spec.Action = "delete" }, func(o *IntegrationOperation) { o.Spec.CredentialDigest = "plain secret" }, func(o *IntegrationOperation) { o.Spec.ProviderIDs = []string{"dup", "dup"} }, func(o *IntegrationOperation) { o.Spec.After.Targets = make([]string, MaxTargets+1) },
	} {
		o := base.DeepCopy()
		mutate(o)
		if err := o.Validate(); err == nil {
			t.Fatal("malformed operation accepted", o)
		}
	}
	o := base.DeepCopy()
	o.Spec.Receipt = &Receipt{Complete: true, ContentHash: o.Spec.AfterHash, ProviderIDs: []string{"id"}, ObservedAt: o.Spec.CreatedAt.Add(time.Second)}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	o.Spec.Receipt.ContentHash = Digest([]byte("wrong"))
	if err := o.Validate(); err == nil {
		t.Fatal("wrong confirmation accepted")
	}
	o = base.DeepCopy()
	o.Spec.Receipt = &Receipt{Targets: []TargetResult{{Target: "192.0.2.1", ProviderID: "id", Applied: true}}, ObservedAt: o.Spec.CreatedAt.Add(time.Second), ErrorClass: "ambiguous"}
	if err := o.Validate(); err != nil {
		t.Fatal("partial result rejected", err)
	}
	before, _ := json.Marshal(base)
	o = base.DeepCopy()
	o.Spec.Action = "invalid"
	rejected, _ := json.Marshal(o)
	_ = o.Validate()
	after, _ := json.Marshal(o)
	if string(after) != string(rejected) || len(before) == 0 {
		t.Fatal("validation mutates rejected input")
	}
}

func TestPrivateNamesAndLedgerBaseline(t *testing.T) {
	id := fixtureRecord()
	other := id
	other.Owner.UID = "recreated"
	if LedgerName(id) == LedgerName(other) {
		t.Fatal("UID recreation matched old ledger")
	}
	other = id
	other.Destination.Zone = "other"
	if LedgerName(id) == LedgerName(other) {
		t.Fatal("destination collision")
	}
	l := &IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: LedgerName(id)}, Spec: IntegrationLedgerSpec{FormatVersion: 1, Record: id, Sequence: 1, Pending: &Pending{OperationID: Digest([]byte("op")), PlanDigest: Digest([]byte("plan")), AfterHash: Digest([]byte("after"))}}}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	l.Spec.Pending.BeforeHash = Digest([]byte("unexpected"))
	if err := l.Validate(); err == nil {
		t.Fatal("pending baseline mismatch accepted")
	}
	l.Spec.Pending = nil
	l.Spec.ProviderIDs = []string{"id"}
	if err := l.Validate(); err == nil {
		t.Fatal("absent record has provider IDs")
	}
	for _, typ := range []string{"OPT", "TSIG", "TKEY", "AXFR", "IXFR", "ANY", "MAILA", "MAILB", "TYPE0", "a"} {
		key := id.Key
		key.Type = typ
		if key.Validate() == nil {
			t.Fatal("pseudo/noncanonical type accepted", typ)
		}
	}
	unknown := id.Key
	unknown.Type = "TYPE65400"
	if err := unknown.Validate(); err != nil {
		t.Fatal("unknown raw type rejected", err)
	}
}

func TestBudgetAndDeliveryBoundaries(t *testing.T) {
	now := fixtureOperation().Spec.CreatedAt
	b := &IntegrationBudget{ObjectMeta: metav1.ObjectMeta{Name: BudgetName("cluster", "cloudflare", "all")}, Spec: IntegrationBudgetSpec{FormatVersion: 1, ClusterID: "cluster", Provider: "cloudflare", Account: "all", Sequence: 1, WindowStart: now, WindowSeconds: 300, Capacity: 1000, Reservations: []BudgetReservation{{ID: Digest([]byte("reservation")), Cost: 1000}}}}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	b.Spec.Reservations[0].Cost++
	if b.Validate() == nil {
		t.Fatal("overspent shared budget accepted")
	}
	o := fixtureRecord().Owner
	id := Digest([]byte("event"))
	d := &IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: DeliveryName(o, id)}, Spec: IntegrationDeliverySpec{FormatVersion: 1, Owner: o, DeliveryID: id, Topic: "record.changed", Payload: json.RawMessage(`{}`), CreatedAt: now, NextAttempt: now, Deadline: now.Add(24 * time.Hour)}}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	d.Spec.Deadline = d.Spec.Deadline.Add(time.Nanosecond)
	if d.Validate() == nil {
		t.Fatal("unbounded retry lifetime accepted")
	}
	d.Spec.Deadline = now.Add(time.Hour)
	d.Spec.Payload = json.RawMessage(strings.Repeat("x", MaxPayloadBytes+1))
	if d.Validate() == nil {
		t.Fatal("unbounded delivery accepted")
	}
}
