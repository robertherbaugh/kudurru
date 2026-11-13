package zone

import (
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// externalSet is a record set as the Kubernetes source adapter writes it.
func externalSet(name, uid string, eps ...v1alpha1.Endpoint) *v1alpha1.DNSRecordSet {
	rs := &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps",
			Labels:          map[string]string{ExternalSourceLabel: "ingress"},
			Annotations:     map[string]string{ExternalSourceUIDAnnotation: uid, ExternalSourceNameAnnotation: "web", v1alpha1.ManagedByAnnotation: ExternalSourceManager(uid)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Name: "web", UID: types.UID(uid)}},
		},
		Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "ext"}, Endpoints: eps},
	}
	return rs
}

// An external zone is built like a primary zone, but only from the record
// sets the source adapter maintains: an API-, file- or kubectl-written set
// that names it (or a copy of an adapter set under another manager or owner)
// is not selected (D402).
func TestExternalZoneServesOnlyAdapterRecordSets(t *testing.T) {
	dz := dnsZone("ext.example.", func(dz *v1alpha1.DNSZone) {
		dz.Name = "ext"
		dz.Spec.Type = v1alpha1.ZoneTypeExternal
		dz.Spec.External = &v1alpha1.ExternalSpec{Ingress: true}
		dz.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllow
	})
	owned := externalSet("external-ingress-a", "uid-a", ep("web.ext.example", "A", "192.0.2.10"))
	api := externalSet("api", "uid-b", ep("api.ext.example", "A", "192.0.2.11"))
	api.Annotations[v1alpha1.ManagedByAnnotation] = "api"
	wrongOwner := externalSet("owner", "uid-c", ep("owner.ext.example", "A", "192.0.2.12"))
	wrongOwner.OwnerReferences[0].UID = "someone-else"
	wrongKind := externalSet("kind", "uid-d", ep("kind.ext.example", "A", "192.0.2.13"))
	wrongKind.OwnerReferences[0].Kind = "Service"
	unlabeled := externalSet("unlabeled", "uid-e", ep("unlabeled.ext.example", "A", "192.0.2.14"))
	delete(unlabeled.Labels, ExternalSourceLabel)

	sets := ServedRecordSets(dz, []*v1alpha1.DNSRecordSet{owned, api, wrongOwner, wrongKind, unlabeled})
	if len(sets) != 1 || sets[0] != owned {
		t.Fatalf("selected %d record sets, want only the adapter's", len(sets))
	}
	z, errs := Build(dz, sets, BuildOptions{Now: testNow})
	if z == nil || !errs.Empty() {
		t.Fatalf("external zone did not build: %v", errs)
	}
	if z.Type() != v1alpha1.ZoneTypeExternal || z.SOA() == nil {
		t.Fatalf("type %s, SOA %v", z.Type(), z.SOA())
	}
	if res := z.Lookup("web.ext.example.", dns.TypeA); res.Kind != Success || len(res.Answer) != 1 {
		t.Fatalf("owned record not served: %+v", res)
	}
	for _, name := range []string{"api.ext.example.", "owner.ext.example.", "kind.ext.example.", "unlabeled.ext.example."} {
		if res := z.Lookup(name, dns.TypeA); len(res.Answer) != 0 {
			t.Fatalf("%s served from a record set the adapter does not own", name)
		}
	}
	// The same foreign sets are served by a primary zone: the rule is the
	// external zone's alone.
	primary := dz.DeepCopy()
	primary.Spec.Type, primary.Spec.External = v1alpha1.ZoneTypePrimary, nil
	if got := ServedRecordSets(primary, []*v1alpha1.DNSRecordSet{owned, api}); len(got) != 2 {
		t.Fatalf("primary zone selected %d record sets", len(got))
	}
	// External zones never transfer.
	if z.TransferAllowed(netip.MustParseAddr("127.0.0.1"), "") {
		t.Fatal("external zone allows transfers")
	}
}

// The complete serving image (SQLite) cannot carry an external zone: the
// store refuses it before it is stored (ServingErrors) and the image
// builder refuses it as well.
func TestExternalZoneIsNotACompleteImageZone(t *testing.T) {
	dz := dnsZone("ext.example.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeExternal })
	if errs := ServingErrors(dz); len(errs) != 1 || errs[0].Field != "spec.type" {
		t.Fatalf("ServingErrors = %v", errs)
	}
	if _, _, err := BuildNonprimary(dz, nil, time.Time{}); err == nil {
		t.Fatal("BuildNonprimary accepted an external zone")
	}
}
