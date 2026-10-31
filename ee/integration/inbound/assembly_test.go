package inbound

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingInput(t *testing.T) ImageInput {
	t.Helper()
	doc, group, at := sourceDocument(t)
	image := encodeDocument(t, doc, at)
	h := inventory.New(func() time.Time { return at })
	if err := h.Replace(host.SourceID(doc.Owner), 1, doc.Snapshot.Clients); err != nil {
		t.Fatal(err)
	}
	state, err := h.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	return ImageInput{ClusterID: doc.Owner.ClusterID, Sources: []SourceImage{image}, Admissions: []SourceAdmission{{Owner: doc.Owner, ConfigDigest: image.Head.Spec.ConfigDigest, RootDigest: image.Head.Spec.RootDigest, Ready: true, Targets: doc.Targets}}, Inventory: state, Groups: []*public.ClientGroup{group}, At: at}
}
func TestPreparePreservesManualAndMergesPrivateLiveMACOnly(t *testing.T) {
	in := servingInput(t)
	before, _ := json.Marshal(in)
	out, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated original public input")
	}
	g := out.Groups[0]
	if len(g.Spec.Clients) != 3 || g.Spec.Clients[0].Comment != "manual overlap" || g.Spec.Clients[1].Comment != "manual subnet" || g.Spec.Clients[2].MAC != "02:00:00:00:00:02" {
		t.Fatal("lost manual membership/order or failed union", g.Spec.Clients)
	}
	g.Spec.Clients[0].Comment = "changed"
	if in.Groups[0].Spec.Clients[0].Comment == "changed" {
		t.Fatal("group copy aliases input")
	}
	restored, err := inventory.RestoreState(out.Inventory, func() time.Time { return in.At })
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().Lookup(netip.MustParseAddr("192.0.2.2"), in.At) == nil {
		t.Fatal("lost live authorized inventory")
	}
}
func TestPrepareMissingChangedOrPendingSourceClosesOnlyItsAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "pending", "config", "uid", "target UID", "target RV", "partial", "retained", "withdrawn", "missing source image"} {
		t.Run(mode, func(t *testing.T) {
			in := servingInput(t)
			originalDesired, _ := DecodeSource(in.Sources[0])
			wantSource := true
			switch mode {
			case "missing":
				in.Admissions = nil
			case "pending":
				in.Admissions[0].Ready = false
			case "config":
				in.Admissions[0].ConfigDigest = kinds.Digest([]byte("new config"))
			case "uid":
				in.Admissions[0].Owner.UID = "recreated"
			case "target UID":
				in.Admissions[0].Targets[0].Object.UID = "recreated"
			case "target RV":
				in.Admissions[0].Targets[0].Object.ResourceVersion = "other"
			case "partial":
				in.Admissions[0].Targets = nil
			case "retained", "withdrawn":
				in.Sources[0].Head.Spec.State = mode
				in.Sources[0].Head.Spec.WithdrawnAt = in.At
			case "missing source image":
				in.Sources = nil
				wantSource = false
			}
			out, err := Prepare(in)
			if err != nil {
				t.Fatal(err)
			}
			if wantSource && out.Sources[0].Eligible {
				t.Fatal("old source still eligible")
			}
			h, err := inventory.RestoreState(out.Inventory, func() time.Time { return in.At })
			if err != nil {
				t.Fatal(err)
			}
			if h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.2"), in.At) != nil {
				t.Fatal("old live authority leaked")
			}
			if len(out.Groups[0].Spec.Clients) != 2 || out.Groups[0].Spec.Clients[0].Comment != "manual overlap" {
				t.Fatal("withdrawal changed manual data")
			}
			if wantSource {
				original, _ := json.Marshal(originalDesired.Snapshot.Desired)
				kept, _ := json.Marshal(out.Sources[0].Document.Snapshot.Desired)
				if !bytes.Equal(original, kept) {
					t.Fatal("finite retained DNS desired/evidence changed")
				}
			}
		})
	}
}
func TestPrepareGroupBindingChangeAndExactExpiry(t *testing.T) {
	for _, mode := range []string{"UID", "ref", "unbound", "expiry", "newer assignment"} {
		t.Run(mode, func(t *testing.T) {
			in := servingInput(t)
			switch mode {
			case "UID":
				in.Groups[0].UID = "recreated"
			case "ref":
				in.Groups[0].Spec.Sources.IntegrationRef.Name = "other"
			case "unbound":
				in.Groups[0].Spec.Sources = nil
			case "expiry":
				in.At = in.At.Add(time.Hour)
			case "newer assignment":
				newer := inventory.Client{Source: "native", ID: "different", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.2")}, ObservedAt: in.At.Add(time.Minute), ExpiresAt: in.At.Add(time.Hour)}
				var err error
				in.At = in.At.Add(time.Minute)
				in.Inventory, err = inventory.PrepareReplacement(in.Inventory, "native", 1, []inventory.Client{newer}, in.At)
				if err != nil {
					t.Fatal(err)
				}
			}
			out, err := Prepare(in)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Groups[0].Spec.Clients) != 2 {
				t.Fatal("stale/foreign generated group grant")
			}
		})
	}
	in := servingInput(t)
	in.At = in.At.Add(time.Hour - time.Nanosecond)
	out, err := Prepare(in)
	if err != nil || len(out.Groups[0].Spec.Clients) != 3 {
		t.Fatal("lost grant before exact boundary", err)
	}
}
func TestPrepareRetainRemovalAndOtherAuthorizedSourceSurvives(t *testing.T) {
	in := servingInput(t)
	doc, _, at := sourceDocument(t)
	doc.Owner.Name = "other-source"
	doc.Owner.UID = "other-uid"
	doc.Snapshot.Clients = doc.Snapshot.Clients[1:]
	doc.Snapshot.Clients[0].ID = "other"
	doc.Snapshot.Clients[0].Source = host.SourceID(doc.Owner)
	doc.Snapshot.Clients[0].IPs = []netip.Addr{netip.MustParseAddr("192.0.2.3")}
	doc.Snapshot.Desired.RecordSets = nil
	doc.Objects = nil
	doc.Allocations = nil
	doc.Targets = nil
	raw := doc.Snapshot.Desired.ClientGroups[0]
	raw.Name = "other-group"
	raw.Spec.Sources.IntegrationRef.Name = doc.Owner.Name
	raw.Spec.Clients = raw.Spec.Clients[1:]
	doc.Snapshot.Desired.SourceID = host.SourceID(doc.Owner)
	doc.Groups[0].Group.Name = "other-group"
	doc.Groups[0].Group.UID = "other-group-uid"
	doc.Groups[0].MACs = doc.Groups[0].MACs[1:]
	otherImage := encodeDocument(t, doc, at)
	in.Sources = append(in.Sources, otherImage)
	in.Admissions = append(in.Admissions, SourceAdmission{Owner: doc.Owner, ConfigDigest: otherImage.Head.Spec.ConfigDigest, RootDigest: otherImage.Head.Spec.RootDigest, Ready: true})
	in.Groups = append(in.Groups, &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "other-group", Namespace: "dns", UID: "other-group-uid"}, Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: doc.Owner.Name}}}})
	var err error
	in.Inventory, err = inventory.PrepareReplacement(in.Inventory, host.SourceID(doc.Owner), 1, doc.Snapshot.Clients, at)
	if err != nil {
		t.Fatal(err)
	}
	source := in.Sources[0].Head.Spec.Owner
	in.Inventory, err = inventory.PrepareReplacement(in.Inventory, host.SourceID(source), 2, nil, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	in.At = at.Add(time.Minute)
	in.Sources[0].Head.Spec.State = "retained"
	in.Sources[0].Head.Spec.WithdrawnAt = in.At
	out, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups[0].Spec.Clients) != 2 || len(out.Groups[1].Spec.Clients) != 1 {
		t.Fatal("manual or other source membership changed")
	}
	h, err := inventory.RestoreState(out.Inventory, func() time.Time { return in.At })
	if err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.3"), in.At) == nil || h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.2"), in.At) != nil {
		t.Fatal("wrong source withdrawn")
	}
}
func TestPrepareRejectsSourceRevisionMismatchAndUnionOverflow(t *testing.T) {
	in := servingInput(t)
	in.Inventory.Sources[0].Revision = 2
	if _, err := Prepare(in); err == nil {
		t.Fatal("mixed source generation published")
	}
	in = servingInput(t)
	for len(in.Groups[0].Spec.Clients) < kinds.MaxEffectiveMembers {
		in.Groups[0].Spec.Clients = append(in.Groups[0].Spec.Clients, public.ClientMatcher{IP: "10.0.0.1"})
	}
	if _, err := Prepare(in); err == nil {
		t.Fatal("union exceeded cap")
	}
	in = servingInput(t)
	other := in.Admissions[0]
	other.Owner.UID = "recreated"
	in.Admissions = append(in.Admissions, other)
	if _, err := Prepare(in); err == nil {
		t.Fatal("two UID grants for one Integration name")
	}
}

func TestPrepareRejectsForeignClusterAndAmbiguousOwnershipEnrichment(t *testing.T) {
	in := servingInput(t)
	in.ClusterID = "other-cluster"
	if _, err := Prepare(in); err == nil {
		t.Fatal("foreign cluster accepted")
	}
	in = servingInput(t)
	other := inventory.Client{Source: "native", ID: "tied", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.2")}, ObservedAt: in.At, ExpiresAt: in.At.Add(time.Hour)}
	var err error
	in.Inventory, err = inventory.PrepareReplacement(in.Inventory, "native", 1, []inventory.Client{other}, in.At)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups[0].Spec.Clients) != 2 {
		t.Fatal("ambiguous inventory granted a group")
	}
	h, err := inventory.RestoreState(out.Inventory, func() time.Time { return in.At })
	if err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(other.IPs[0], in.At) != nil {
		t.Fatal("ambiguous assignment restored as live")
	}
}
