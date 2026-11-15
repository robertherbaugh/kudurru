package v1alpha1

import (
	"bytes"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func sourceFixtures() (*IntegrationSourceHead, *IntegrationSourceChunk, *IntegrationNameAllocation) {
	owner := Owner{ClusterID: "cluster", Namespace: "default", Name: "unifi", UID: "original"}
	data := []byte(`{"complete":true}`)
	d := Digest(data)
	chunk := &IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: SourceChunkName(owner, 1, 0, d)}, Spec: IntegrationSourceChunkSpec{FormatVersion: 1, Owner: owner, Sequence: 1, Digest: d, Data: data}}
	head := &IntegrationSourceHead{ObjectMeta: metav1.ObjectMeta{Name: SourceHeadName(owner)}, Spec: IntegrationSourceHeadSpec{FormatVersion: 1, Owner: owner, Sequence: 1, ConfigDigest: d, CredentialDigest: d, RootDigest: d, AcceptedAt: time.Unix(100, 0).UTC(), State: "active", Chunks: []SourceChunkRef{{Name: chunk.Name, Digest: d, Bytes: len(data)}}}}
	scope := AllocationScope{ClusterID: "cluster", ZoneUID: "zone-original", Apex: "home.test."}
	allocation := &IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: NameAllocationName(scope, "phone-2")}, Spec: IntegrationNameAllocationSpec{FormatVersion: 1, Scope: scope, Label: "phone-2", Base: "phone", Owner: owner, ClientID: "mac", CreatedAt: head.Spec.AcceptedAt}}
	return head, chunk, allocation
}

func TestSourceCarriersValidationAndCopies(t *testing.T) {
	h, c, a := sourceFixtures()
	for _, v := range []interface{ Validate() error }{h, c, a} {
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	hc, cc, ac := h.DeepCopy(), c.DeepCopy(), a.DeepCopy()
	hc.Spec.Chunks[0].Digest = "changed"
	cc.Spec.Data[0] = '!'
	ac.Spec.Owner.UID = "changed"
	if h.Spec.Chunks[0].Digest == "changed" || bytes.Equal(c.Spec.Data, cc.Spec.Data) || a.Spec.Owner.UID == "changed" {
		t.Fatal("copy aliases input")
	}
	s := runtime.NewScheme()
	if err := AddSourceToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range SourceKinds {
		if _, err := s.New(public.GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
	if SourceHeadName(a.Spec.Owner) == SourceHeadName(ac.Spec.Owner) {
		t.Fatal("recreated UID matched")
	}
	scope := a.Spec.Scope
	scope.ZoneUID = "new"
	if NameAllocationName(scope, a.Spec.Label) == a.Name {
		t.Fatal("recreated zone matched")
	}
}
func TestSourceCarriersRejectMalformed(t *testing.T) {
	for name, mutate := range map[string]func(*IntegrationSourceHead){
		"sequence":  func(h *IntegrationSourceHead) { h.Spec.Sequence = 0 },
		"version":   func(h *IntegrationSourceHead) { h.Spec.FormatVersion = 2 },
		"scope":     func(h *IntegrationSourceHead) { h.Namespace = "default" },
		"digest":    func(h *IntegrationSourceHead) { h.Spec.RootDigest = "bogus" },
		"order":     func(h *IntegrationSourceHead) { h.Spec.Chunks = append(h.Spec.Chunks, h.Spec.Chunks[0]) },
		"count":     func(h *IntegrationSourceHead) { h.Spec.Chunks = make([]SourceChunkRef, MaxSourceChunks+1) },
		"lifecycle": func(h *IntegrationSourceHead) { h.Spec.State = "retained" },
	} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := sourceFixtures()
			mutate(h)
			if h.Validate() == nil {
				t.Fatal("accepted invalid head")
			}
		})
	}
	_, c, a := sourceFixtures()
	c.Spec.Data[0] = '!'
	if c.Validate() == nil {
		t.Fatal("accepted tampered bytes")
	}
	_, c, _ = sourceFixtures()
	c.Spec.Data = make([]byte, MaxSourceChunkBytes+1)
	c.Spec.Digest = Digest(c.Spec.Data)
	c.Name = SourceChunkName(c.Spec.Owner, 1, 0, c.Spec.Digest)
	if c.Validate() == nil {
		t.Fatal("accepted oversized chunk")
	}
	for _, apex := range []string{".", "", "Home.test.", "*.test.", "bad\\032.test.", "_srv.test."} {
		b := a.DeepCopy()
		b.Spec.Scope.Apex = apex
		b.Name = NameAllocationName(b.Spec.Scope, b.Spec.Label)
		if b.Validate() == nil {
			t.Errorf("accepted apex %q", apex)
		}
	}
}

func TestSourceAllocationContractAndListCopy(t *testing.T) {
	_, _, a := sourceFixtures()
	for _, label := range []string{"unrelated", "phone-1", "phone-02"} {
		b := a.DeepCopy()
		b.Spec.Label = label
		b.Name = NameAllocationName(b.Spec.Scope, label)
		if b.Validate() == nil {
			t.Errorf("accepted undocumented label %s", label)
		}
	}
	h, c, _ := sourceFixtures()
	heads := &IntegrationSourceHeadList{Items: []IntegrationSourceHead{*h}}
	hc := heads.DeepCopyObject().(*IntegrationSourceHeadList)
	hc.Items[0].Spec.Chunks[0].Name = "changed"
	if heads.Items[0].Spec.Chunks[0].Name == "changed" {
		t.Fatal("head list aliases")
	}
	chunks := &IntegrationSourceChunkList{Items: []IntegrationSourceChunk{*c}}
	cc := chunks.DeepCopyObject().(*IntegrationSourceChunkList)
	cc.Items[0].Spec.Data[0] = '!'
	if chunks.Items[0].Spec.Data[0] == '!' {
		t.Fatal("chunk list aliases")
	}
	b := h.DeepCopy()
	b.Annotations = map[string]string{"oversized": string(make([]byte, MaxObjectBytes))}
	if b.Validate() == nil {
		t.Fatal("object encoding bound bypassed")
	}
}
