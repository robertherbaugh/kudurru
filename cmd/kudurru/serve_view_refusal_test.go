package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunServeSQLiteReadPartialViewRefusal(t *testing.T) {
	node := startZoneChangeNode(t, nil)
	const zoneName = "refusal.test."
	node.call(http.MethodPost, "/objects/dnszones", &public.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: "refusal"},
		Spec:       public.DNSZoneSpec{ZoneName: zoneName, Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + zoneName}},
	}, http.StatusCreated, nil)
	putSet := func(namespace, name string, endpoints ...public.Endpoint) {
		t.Helper()
		node.call(http.MethodPut, "/objects/dnsrecordsets/"+namespace+"/"+name, &public.DNSRecordSet{
			TypeMeta:   metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"},
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec:       public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "refusal"}, Endpoints: endpoints},
		}, http.StatusCreated, nil)
	}
	putSet("default", "owner", public.Endpoint{DNSName: "printer." + zoneName, RecordType: "A", Targets: []string{"192.0.2.1"}})
	putSet("kids", "mixed",
		public.Endpoint{DNSName: "kept." + zoneName, RecordType: "A", Targets: []string{"192.0.2.2"}},
		public.Endpoint{DNSName: "printer." + zoneName, RecordType: "A", Targets: []string{"192.0.2.3"}, Kudurru: &public.EndpointExt{View: "lan"}})
	const conditionType = "ViewEndpointsRefused"
	const mixedPath = "/objects/dnsrecordsets/kids/mixed"
	var mixed public.DNSRecordSet
	for deadline := time.Now().Add(20 * time.Second); ; {
		mixed = public.DNSRecordSet{}
		node.call(http.MethodGet, mixedPath, nil, http.StatusOK, &mixed)
		condition := meta.FindStatusCondition(mixed.Status.Conditions, conditionType)
		if condition != nil {
			if condition.Status != metav1.ConditionTrue || len(condition.Message) > 512 ||
				!strings.Contains(condition.Message, "printer.refusal.test. A (view kids/lan): Duplicated") {
				t.Fatalf("unexpected refusal condition: %+v", condition)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refusal missing from record set status: %+v", mixed.Status)
		}
		time.Sleep(30 * time.Millisecond)
	}
	var owner public.DNSRecordSet
	node.call(http.MethodGet, "/objects/dnsrecordsets/default/owner", nil, http.StatusOK, &owner)
	if condition := meta.FindStatusCondition(owner.Status.Conditions, conditionType); condition != nil {
		t.Fatalf("refusal attached to wrong record set: %+v", condition)
	}
	var rows api.ZoneRecordList
	node.call(http.MethodGet, "/zones/refusal/records", nil, http.StatusOK, &rows)
	if rows.Source != api.RecordSourceServed {
		t.Fatalf("record list source: %s", rows.Source)
	}
	kept := false
	for _, row := range rows.Items {
		if row.Kudurru != nil && row.Kudurru.View == "lan" {
			t.Fatalf("refused endpoint in served list: %+v", row)
		}
		kept = kept || row.DNSName == "kept.refusal.test" && row.RecordSet == "kids/mixed"
	}
	if !kept {
		t.Fatalf("unrelated endpoint lost from the served list: %+v", rows.Items)
	}
	mixed.Spec.Endpoints = mixed.Spec.Endpoints[:1]
	mixed.Status = public.DNSRecordSetStatus{}
	node.call(http.MethodPut, mixedPath, &mixed, http.StatusOK, nil)
	var replaced public.DNSRecordSet
	node.call(http.MethodGet, mixedPath, nil, http.StatusOK, &replaced)
	if len(replaced.Spec.Endpoints) != 1 || meta.FindStatusCondition(replaced.Status.Conditions, conditionType) != nil {
		t.Fatalf("replacement intent kept stale refusal evidence: %+v", replaced)
	}
}
