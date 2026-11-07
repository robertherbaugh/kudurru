package primaryruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
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

// imageHoldReconcile pauses the real compiler after any lifecycle source CAS.
// Cleanup releases and joins the operation even if an image assertion fails.
func imageHoldReconcile(t *testing.T, h *harness, id zk.ZoneID) (*PreparedImage, func()) {
	t.Helper()
	compiler := h.r.o.Compiler
	entered, resume := make(chan struct{}), make(chan struct{})
	h.r.o.Compiler = statusHoldCompiler{compiler, entered, resume}
	done := make(chan error, 1)
	go func() { _, err := h.r.Reconcile(context.Background(), id); done <- err }()
	finished := false
	finish := func() {
		if finished {
			return
		}
		finished = true
		close(resume)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("held source reconcile: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("held reconcile did not join")
		}
		h.r.o.Compiler = compiler
	}
	t.Cleanup(finish)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("source reconcile did not enter compiler")
	}
	return statusImage(t, h), finish
}

func TestImageReadinessActualParentConfigGapClosesPrimaryAndNonprimaryChildren(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("parent", "example.")
	parent := h.begin(z)
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
	forwarded := h.zone("forward", "forward.example.")
	forwarded.Spec.Type = public.ZoneTypeForward
	object, err := h.st.Apply(ctx, forwarded, store.ApplyOptions{Manager: "api"})
	if err != nil {
		t.Fatal(err)
	}
	nonprimary := h.begin(object.(*public.DNSZone), parent)
	if _, err := h.r.FenceParents(ctx, nonprimary.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Activate(ctx, nonprimary.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	parentView, err := h.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	a := *parentView.Aggregate
	expiry := time.Now().Add(time.Hour)
	a.Expiry = expiry
	prepared, err := zonestate.Prepare(&a, parentView.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	parentView, err = h.repo.Commit(ctx, parentView.Expected, prepared, nil)
	if err != nil {
		t.Fatal(err)
	}
	held := statusImage(t, h)
	original := map[string]PrimaryStatusObservation{}
	for _, name := range []string{"parent", "child", "forward"} {
		o, ok := held.CommittedPrimaryStatus(name)
		if !ok || !held.Admitted(o.Member.Zone.Apex, time.Now()) || !o.ExpiresAt.Equal(expiry) {
			t.Fatal("baseline member/witness/deadline missing", o)
		}
		original[name] = o
	}
	if err := h.st.Get(ctx, store.KeyOf(z), z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Policy.QueryAccess = public.AccessDeny
	if _, err := h.st.Apply(ctx, z, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	gap, finish := imageHoldReconcile(t, h, parent.Spec.Zone)
	for _, name := range []string{"parent", "child", "forward"} {
		o, _ := gap.CommittedPrimaryStatus(name)
		if gap.Admitted(o.Member.Zone.Apex, time.Now()) || o.Member.Version != original[name].Member.Version || !reflect.DeepEqual(o.Member.Parents, original[name].Member.Parents) || !o.ExpiresAt.Equal(expiry) {
			t.Fatal("config gap lost evidence or admitted descendant", o)
		}
	}
	assertImageCoverage(t, gap, "example.", time.Now())
	assertImageCoverage(t, gap, "child.example.", time.Now())
	if gap.Cut(time.Now()).Match("x.example.").QueryAllowed(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("parent gap retained old query ACL")
	}
	if !hasFence(parentView, child.Spec.Zone.Apex) || !hasFence(parentView, nonprimary.Spec.Zone.Apex) {
		t.Fatal("missing durable parent boundaries")
	}
	finish()
	aligned := statusImage(t, h)
	repaired, err := h.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if !hasFence(repaired, child.Spec.Zone.Apex) || !hasFence(repaired, nonprimary.Spec.Zone.Apex) {
		t.Fatal("background config repair erased boundaries")
	}
	for _, name := range []string{"parent", "child", "forward"} {
		o, _ := aligned.CommittedPrimaryStatus(name)
		if !aligned.Admitted(o.Member.Zone.Apex, time.Now()) || !reflect.DeepEqual(o.Member.Parents, original[name].Member.Parents) {
			t.Fatal("repair did not restore exact child witnesses", o)
		}
	}
	// Recompilation uses current RR expiry, not the fixture's independent older
	// deadline. The held gap still retains its exact original ancestor deadline.
	if gap.Admitted("forward.example.", expiry) || held.Admitted("child.example.", expiry) {
		t.Fatal("held absolute deadline was weakened")
	}
	if !held.Admitted("example.", time.Now()) || !held.Cut(time.Now()).Match("x.child.example.").AvailableAt(time.Now()) {
		t.Fatal("held image changed after repair")
	}
}

func TestImageReadinessStatusOnlySourceAlignmentAdmitsEquivalentHead(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	l := h.begin(z)
	old, err := h.r.Reconcile(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if err := h.st.Get(ctx, store.KeyOf(z), z); err != nil {
		t.Fatal(err)
	}
	z.Status.Serial, z.Status.ContentHash, z.Status.RecordCount = before.Serial, before.ContentHash, before.RecordCount
	if err := h.st.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	unaligned := statusImage(t, h)
	if unaligned.Admitted("example.", time.Now()) {
		t.Fatal("source RV mismatch was bypassed")
	}
	gap, finish := imageHoldReconcile(t, h, l.Spec.Zone)
	o, _ := gap.CommittedPrimaryStatus(z.Name)
	if !gap.Admitted("example.", time.Now()) || !o.ConfigMatchesDesired || o.Member.Version != version(old) || o.CommittedSource != before.CommittedSource || o.Source.ResourceVersion == o.CommittedSource.ResourceVersion {
		t.Fatal("equivalent old receipt incorrectly blocked current head", o)
	}
	finish()
	after, _ := statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if after.Member.Version != before.Member.Version || after.Serial != before.Serial || after.ContentHash != before.ContentHash || after.CommittedSource != before.CommittedSource {
		t.Fatal("status-only alignment manufactured head/serial/fresh observed RV", after)
	}
}

func TestImageReadinessRecordSetPendingCreateEditDeleteKeepsCommittedAuthority(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	// An inactive static source reserves ownership even while it contributes no RR.
	inactive := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "inactive", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "inactive.example.", RecordType: "A", Targets: []string{"192.0.2.99"}, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: time.Now().Add(-time.Hour)}}}}}}
	if _, err := h.st.Apply(ctx, inactive, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	l := h.begin(z)
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	parent, err := h.r.ReadLifecycle(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	child := h.begin(h.zone("child", "child.example."), parent)
	if _, err := h.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	if out := h.r.Apply(ctx, h.request(l, "pending-recordsets", "dynamic.example. 60 IN A 192.0.2.17", false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	observer, err := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: primaryzone.BuildCommitted})
	if err != nil {
		t.Fatal(err)
	}
	h.r.o.Reader = primaryzone.NewReader(h.st, primaryzone.ReaderOptions{PageSize: 1, Targets: observer.PendingTargets, Policy: func(context.Context, *public.DNSZone) (*zonestate.Policy, error) { return &h.policy, nil }})
	var desired *public.DNSRecordSet
	previousIP := ""
	assertRecord := func(t *testing.T, model *zone.Zone, name, want string) {
		t.Helper()
		answer := model.Lookup(name, dns.TypeA)
		if want == "" {
			if answer.Kind != zone.NXDomain {
				t.Fatal("uncommitted/removed record leaked", name, answer)
			}
			return
		}
		if answer.Kind != zone.Success || len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != want {
			t.Fatal("wrong committed RR", name, answer, want)
		}
	}
	for _, operation := range []string{"create", "edit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			old, err := h.repo.Read(ctx, l.Spec.HeadZone())
			if err != nil {
				t.Fatal(err)
			}
			if err := observer.Load(ctx, old); err != nil {
				t.Fatal(err)
			}
			var nextIP string
			action := "apply"
			if operation == "delete" {
				if err := h.st.Delete(ctx, store.KeyOf(desired), store.DeleteOptions{Manager: "api"}); err != nil {
					t.Fatal(err)
				}
				nextIP, action = "", "delete"
			} else {
				if operation == "create" {
					desired = &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "desired", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "desired.example.", RecordType: "A", Targets: []string{"192.0.2.1"}}}}}
					nextIP = "192.0.2.1"
				} else {
					desired = desired.DeepCopy()
					desired.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
					nextIP = "192.0.2.2"
				}
				saved, err := h.st.Apply(ctx, desired, store.ApplyOptions{Manager: "api"})
				if err != nil {
					t.Fatal(err)
				}
				desired = saved.(*public.DNSRecordSet)
			}
			token, err := primaryzone.SourceToken(desired)
			if err != nil {
				t.Fatal(err)
			}
			target := zonestate.DesiredTarget{Zone: l.Spec.HeadZone(), Source: token, Operation: action}
			if err := observer.RegisterDesired(target); err != nil {
				t.Fatal(err)
			}
			// This local observer is used only for pending durable source evidence;
			// its local publication flag is never a whole-publisher acknowledgment.
			if evidence := observer.Desired(target); evidence.Outcome != "pending" || evidence.Published {
				t.Fatal("old data claimed pending desired receipt published", evidence)
			}
			pending := statusImage(t, h)
			o, _ := pending.CommittedPrimaryStatus(z.Name)
			now := time.Now()
			model := pending.Cut(now).Match("desired.example.")
			if !pending.Admitted("example.", now) || !o.ConfigMatchesDesired || model == nil || !model.AvailableAt(now) || o.Member.Version != version(old) || o.Serial != old.Head.Spec.Serial {
				t.Fatal("recordset-only pending operation suspended or rewrote committed head", o)
			}
			assertRecord(t, model, "desired.example.", previousIP)
			assertRecord(t, model, "dynamic.example.", "192.0.2.17")
			fresh, err := h.r.Reconcile(ctx, l.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			committed := statusImage(t, h)
			model = committed.Cut(time.Now()).Match("desired.example.")
			if !committed.Admitted("example.", time.Now()) || fresh.Head.Spec.Serial == old.Head.Spec.Serial || fresh.Head.Spec.ContentHash == old.Head.Spec.ContentHash || !hasFence(fresh, child.Spec.Zone.Apex) {
				t.Fatal("reconcile did not commit new static data with boundary retained")
			}
			assertRecord(t, model, "desired.example.", nextIP)
			assertRecord(t, model, "dynamic.example.", "192.0.2.17")
			protected := false
			for _, reservation := range fresh.Aggregate.Reservations {
				if reservation.Name == "inactive.example." && reservation.Type == dns.TypeA {
					protected = true
				}
			}
			if !protected || !reflect.DeepEqual(fresh.Aggregate.Receipts, old.Aggregate.Receipts) {
				t.Fatal("source repair lost inactive reservation or dynamic UPDATE receipt")
			}
			acknowledged := false
			for _, s := range fresh.Aggregate.Sources {
				if s.ID != token.ID {
					continue
				}
				envelope, err := zonestate.DecodeSourceEnvelope(s.Desired)
				if err != nil {
					t.Fatal(err)
				}
				expectedState := "present"
				if operation == "delete" {
					expectedState = "removed"
				}
				if envelope.State != expectedState || s.UID != token.UID || s.ResourceVersion != token.ResourceVersion || envelope.IntentDigest != token.IntentDigest {
					t.Fatal("source reconciliation receipt mismatched actual desired operation", s)
				}
				for _, ack := range envelope.Acknowledged {
					if ack.Source == token && ack.Operation == action && ack.Outcome == "committed" {
						acknowledged = true
					}
				}
			}
			if !acknowledged {
				t.Fatal("current target not acknowledged durably")
			}
			observer.ForgetDesired(target)
			// After create, a record-set status RV update alone must not suspend DNS
			// or force another durable head/serial merely to relabel observed receipt.
			if operation == "create" {
				originalToken := token
				desired.Status.ObservedGeneration = desired.Generation
				if err := h.st.UpdateStatus(ctx, desired); err != nil {
					t.Fatal(err)
				}
				beforeStatus := statusImage(t, h)
				if !beforeStatus.Admitted("example.", time.Now()) {
					t.Fatal("record-set status echo closed committed primary")
				}
				equivalent, err := h.r.Reconcile(ctx, l.Spec.Zone)
				if err != nil {
					t.Fatal(err)
				}
				if version(equivalent) != version(fresh) || equivalent.Head.Spec.Serial != fresh.Head.Spec.Serial {
					t.Fatal("record-set status echo generated a head/serial")
				}
				for _, s := range equivalent.Aggregate.Sources {
					if s.ID == originalToken.ID && s.ResourceVersion != originalToken.ResourceVersion {
						t.Fatal("record-set observed RV was restamped")
					}
				}
			}
			// A previously prepared pending candidate continues to hold the exact old
			// committed data even after the source was reconciled into a later head.
			assertRecord(t, pending.Cut(now).Match("desired.example."), "desired.example.", previousIP)
			previousIP = nextIP
		})
	}
}
