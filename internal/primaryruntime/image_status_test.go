package primaryruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func statusImage(t *testing.T, h *harness) *PreparedImage {
	t.Helper()
	return prepareTestImage(t, readTestImage(t, h), imageTestOptions(t, h))
}

func TestImageStatusCommittedUpdateAndEquivalentSource(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	l := h.begin(z)
	before, err := h.r.Reconcile(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	if out := h.r.Apply(ctx, h.request(l, "status-update", "dynamic.example. 60 IN A 192.0.2.17", false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	view, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	p := statusImage(t, h)
	o, ok := p.CommittedPrimaryStatus(z.Name)
	model := p.Cut(time.Now()).Match("dynamic.example.")
	token, _ := primaryzone.SourceToken(z)
	if !ok || !o.ConfigMatchesDesired || o.Member.Published || o.Member.Version != version(view) || o.HeadResourceVersion != view.Head.ResourceVersion || o.HeadZone != l.Spec.HeadZone() || o.CommittedSource != token || o.Serial != model.Serial() || o.ContentHash != model.ContentHash() || o.RecordCount != model.RecordCount() || !strings.HasPrefix(o.ContentHash, "sha256:") || o.ContentHash == view.Head.Spec.ContentHash || o.Serial == before.Head.Spec.Serial {
		t.Fatalf("untruthful committed status: %+v", o)
	}
	// Status-only RV churn aligns lifecycle without relabeling the committed receipt
	// or generating a second head/serial solely to project that head's status.
	if err := h.st.Get(ctx, store.KeyOf(z), z); err != nil {
		t.Fatal(err)
	}
	z.Status.Serial, z.Status.ContentHash, z.Status.RecordCount = o.Serial, o.ContentHash, o.RecordCount
	if err := h.st.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	equivalent, ok := statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if !ok || !equivalent.ConfigMatchesDesired || equivalent.CommittedSource != o.CommittedSource || equivalent.Source.ResourceVersion == o.Source.ResourceVersion || equivalent.Member.Version != o.Member.Version || equivalent.Serial != o.Serial || equivalent.ContentHash != o.ContentHash {
		t.Fatalf("status RV echo restamped evidence/head: %+v", equivalent)
	}
	if _, ok := p.CommittedPrimaryStatus("/primary"); ok {
		t.Fatal("normalized invalid source key")
	}
	var absent *PreparedImage
	if _, ok := absent.CommittedPrimaryStatus(z.Name); ok {
		t.Fatal("nil image supplied evidence")
	}
}

func TestImageStatusAbsenceAndHiddenFormerIdentity(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	if _, ok := statusImage(t, h).CommittedPrimaryStatus(z.Name); ok {
		t.Fatal("placeholder invented lifecycle")
	}
	l := h.begin(z)
	o, ok := statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if !ok || o.Member.Phase != zk.PhasePreparing || o.HeadZone != l.Spec.HeadZone() || o.HeadResourceVersion != "" || o.ConfigMatchesDesired || o.Serial != 0 || o.ContentHash != "" || o.RecordCount != 0 {
		t.Fatal("bad absent-head evidence", o)
	}
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	held := statusImage(t, h)
	original, _ := held.CommittedPrimaryStatus(z.Name)
	if err := h.st.Delete(ctx, store.KeyOf(z), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	removed := l.Spec.Source
	removed.Removed = true
	if _, err := h.r.RetireSource(ctx, l.Spec.Zone, removed); err != nil {
		t.Fatal(err)
	}
	replacement := h.zone(z.Name, z.Spec.ZoneName)
	p := statusImage(t, h)
	hidden, ok := p.CommittedPrimaryStatus(z.Name)
	placeholder, _ := p.Cut(time.Now()).Member(l.Spec.Zone.Apex)
	if !ok || hidden.Member.Zone.UID != string(z.UID) || hidden.Member.Phase != zk.PhaseRetired || hidden.HeadResourceVersion == "" || hidden.ConfigMatchesDesired || hidden.Member.Version.CommitSequence <= original.Member.Version.CommitSequence || placeholder.Zone.UID != string(replacement.UID) || placeholder.Version.CommitSequence != 0 {
		t.Fatal("desired overlay hid durable retired status", hidden, placeholder)
	}
	// A retained observation is independent of returned parent slices/candidate edits.
	if again, _ := held.CommittedPrimaryStatus(z.Name); !reflect.DeepEqual(again, original) {
		t.Fatal("held status changed")
	}
	image, opts := readTestImage(t, h), imageTestOptions(t, h)
	for _, typ := range []public.ZoneType{public.ZoneTypeSecondary, public.ZoneTypePrimary} {
		changed := rewriteTestImage(t, image, opts, func(obj store.Object) bool {
			if desired, ok := obj.(*public.DNSZone); ok {
				desired.Spec.Type = typ
				desired.Spec.ZoneName = "moved.example."
			}
			return true
		})
		moved, _ := prepareTestImage(t, changed, opts).CommittedPrimaryStatus(z.Name)
		if moved.ConfigMatchesDesired || moved.Member.Zone != hidden.Member.Zone || moved.HeadZone != hidden.HeadZone {
			t.Fatal("new type/apex borrowed historical metadata", moved)
		}
	}
}

func TestImageStatusRawMetadataAfterExpiryAndAncestorDeadline(t *testing.T) {
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
	view, err := h.repo.Read(ctx, parent.Spec.HeadZone())
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
	before, _ := p.CommittedPrimaryStatus("parent")
	c, _ := p.CommittedPrimaryStatus("child")
	if !before.ExpiresAt.Equal(expiry) || !c.ExpiresAt.Equal(expiry) || !c.ConfigMatchesDesired || len(c.Member.Parents) != 1 {
		t.Fatal("ancestor deadline/metadata", before, c)
	}
	c.Member.Parents[0].RootDigest = "mutated"
	again, _ := p.CommittedPrimaryStatus("child")
	if again.Member.Parents[0].RootDigest == "mutated" {
		t.Fatal("status aliases parents")
	}
	opts.Now = expiry
	expired := prepareTestImage(t, image, opts)
	after, _ := expired.CommittedPrimaryStatus("parent")
	if after.Serial != before.Serial || after.ContentHash != before.ContentHash || after.RecordCount != before.RecordCount || after.RecordCount == 0 || expired.Admitted("child.example.", expiry) {
		t.Fatal("expiry coverage replaced durable scalars", after)
	}
	opts.Now = expiry.Add(-time.Second)
	missing := rewriteTestImage(t, image, opts, func(obj store.Object) bool {
		if l, ok := obj.(*zk.ZoneLifecycle); ok && l.Spec.Zone.ObjectName == "child" {
			l.Spec.Parents = nil
		}
		return true
	})
	missingPart := prepareTestImage(t, missing, opts)
	obs, _ := missingPart.CommittedPrimaryStatus("child")
	if !obs.ConfigMatchesDesired || obs.ContentHash != again.ContentHash || missingPart.Admitted("child.example.", opts.Now) {
		t.Fatal("missing witness invented admission or lost metadata", obs)
	}
}

// statusHoldCompiler captures the actual SQLite gap after lifecycle source CAS,
// before the new head has been compiled/admitted. There are no synthetic RVs.
type statusHoldCompiler struct {
	zonestate.DesiredCompiler
	entered chan struct{}
	resume  chan struct{}
}

func (c statusHoldCompiler) Project(ctx context.Context, v *zonestate.View, in *zonestate.DesiredInputs, now time.Time) (*zonestate.Compilation, error) {
	close(c.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.resume:
	}
	return c.DesiredCompiler.Project(ctx, v, in, now)
}

func TestImageStatusActualSourceAdvancedOldHeadAdmissionWindow(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	l := h.begin(z)
	old, err := h.r.Reconcile(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Get(ctx, store.KeyOf(z), z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Nameservers = []string{"new-ns.example."}
	saved, err := h.st.Apply(ctx, z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := primaryzone.SourceToken(saved)
	entered, resume := make(chan struct{}), make(chan struct{})
	h.r.o.Compiler = statusHoldCompiler{h.r.o.Compiler, entered, resume}
	done := make(chan error, 1)
	go func() { _, err := h.r.Reconcile(ctx, l.Spec.Zone); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("compiler did not pause")
	}
	p := statusImage(t, h)
	o, ok := p.CommittedPrimaryStatus(z.Name)
	// This is an explicit reproduction of EXISTING serving admission, not a
	// status-slice correction. Root owns the separate serving readiness decision.
	admitted := p.Admitted(l.Spec.Zone.Apex, time.Now())
	t.Logf("source-advanced gap: admitted=%t configMatches=%t lifecycleRV=%s committedSourceRV=%s headRV=%s headSequence=%d", admitted, o.ConfigMatchesDesired, o.Source.ResourceVersion, o.CommittedSource.ResourceVersion, o.HeadResourceVersion, o.Member.Version.CommitSequence)
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !ok || !admitted || o.ConfigMatchesDesired || o.Source.ResourceVersion != token.ResourceVersion || o.Source.IntentDigest != token.IntentDigest || o.Member.Version != version(old) || o.CommittedSource.IntentDigest == token.IntentDigest {
		t.Fatal("did not expose exact lifecycle-new/head-old window", o, admitted)
	}
	aligned, _ := statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if !aligned.ConfigMatchesDesired || aligned.CommittedSource.IntentDigest != token.IntentDigest || aligned.Member.Version == o.Member.Version {
		t.Fatal("new source never aligned", aligned)
	}
}

func TestImageStatusFrozenNSAndMalformedReceiptFailClosed(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	l := h.begin(z)
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Get(ctx, store.KeyOf(z), z); err != nil {
		t.Fatal(err)
	}
	z.Status.NameserverAddresses = []string{"192.0.2.53"}
	if err := h.st.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	o, _ := statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if o.ConfigMatchesDesired {
		t.Fatal("NS address status treated as intent-equivalent config")
	}
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	o, _ = statusImage(t, h).CommittedPrimaryStatus(z.Name)
	if !o.ConfigMatchesDesired {
		t.Fatal("frozen NS dependency never aligned", o)
	}
	view, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"unknownVersion", "malformedObject", "wrongIdentity"} {
		t.Run(mutation, func(t *testing.T) {
			a := *view.Aggregate
			a.Sources = append([]zonestate.Source(nil), a.Sources...)
			for i, s := range a.Sources {
				if s.ID != o.CommittedSource.ID {
					continue
				}
				envelope, err := zonestate.DecodeSourceEnvelope(s.Desired)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "unknownVersion":
					envelope.FormatVersion = 2
				case "malformedObject":
					envelope.Object = json.RawMessage(`[]`)
				case "wrongIdentity":
					envelope.Object = json.RawMessage(`{"metadata":{"name":"other","uid":"other"},"spec":{}}`)
				}
				a.Sources[i].Desired, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Repository stores bounded opaque evidence; the status reader recognizes
			// only strict known evidence and must not treat a claimed digest as proof.
			base, err := h.repo.Read(ctx, l.Spec.HeadZone())
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := zonestate.Prepare(&a, base.Head, zonestate.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.repo.Commit(ctx, base.Expected, prepared, nil); err != nil {
				t.Fatal(err)
			}
			observed, _ := statusImage(t, h).CommittedPrimaryStatus(z.Name)
			if observed.ConfigMatchesDesired {
				t.Fatal("malformed source receipt supplied current config")
			}
		})
	}
}

func TestImageStatusEffectiveHeadIncarnationKeepsActualSourceReceipt(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	l := h.begin(z)
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	retired, err := h.r.Retire(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewLifecycle(l.Spec.Zone.ClusterID, z, nil)
	if err != nil {
		t.Fatal(err)
	}
	next.Spec.Sequence = retired.Spec.Sequence + 1
	next.Spec.Incarnation = retired.Spec.Incarnation + 1
	next, err = h.r.Transition(ctx, retired, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(ctx, next.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	p := statusImage(t, h)
	o, ok := p.CommittedPrimaryStatus(z.Name)
	token, err := primaryzone.SourceToken(z)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !o.ConfigMatchesDesired || o.Member.Incarnation != 2 || o.Member.Zone.UID != string(z.UID) || o.HeadZone.UID == string(z.UID) || o.HeadZone != next.Spec.HeadZone() || o.Member.Version.Zone != o.HeadZone || o.CommittedSource != token || !p.Admitted(z.Spec.ZoneName, time.Now()) {
		t.Fatal("effective head UID replaced actual desired receipt", o)
	}
}
