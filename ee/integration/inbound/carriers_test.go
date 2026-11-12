package inbound

import (
	"testing"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// A Kubernetes listing (D384) is decoded with the image's cross-carrier rules,
// except that unreferenced chunks are tolerated as unretired garbage.
func TestDecodeCarriersListingRules(t *testing.T) {
	doc, _, at := sourceDocument(t)
	encoded := encodeDocument(t, doc, at)
	listing := func() CarrierListing {
		head := encoded.Head.DeepCopy()
		head.UID, head.ResourceVersion = "head-uid", "11"
		var chunks []*kinds.IntegrationSourceChunk
		for i, c := range encoded.Chunks {
			c = c.DeepCopy()
			c.UID, c.ResourceVersion = carrierUID("chunk", i), "10"
			chunks = append(chunks, c)
		}
		proof := doc.Allocations[0]
		allocation := &kinds.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: proof.Object.Name, UID: "allocation-original", ResourceVersion: "captured"}, Spec: proof.Spec}
		return CarrierListing{ClusterID: doc.Owner.ClusterID, Heads: []*kinds.IntegrationSourceHead{head}, Chunks: chunks, Allocations: []*kinds.IntegrationNameAllocation{allocation}}
	}
	decoded, err := DecodeCarriers(listing())
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sources) != 1 || len(decoded.Sources[0].Chunks) != len(encoded.Chunks) || len(decoded.Allocations) != 1 {
		t.Fatal("listing did not decode completely")
	}
	if _, err := DecodeSource(decoded.Sources[0]); err != nil {
		t.Fatal(err)
	}

	// A chunk staged by a lost head CAS references nothing and changes nothing.
	l := listing()
	orphan := encodeDocument(t, doc, at.Add(1))
	for i, c := range orphan.Chunks {
		c = c.DeepCopy()
		c.Spec.Sequence = 2
		c.Name = kinds.SourceChunkName(c.Spec.Owner, 2, c.Spec.Index, c.Spec.Digest)
		c.UID, c.ResourceVersion = carrierUID("orphan", i), "12"
		l.Chunks = append(l.Chunks, c)
	}
	if _, err := DecodeCarriers(l); err != nil {
		t.Fatal("unreferenced chunk refused a listing", err)
	}

	for name, mutate := range map[string]func(*CarrierListing){
		"missing referenced chunk": func(l *CarrierListing) { l.Chunks = l.Chunks[1:] },
		"allocation evidence":      func(l *CarrierListing) { l.Allocations[0].ResourceVersion = "other" },
		"missing allocation":       func(l *CarrierListing) { l.Allocations = nil },
		"wrong cluster":            func(l *CarrierListing) { l.ClusterID = "other-cluster" },
		"duplicate uid":            func(l *CarrierListing) { l.Chunks[0].UID = l.Heads[0].UID },
		"no server identity":       func(l *CarrierListing) { l.Heads[0].ResourceVersion = "" },
		"tampered chunk":           func(l *CarrierListing) { l.Chunks[0].Spec.Data = append([]byte(nil), "tampered"...) },
		"duplicate head":           func(l *CarrierListing) { h := l.Heads[0].DeepCopy(); h.UID = "second"; l.Heads = append(l.Heads, h) },
	} {
		l := listing()
		mutate(&l)
		if _, err := DecodeCarriers(l); err == nil {
			t.Errorf("%s: listing accepted", name)
		}
	}
}

func carrierUID(prefix string, i int) types.UID { return types.UID(prefix + "-" + string(rune('a'+i))) }
