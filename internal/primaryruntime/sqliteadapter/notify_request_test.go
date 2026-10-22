package sqliteadapter

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func notifyActive(t *testing.T, f *fixture, mode public.NotifyMode) *zk.ZoneLifecycle {
	t.Helper()
	z := f.zone("primary", "example.")
	z.Spec.Policy.Notify = mode
	z.Spec.Policy.NotifyTargets = []string{"192.0.2.53"}
	saved, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: z.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	l := f.begin(saved.(*public.DNSZone))
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	l, err = f.r.ReadLifecycle(f.ctx(), l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func notifyObservation(t *testing.T, f *fixture, l *zk.ZoneLifecycle) pr.PrimaryStatusObservation {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zk.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var kinds []replication.Kind
	for name, typ := range scheme.KnownTypes(public.GroupVersion) {
		if _, isList := reflect.New(typ).Interface().(metav1.ListInterface); isList {
			continue
		}
		if _, ok := reflect.New(typ).Interface().(store.Object); !ok {
			continue
		}
		if _, ok := scheme.KnownTypes(public.GroupVersion)[name+"List"]; ok {
			kinds = append(kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
		}
	}
	slices.SortFunc(kinds, func(a, b replication.Kind) int { return strings.Compare(a.Kind, b.Kind) })
	image, err := f.s.ReadGeneration(f.ctx())
	if err != nil {
		t.Fatal(err)
	}
	part, err := pr.PrepareImage(f.ctx(), *image, pr.ImageOptions{ClusterID: f.s.Info().ClusterID, Scheme: scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: kinds}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	o, ok := part.CommittedPrimaryStatus(l.Spec.Zone.ObjectName)
	if !ok {
		t.Fatal("missing complete-image observation")
	}
	return o
}

func requestNotify(ctx context.Context, t *testing.T, f *fixture, o pr.PrimaryStatusObservation, final func(context.Context) error) (NotifyIntentResult, error) {
	t.Helper()
	var result NotifyIntentResult
	err := f.s.Command(ctx, f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if final != nil {
			if err := tx.RequireCommitCheck(final); err != nil {
				return err
			}
		}
		var err error
		result, err = f.a.RequestNotifyTx(ctx, tx, o)
		return err
	})
	return result, err
}

func TestRequestNotifyTxExactRearmAndCoalescing(t *testing.T) {
	f := setup(t)
	l := notifyActive(t, f, public.NotifySpecified)
	o := notifyObservation(t, f, l)
	before, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	image, err := f.s.ReadGeneration(f.ctx())
	if err != nil {
		t.Fatal(err)
	}
	result, err := requestNotify(f.ctx(), t, f, o, nil)
	if err != nil || !result.Changed {
		t.Fatal("rearm", err)
	}
	after, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	want := before.Head.Spec
	want.CommitSequence++
	want.NotifySequence, want.NotifySerial = want.CommitSequence, want.Serial
	if after.Head.Spec != want || result.Head.UID != before.Head.UID || result.Head.ResourceVersion == before.Head.ResourceVersion || !reflect.DeepEqual(after.Aggregate, before.Aggregate) {
		t.Fatal("rearm altered data/evidence")
	}
	next, err := f.s.ReadGeneration(f.ctx())
	if err != nil || next.Revision != image.Revision+1 {
		t.Fatal("not one durable command", err)
	}
	if _, err := requestNotify(f.ctx(), t, f, o, nil); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("adopted newer head", err)
	}
	o = notifyObservation(t, f, l)
	snap := snapshot(t, f.s)
	checks := 0
	result, err = requestNotify(f.ctx(), t, f, o, func(context.Context) error { checks++; return nil })
	if err != nil || result.Changed || checks != 1 || !reflect.DeepEqual(result.Head, after.Head) || !bytes.Equal(snap, snapshot(t, f.s)) {
		t.Fatal("pending coalescing mutated/adopted", checks, err)
	}
	// Final admission failure rolls back both changed and unchanged results.
	if _, err := requestNotify(f.ctx(), t, f, o, func(context.Context) error { return pr.ErrUnavailable }); !errors.Is(err, pr.ErrUnavailable) || !bytes.Equal(snap, snapshot(t, f.s)) {
		t.Fatal("no-op bypassed final fence", err)
	}
}

func TestRequestNotifyTxPolicyNoneAndMalformedIntent(t *testing.T) {
	for _, mode := range []public.NotifyMode{public.NotifyNone, public.NotifySpecified} {
		t.Run(string(mode), func(t *testing.T) {
			f := setup(t)
			l := notifyActive(t, f, mode)
			o := notifyObservation(t, f, l)
			before := snapshot(t, f.s)
			if mode == public.NotifyNone {
				r, err := requestNotify(f.ctx(), t, f, o, nil)
				if err != nil || r.Changed || !bytes.Equal(before, snapshot(t, f.s)) {
					t.Fatal("policy-none wrote", err)
				}
				// Disabling delivery policy must not implicitly acknowledge an
				// existing valid intent or normalize its metadata.
				var h zk.ZoneState
				if err := f.s.Get(f.ctx(), store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(l.Spec.HeadZone())}, &h); err != nil {
					t.Fatal(err)
				}
				h.Spec.NotifySequence, h.Spec.NotifySerial = h.Spec.CommitSequence, h.Spec.Serial
				if _, err := f.s.Apply(f.ctx(), &h, store.ApplyOptions{Manager: "zonestate", ExpectedResourceVersion: h.ResourceVersion}); err != nil {
					t.Fatal(err)
				}
				o = notifyObservation(t, f, l)
				before = snapshot(t, f.s)
				if r, err := requestNotify(f.ctx(), t, f, o, nil); err != nil || r.Changed || r.Head.Spec.NotifySequence == 0 || !bytes.Equal(before, snapshot(t, f.s)) {
					t.Fatal("policy-none acknowledged pending intent", err)
				}
			}
			for _, fields := range []struct {
				sequence uint64
				serial   uint32
			}{{1, o.Serial + 1}, {0, 1}} {
				var head zk.ZoneState
				if err := f.s.Get(f.ctx(), store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(l.Spec.HeadZone())}, &head); err != nil {
					t.Fatal(err)
				}
				head.Spec.NotifySequence, head.Spec.NotifySerial = fields.sequence, fields.serial
				if _, err := f.s.Apply(f.ctx(), &head, store.ApplyOptions{Manager: "zonestate", ExpectedResourceVersion: head.ResourceVersion}); err != nil {
					t.Fatal(err)
				}
				o = notifyObservation(t, f, l)
				before = snapshot(t, f.s)
				if _, err := requestNotify(f.ctx(), t, f, o, nil); !errors.Is(err, pr.ErrInvalid) || !bytes.Equal(before, snapshot(t, f.s)) {
					t.Fatal("malformed intent accepted", fields, err)
				}
			}
		})
	}
}

func TestRequestNotifyTxExactEvidenceAndFinalRollback(t *testing.T) {
	f := setup(t)
	l := notifyActive(t, f, public.NotifySpecified)
	o := notifyObservation(t, f, l)
	before := snapshot(t, f.s)
	for _, change := range []func(*pr.PrimaryStatusObservation){
		func(o *pr.PrimaryStatusObservation) { o.LifecycleResourceVersion = "stale" },
		func(o *pr.PrimaryStatusObservation) { o.HeadResourceVersion = "stale" },
		func(o *pr.PrimaryStatusObservation) { o.Source.ResourceVersion = "stale" },
		func(o *pr.PrimaryStatusObservation) { o.CommittedSource.ResourceVersion = "stale" },
		func(o *pr.PrimaryStatusObservation) { o.Member.Sequence++ },
		func(o *pr.PrimaryStatusObservation) { o.Member.Incarnation++ },
		func(o *pr.PrimaryStatusObservation) { o.Member.Version.CommitSequence++ },
		func(o *pr.PrimaryStatusObservation) { o.Member.Version.RootDigest = zk.Digest([]byte("other")) },
		func(o *pr.PrimaryStatusObservation) { o.Member.Zone.UID = "recreated" },
		func(o *pr.PrimaryStatusObservation) { o.HeadZone.UID = "substituted" },
		func(o *pr.PrimaryStatusObservation) { o.Serial++ },
		func(o *pr.PrimaryStatusObservation) { o.ConfigMatchesDesired = false },
	} {
		bad := o
		change(&bad)
		if _, err := requestNotify(f.ctx(), t, f, bad, nil); err == nil {
			t.Fatal("stale observation admitted", bad)
		}
	}
	for _, ctx := range []context.Context{context.Background(), pr.WithPublicationFence(f.ctx(), pr.PublicationFence{Term: f.fence.Term, Incarnation: f.fence.Incarnation + 1})} {
		if _, err := requestNotify(ctx, t, f, o, nil); !errors.Is(err, pr.ErrUnavailable) {
			t.Fatal("sampled successor fence", err)
		}
	}
	if _, err := requestNotify(f.ctx(), t, f, o, func(context.Context) error { return pr.ErrUnavailable }); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("changed intent passed final rejection", err)
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("denial leaked head/chunks/audit/revision")
	}
	// A newly desired child denies even though no lifecycle/head exists yet.
	f.zone("late-child", "child.example.")
	before = snapshot(t, f.s)
	if _, err := requestNotify(f.ctx(), t, f, o, nil); !errors.Is(err, pr.ErrConflict) || !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("late child ignored", err)
	}
}

func TestRequestNotifyTxStatusOnlyReceiptAndEffectiveHeadIdentity(t *testing.T) {
	f := setup(t)
	l := notifyActive(t, f, public.NotifySpecified)
	before := notifyObservation(t, f, l)
	var z public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "primary"}, &z); err != nil {
		t.Fatal(err)
	}
	z.Status.Serial, z.Status.ContentHash, z.Status.RecordCount = before.Serial, before.ContentHash, before.RecordCount
	if err := f.s.UpdateStatus(f.ctx(), &z); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	l, err := f.r.ReadLifecycle(f.ctx(), l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	o := notifyObservation(t, f, l)
	if o.CommittedSource != before.CommittedSource || o.Source.ResourceVersion == o.CommittedSource.ResourceVersion {
		t.Fatal("fixture lost distinct current/committed RVs")
	}
	if r, err := requestNotify(f.ctx(), t, f, o, nil); err != nil || !r.Changed {
		t.Fatal("status-only source incorrectly denied", err)
	}
	// Retire and activate a later incarnation of this same actual source UID.
	retired, err := f.r.Retire(f.ctx(), l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Get(f.ctx(), store.KeyOf(&z), &z); err != nil {
		t.Fatal(err)
	}
	next := f.intent(&z)
	next.Spec.Incarnation = retired.Spec.Incarnation + 1
	next.Spec.Sequence = retired.Spec.Sequence + 1
	next, err = f.r.Transition(f.ctx(), retired, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(f.ctx(), next.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	next, err = f.r.ReadLifecycle(f.ctx(), next.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	o = notifyObservation(t, f, next)
	if o.HeadZone.UID == o.Source.UID || o.Member.Zone.UID != o.Source.UID || !o.ConfigMatchesDesired {
		t.Fatal("fixture lacks effective head identity", o)
	}
	if r, err := requestNotify(f.ctx(), t, f, o, nil); err != nil || !r.Changed {
		t.Fatal("effective UID rejected/adopted actual UID", err)
	}
}

func TestRequestNotifyTxReplicaRestartPromotionAndOldAckCAS(t *testing.T) {
	f := setup(t)
	l := notifyActive(t, f, public.NotifySpecified)
	o := notifyObservation(t, f, l)
	before, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := requestNotify(f.ctx(), t, f, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An acknowledgment constructed before this intent cannot clear it.
	staleAck, err := zonestate.Prepare(before.Aggregate, before.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.CommitZoneState(ctx, before.Expected, staleAck, zonestate.Limits{}, nil)
		return err
	})
	if !errors.Is(err, zonestate.ErrConflict) {
		t.Fatal("old ack cleared later intent", err)
	}
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zk.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replica.db")
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InitialMode: replication.Follower})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Enroll(f.ctx(), f.fence.Term); err != nil {
		t.Fatal(err)
	}
	image, err := f.s.ReadGeneration(f.ctx())
	if err != nil {
		t.Fatal(err)
	}
	validated, err := s.ValidateImage(f.ctx(), *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallReplica(f.ctx(), validated, replication.InstallOptions{ExpectedTerm: s.Authority().Term}); err != nil {
		t.Fatal(err)
	}
	read := func() *zk.ZoneState {
		t.Helper()
		var h zk.ZoneState
		if err := s.Get(t.Context(), store.Key{Kind: zk.KindZoneState, Name: intent.Head.Name}, &h); err != nil {
			t.Fatal(err)
		}
		return &h
	}
	if !reflect.DeepEqual(read(), intent.Head) {
		t.Fatal("replica changed intent/UID/RV")
	}
	a, err := New(Options{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(f.ctx(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := a.RequestNotifyTx(ctx, tx, o)
		return err
	})
	if err == nil {
		t.Fatal("follower admitted manual command")
	}
	installed, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: s.Authority().Term, ExpectedInstalled: installed.Generation(), RequestID: "notify-recovery"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read(), intent.Head) {
		t.Fatal("promotion renewed/rearmed intent")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(path, sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read(), intent.Head) {
		t.Fatal("restart changed durable intent")
	}
	a, err = New(Options{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	// Old request cannot adopt the promoted writer even though its data matches.
	err = s.Command(f.ctx(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := a.RequestNotifyTx(ctx, tx, o)
		return err
	})
	if !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("old request adopted promoted writer", err)
	}
	fresh := pr.WithPublicationFence(t.Context(), pr.PublicationFence{Term: s.Authority().Term, Incarnation: s.Authority().Incarnation})
	// New admitted episode coalesces the SAME restored intent; no data rebuild.
	current := o
	current.HeadResourceVersion = intent.Head.ResourceVersion
	current.Member.Version.CommitSequence = intent.Head.Spec.CommitSequence
	var r NotifyIntentResult
	err = s.Command(fresh, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var e error
		r, e = a.RequestNotifyTx(ctx, tx, current)
		return e
	})
	if err != nil || r.Changed || !reflect.DeepEqual(r.Head, intent.Head) {
		t.Fatal("new episode did not recover exact pending intent", err)
	}
}
