package primaryruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func imageTestOptions(t *testing.T, h *harness) ImageOptions {
	t.Helper()
	scheme := runtimeScheme(t)
	// LOCAL scheme registry, not the candidate's manifest. Mirror the store's
	// object/list registration rule, including every zero-count local kind.
	known := scheme.KnownTypes(public.GroupVersion)
	var kinds []replication.Kind
	for name, typ := range known {
		listTyp, ok := known[name+"List"]
		if !ok {
			continue
		}
		obj := reflect.New(typ).Interface()
		if _, isList := obj.(metav1.ListInterface); isList {
			continue
		}
		if _, ok := obj.(store.Object); !ok {
			continue
		}
		if _, ok := reflect.New(listTyp).Interface().(metav1.ListInterface); !ok {
			continue
		}
		kinds = append(kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
	}
	slices.SortFunc(kinds, func(a, b replication.Kind) int { return strings.Compare(a.Kind, b.Kind) })
	return ImageOptions{ClusterID: h.st.Info().ClusterID, Scheme: scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: kinds}, Now: time.Now()}
}

func readTestImage(t *testing.T, h *harness) replication.Image {
	t.Helper()
	i, err := h.st.(*sqlite.Store).ReadGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return *i
}

func rewriteTestImage(t *testing.T, input replication.Image, opts ImageOptions, each func(store.Object) bool) replication.Image {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var image replication.Image
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	objects := make([]json.RawMessage, 0, len(image.Payload.Objects))
	for _, raw := range image.Payload.Objects {
		var h metav1.TypeMeta
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		gv, err := schema.ParseGroupVersion(h.APIVersion)
		if err != nil {
			t.Fatal(err)
		}
		object, err := opts.Scheme.New(gv.WithKind(h.Kind))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, object); err != nil {
			t.Fatal(err)
		}
		if each(object.(store.Object)) {
			encoded, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			objects = append(objects, encoded)
		}
	}
	image.Payload.Objects = objects
	if err := replication.Seal(&image); err != nil {
		t.Fatal(err)
	}
	return image
}

func prepareTestImage(t *testing.T, image replication.Image, opts ImageOptions) *PreparedImage {
	t.Helper()
	p, err := PrepareImage(context.Background(), image, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func assertImageCoverage(t *testing.T, p *PreparedImage, apex string, now time.Time) {
	t.Helper()
	model := p.Cut(now).Match("x." + apex)
	if model == nil || model.Name() != apex || model.AvailableAt(now) {
		t.Fatalf("missing unavailable coverage for %s: %v", apex, model)
	}
}

func TestImageActualCommittedDataAndDetachedCandidate(t *testing.T) {
	h := newHarness(t, "sqlite")
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(context.Background(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	if out := h.r.Apply(context.Background(), h.request(l, "one", "dynamic.example. 60 IN A 192.0.2.1", false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	// A newer desired RRset is not an independently retained legacy primary.
	if _, err := h.st.Apply(context.Background(), &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "uncommitted", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "primary"}, Endpoints: []public.Endpoint{{DNSName: "desired.example.", RecordType: "A", Targets: []string{"192.0.2.99"}}}}}, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	image, opts := readTestImage(t, h), imageTestOptions(t, h)
	want, err := h.repo.Read(context.Background(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	// Closing the actual store proves preparation cannot reenter it. Removing
	// live runtime entries also proves no independently captured runtime part.
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.r.entries = nil
	h.r.rebuild()
	p := prepareTestImage(t, image, opts)
	held := p.Cut(opts.Now)
	model := held.Match("dynamic.example.")
	if model == nil || !model.AvailableAt(opts.Now) || model.Lookup("dynamic.example.", dns.TypeA).Kind != zone.Success || model.Lookup("dynamic.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatal("committed UPDATE was not built from the image")
	}
	if model.Lookup("desired.example.", dns.TypeA).Kind != zone.NXDomain {
		t.Fatal("uncommitted desired RRset mixed with committed authority")
	}
	m, ok := held.Member("example.")
	if !ok || m.Incarnation != 1 || m.Published || m.Version != version(want) || p.Versions()["example."] != version(want) || !p.Base().Same(image.Generation()) {
		t.Fatal("candidate base/version/incarnation mismatch", m)
	}
	for i := range image.Payload.Objects {
		clear(image.Payload.Objects[i])
	}
	image.Term.ClusterID = "mutated"
	image.KindManifest[0].Kind = "mutated"
	versions := p.Versions()
	delete(versions, "example.")
	names := p.DesiredPrimaryNames()
	names[0] = "mutated"
	if held.Match("dynamic.example.").Lookup("dynamic.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.1" || p.Versions()["example."] != m.Version || p.DesiredPrimaryNames()[0] != "example." || p.Base().Term.ClusterID == "mutated" {
		t.Fatal("held part aliases input or returned collections")
	}
}

func TestImageDesiredWithoutHeadHasNoDurableMembership(t *testing.T) {
	h := newHarness(t, "sqlite")
	z := h.zone("primary", "example.")
	opts := imageTestOptions(t, h)
	for _, preparing := range []bool{false, true} {
		if preparing {
			h.begin(z)
		}
		p := prepareTestImage(t, readTestImage(t, h), opts)
		assertImageCoverage(t, p, "example.", opts.Now)
		m, ok := p.Cut(opts.Now).Member("example.")
		if !ok || m.Published || m.Version != (zonestate.Version{}) || len(p.Versions()) != 0 || (!preparing && (m.Incarnation != 0 || m.Sequence != 0 || m.Phase != "")) || (preparing && m.Incarnation != 1) {
			t.Fatal("coverage invented durable head or admission", m)
		}
	}
}

func TestImageDeletionTypeReplacementAndRetiredIncarnation(t *testing.T) {
	h := newHarness(t, "sqlite")
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(context.Background(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	image, opts := readTestImage(t, h), imageTestOptions(t, h)
	orphanHead := rewriteTestImage(t, image, opts, func(o store.Object) bool { _, ledger := o.(*zk.ZoneLifecycle); return !ledger })
	orphanPart := prepareTestImage(t, orphanHead, opts)
	assertImageCoverage(t, orphanPart, "example.", opts.Now)
	if m, _ := orphanPart.Cut(opts.Now).Member("example."); m.Incarnation != 0 || m.Version != (zonestate.Version{}) || len(orphanPart.Versions()) != 0 {
		t.Fatal("orphan head substituted for admitted primary membership", m)
	}
	deleted := rewriteTestImage(t, image, opts, func(o store.Object) bool { _, desired := o.(*public.DNSZone); return !desired })
	p := prepareTestImage(t, deleted, opts)
	assertImageCoverage(t, p, "example.", opts.Now)
	if len(p.DesiredPrimaryNames()) != 0 {
		t.Fatal("deleted source retained desired membership")
	}
	retired := rewriteTestImage(t, deleted, opts, func(o store.Object) bool {
		switch v := o.(type) {
		case *zk.ZoneLifecycle:
			v.Spec.Phase = zk.PhaseRetired
			v.Spec.Source.Removed = true
		case *zk.ZoneState:
			v.Spec.Lifecycle = zk.LifecycleTombstoned
		}
		return true
	})
	if cut := prepareTestImage(t, retired, opts).Cut(opts.Now); cut.Match("x.example.") != nil {
		t.Fatal("completed desired deletion retained primary routing")
	}
	typeChanged := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if z, ok := o.(*public.DNSZone); ok {
			z.Spec.Type = public.ZoneTypeSecondary
		}
		return true
	})
	assertImageCoverage(t, prepareTestImage(t, typeChanged, opts), "example.", opts.Now)
	newUID := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if z, ok := o.(*public.DNSZone); ok {
			z.UID = "replacement-uid"
		}
		return true
	})
	p = prepareTestImage(t, newUID, opts)
	assertImageCoverage(t, p, "example.", opts.Now)
	m, _ := p.Cut(opts.Now).Member("example.")
	if m.Zone.UID != "replacement-uid" || m.Incarnation != 0 || m.Version != (zonestate.Version{}) || len(p.Versions()) != 0 {
		t.Fatal("replacement placeholder inherited retired membership", m)
	}
	replaced := rewriteTestImage(t, typeChanged, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneLifecycle); ok {
			v.Spec.Incarnation = 2
			v.Spec.Sequence++
			v.Spec.Type = public.ZoneTypeSecondary
		}
		return true
	})
	if cut := prepareTestImage(t, replaced, opts).Cut(opts.Now); cut.Match("x.example.") != nil {
		t.Fatal("nonprimary replacement mixed retired primary RR authority")
	}
	// An incarnation-two primary never falls back to incarnation-one's head.
	inc2 := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneLifecycle); ok {
			v.Spec.Incarnation = 2
			v.Spec.Sequence++
			v.Spec.Phase = zk.PhasePreparing
		}
		return true
	})
	p = prepareTestImage(t, inc2, opts)
	assertImageCoverage(t, p, "example.", opts.Now)
	m, _ = p.Cut(opts.Now).Member("example.")
	if m.Incarnation != 2 || m.Version != (zonestate.Version{}) {
		t.Fatal("reused an earlier incarnation's head", m)
	}
	activeMissing := rewriteTestImage(t, inc2, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneLifecycle); ok {
			v.Spec.Phase = zk.PhaseActive
		}
		return true
	})
	if _, err := PrepareImage(context.Background(), activeMissing, opts); err == nil {
		t.Fatal("active lifecycle without its exact head accepted")
	}
}

func TestImageParentWitnessesLateAncestorAndExactExpiry(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	parent := h.begin(h.zone("parent", "example."))
	if _, err := h.r.Reconcile(ctx, parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	parent, err := h.r.ReadLifecycle(ctx, parent.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	child := h.begin(h.zone("child", "child.example."), parent)
	if _, err := h.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	view, err := h.repo.Read(ctx, child.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	a := *view.Aggregate
	a.Expiry = expiry
	prepared, err := zonestate.Prepare(&a, view.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.Commit(ctx, view.Expected, prepared, nil); err != nil {
		t.Fatal(err)
	}
	image, opts := readTestImage(t, h), imageTestOptions(t, h)
	p := prepareTestImage(t, image, opts)
	m, _ := p.Cut(opts.Now).Member("child.example.")
	if len(m.Parents) != 1 || m.Parents[0].CommitSequence == 0 || !p.Cut(expiry.Add(-time.Nanosecond)).Match("x.child.example.").AvailableAt(expiry.Add(-time.Nanosecond)) {
		t.Fatal("persisted witness/expiry lost", m)
	}
	assertImageCoverage(t, p, "child.example.", expiry)
	m.Parents[0].RootDigest = "mutated"
	again, _ := p.Cut(opts.Now).Member("child.example.")
	if again.Parents[0].RootDigest == "mutated" {
		t.Fatal("parent slice not detached")
	}
	missing := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneLifecycle); ok && v.Spec.Zone.Apex == "child.example." {
			v.Spec.Parents = nil
		}
		return true
	})
	p = prepareTestImage(t, missing, opts)
	assertImageCoverage(t, p, "child.example.", opts.Now)
	assertImageCoverage(t, p, "example.", opts.Now)
	future := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneLifecycle); ok && v.Spec.Zone.Apex == "child.example." {
			v.Spec.Parents[0].CommitSequence += 100
		}
		return true
	})
	assertImageCoverage(t, prepareTestImage(t, future, opts), "child.example.", opts.Now)
	wrongBoundary := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneLifecycle); ok && v.Spec.Zone.Apex == "child.example." {
			v.Spec.Parents[0].BoundaryID = zk.Digest([]byte("wrong"))
		}
		return true
	})
	if _, err := PrepareImage(ctx, wrongBoundary, opts); err == nil {
		t.Fatal("wrong persisted boundary identity accepted")
	}
}

func TestImageStrictRegistryBoundsCarriersAndOrphanChunks(t *testing.T) {
	h := newHarness(t, "sqlite")
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(context.Background(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	image, opts := readTestImage(t, h), imageTestOptions(t, h)
	// Valid orphan chunk is legal between staging and GC. It cannot create data.
	data := []byte("unreferenced")
	digest := zk.Digest(data)
	chunk := &zk.ZoneChunk{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: zk.KindZoneChunk}, ObjectMeta: metav1.ObjectMeta{Name: zk.ChunkName(digest), ResourceVersion: "1", UID: "orphan", Generation: 1, CreationTimestamp: metav1.NewTime(opts.Now)}, Spec: zk.ZoneChunkSpec{FormatVersion: zk.FormatVersion, Digest: digest, Data: data}}
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	orphan := rewriteTestImage(t, image, opts, func(store.Object) bool { return true })
	orphan.Payload.Objects = append(orphan.Payload.Objects, raw)
	if err := replication.Seal(&orphan); err != nil {
		t.Fatal(err)
	}
	prepareTestImage(t, orphan, opts)
	corrupt := rewriteTestImage(t, orphan, opts, func(o store.Object) bool {
		if c, ok := o.(*zk.ZoneChunk); ok && c.Spec.Digest == digest {
			c.Spec.Data[0] ^= 1
		}
		return true
	})
	missing := rewriteTestImage(t, image, opts, func(o store.Object) bool { _, chunk := o.(*zk.ZoneChunk); return !chunk })
	unknown := rewriteTestImage(t, image, opts, func(o store.Object) bool {
		if v, ok := o.(*zk.ZoneState); ok {
			v.Spec.FormatVersion++
		}
		return true
	})
	noMetadata := rewriteTestImage(t, image, opts, func(o store.Object) bool { o.SetUID(""); return true })
	unknownField := rewriteTestImage(t, image, opts, func(store.Object) bool { return true })
	for i, raw := range unknownField.Payload.Objects {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		if string(obj["kind"]) != `"ZoneState"` {
			continue
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(obj["spec"], &spec); err != nil {
			t.Fatal(err)
		}
		spec["future"] = json.RawMessage(`true`)
		encoded, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		obj["spec"] = encoded
		unknownField.Payload.Objects[i], err = json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if err := replication.Seal(&unknownField); err != nil {
		t.Fatal(err)
	}
	noZero := rewriteTestImage(t, image, opts, func(store.Object) bool { return true })
	for i, kind := range noZero.KindManifest {
		if kind.Count == 0 {
			noZero.KindManifest = slices.Delete(noZero.KindManifest, i, i+1)
			break
		}
	}
	if err := replication.Seal(&noZero); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]replication.Image{"missing zero manifest": noZero, "missing referenced chunk": missing, "corrupt orphan chunk": corrupt, "unknown head version": unknown, "unknown private field": unknownField, "missing source metadata": noMetadata} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareImage(context.Background(), bad, opts); err == nil {
				t.Fatal("malformed complete image accepted")
			}
		})
	}
	for name, bad := range map[string]ImageOptions{"nil scheme": {Validation: opts.Validation, ClusterID: opts.ClusterID, Now: opts.Now}, "no registry": {Scheme: opts.Scheme, ClusterID: opts.ClusterID, Now: opts.Now}, "wrong cluster": {Scheme: opts.Scheme, Validation: opts.Validation, ClusterID: "wrong", Now: opts.Now}, "unbounded zones": {Scheme: opts.Scheme, Validation: opts.Validation, ClusterID: opts.ClusterID, Now: opts.Now, MaxZones: 100001}} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareImage(context.Background(), image, bad); err == nil {
				t.Fatal("invalid local preparation contract accepted")
			}
		})
	}
	bounded := opts
	bounded.Validation.Limits.MaxObjectBytes = 10
	if _, err := PrepareImage(context.Background(), image, bounded); !errors.Is(err, ErrCapacity) {
		t.Fatal("object preflight bound not enforced", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareImage(canceled, image, opts); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preparation proceeded", err)
	}
}

func TestImageLateDesiredChildBeforeLifecycleBlocksParent(t *testing.T) {
	h := newHarness(t, "sqlite")
	parent := h.begin(h.zone("parent", "example."))
	if _, err := h.r.Reconcile(context.Background(), parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	h.zone("new-child", "child.example.")
	opts := imageTestOptions(t, h)
	p := prepareTestImage(t, readTestImage(t, h), opts)
	assertImageCoverage(t, p, "example.", opts.Now)
	assertImageCoverage(t, p, "child.example.", opts.Now)
	m, _ := p.Cut(opts.Now).Member("child.example.")
	if m.Incarnation != 0 || m.Version != (zonestate.Version{}) || m.Published {
		t.Fatal("late desired child fabricated membership", m)
	}
}

func TestImageActualRetirementReplacementAndDeletion(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	source := h.zone("primary", "example.")
	l := h.begin(source)
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	oldID := l.Spec.HeadZone()
	opts := imageTestOptions(t, h)
	held := prepareTestImage(t, readTestImage(t, h), opts).Cut(opts.Now)
	retired, err := h.r.Retire(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	assertImageCoverage(t, prepareTestImage(t, readTestImage(t, h), opts), "example.", opts.Now)
	next, err := NewLifecycle(l.Spec.Zone.ClusterID, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	next.Spec.Sequence = retired.Spec.Sequence + 1
	next.Spec.Incarnation = retired.Spec.Incarnation + 1
	l, err = h.r.Transition(ctx, retired, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	p := prepareTestImage(t, readTestImage(t, h), opts)
	m, _ := p.Cut(opts.Now).Member("example.")
	if m.Incarnation != 2 || m.Version.Zone != l.Spec.HeadZone() || m.Version.Zone == oldID || !p.Cut(opts.Now).Match("x.example.").AvailableAt(opts.Now) || m.Published {
		t.Fatal("did not preserve actual replacement head/incarnation", m)
	}
	token, err := primaryzone.SourceToken(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Delete(ctx, store.KeyOf(source), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	assertImageCoverage(t, prepareTestImage(t, readTestImage(t, h), opts), "example.", opts.Now)
	if _, err := h.r.RetireSource(ctx, l.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest, Removed: true}); err != nil {
		t.Fatal(err)
	}
	if p := prepareTestImage(t, readTestImage(t, h), opts); p.Cut(opts.Now).Match("x.example.") != nil || len(p.Versions()) != 0 {
		t.Fatal("actual retired deletion retained active routing")
	}
	if !held.Match("x.example.").AvailableAt(opts.Now) {
		t.Fatal("held old image changed after real retirement/replacement/deletion")
	}
}
