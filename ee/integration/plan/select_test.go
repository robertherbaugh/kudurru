package plan

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func selectionFixture() SelectionInput {
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "dns", UID: "zone-uid", ResourceVersion: "z1"}, Spec: public.DNSZoneSpec{ZoneName: "example.com."}}
	r := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "dns", UID: "record-uid", ResourceVersion: "r1"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "zone"}}}
	return SelectionInput{Owner: fixture().Owner, Sources: []Source{{z, r}}, Complete: true, Now: time.Unix(100, 0), Domains: []string{"example.com"}}
}
func TestSelectOriginalPublicAndExpiry(t *testing.T) {
	in := selectionFixture()
	now := metav1.NewTime(in.Now)
	later := metav1.NewTime(in.Now.Add(time.Second))
	r := in.Sources[0].Records
	r.Spec.Endpoints = []public.Endpoint{endpoint("public.example.com.", "A", "192.0.2.1"), endpoint("disabled.example.com.", "A", "192.0.2.2"), endpoint("expired.example.com.", "A", "192.0.2.3"), endpoint("future.example.com.", "A", "192.0.2.4")}
	r.Spec.Endpoints[0].Kudurru = &public.EndpointExt{View: "public"}
	r.Spec.Endpoints[1].Kudurru = &public.EndpointExt{Disabled: true}
	r.Spec.Endpoints[2].Kudurru = &public.EndpointExt{ExpiresAt: &now}
	r.Spec.Endpoints[3].Kudurru = &public.EndpointExt{ExpiresAt: &later}
	out, err := Select(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("records=%+v", out.Records)
	}
	if out.Records[1].Endpoint.Kudurru.View != "public" {
		t.Fatal("public-only metadata lost")
	}
	out.Records[0].Endpoint.Targets[0] = "mutated"
	if r.Spec.Endpoints[3].Targets[0] == "mutated" {
		t.Fatal("caller source changed")
	}
	in.Now = later.Time
	out, err = Select(in)
	if err != nil || len(out.Records) != 1 {
		t.Fatalf("exact expiry: %+v %v", out, err)
	}
}
func TestSelectLabelsPublishTypesAndExclusions(t *testing.T) {
	in := selectionFixture()
	in.Select = public.PublicationSelection{Labels: map[string]string{"publish": "yes"}, Types: []string{"A", "CNAME"}, Exclude: []string{"*.internal.example.com", "exact.example.com"}}
	z, r := in.Sources[0].Zone, in.Sources[0].Records
	z.Spec.Publish = []public.PublishRule{{IntegrationRef: public.ObjectRef{Name: "public"}, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"rule": "yes"}}, Types: []string{"A"}, Policy: "upsert-only"}}
	r.Spec.Endpoints = []public.Endpoint{endpoint("global.example.com.", "CNAME", "target.example.com."), endpoint("rule.example.com.", "A", "192.0.2.1"), endpoint("excluded.internal.example.com.", "A", "192.0.2.2"), endpoint("internal.example.com.", "A", "192.0.2.3"), endpoint("exact.example.com.", "A", "192.0.2.4"), endpoint("txt.example.com.", "TXT", "\"value\""), endpoint("skip.example.com.", "A", "192.0.2.5")}
	for i := 0; i < 6; i++ {
		r.Spec.Endpoints[i].Labels = map[string]string{"publish": "yes"}
	}
	r.Spec.Endpoints[1].Labels = map[string]string{"rule": "yes"}
	out, err := Select(in)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range out.Records {
		names = append(names, s.Endpoint.DNSName)
		if s.Endpoint.DNSName == "rule.example.com." && s.Policy != PolicyUpsertOnly {
			t.Fatal("rule policy lost")
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"global.example.com.", "internal.example.com.", "rule.example.com."}) {
		t.Fatal(names)
	}
	// Publication rule namespace must resolve relative to the zone, not caller.
	z.Spec.Publish[0].IntegrationRef.Namespace = "elsewhere"
	out, err = Select(in)
	if err != nil || len(out.Records) != 2 {
		t.Fatal("foreign rule selected", out, err)
	}
	// Metadata labels are available, endpoint labels override them.
	r.Labels = map[string]string{"publish": "yes"}
	r.Spec.Endpoints[6].Labels = map[string]string{"publish": "no"}
	out, err = Select(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Records {
		if s.Endpoint.DNSName == "skip.example.com." {
			t.Fatal("endpoint override ignored")
		}
	}
}
func TestSelectDeterminismAndRejection(t *testing.T) {
	in := selectionFixture()
	r := in.Sources[0].Records
	r.Spec.Endpoints = []public.Endpoint{endpoint("a.example.com.", "A", "192.0.2.1"), endpoint("b.example.com.", "A", "192.0.2.2")}
	a, err := Select(in)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(r.Spec.Endpoints)
	b, err := Select(in)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("selection reordering changed digest")
	}
	r.ResourceVersion = "r2"
	b, err = Select(in)
	if err != nil || a.Digest == b.Digest {
		t.Fatal("source token not captured")
	}
	in.Complete = false
	if _, err := Select(in); !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	in.Complete = true
	in.Select.Exclude = []string{"bad*pattern"}
	if _, err := Select(in); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	in.Select.Exclude = nil
	r.Spec.Endpoints[0].DNSName = "outside.test."
	if _, err := Select(in); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestSelectPolicyConflict(t *testing.T) {
	in := selectionFixture()
	z, r := in.Sources[0].Zone, in.Sources[0].Records
	r.Spec.Endpoints = []public.Endpoint{endpoint("a.example.com.", "A", "192.0.2.1")}
	z.Spec.Publish = []public.PublishRule{{IntegrationRef: public.ObjectRef{Name: "public"}, Policy: "sync"}, {IntegrationRef: public.ObjectRef{Name: "public"}, Policy: "upsert-only"}}
	if _, err := Select(in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("conflicting matching policies: %v", err)
	}
}
