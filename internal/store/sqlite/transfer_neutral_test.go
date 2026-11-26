package sqlite

import (
	"context"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"github.com/robertherbaugh/kudurru/internal/zonestate/zonestatetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The bookkeeping commits around a zone transfer, a NOTIFY acknowledgment, a
// primary zone's status projection and the receipt realignment after it,
// keep a publication's transfer answers current (D419). Every other commit,
// a commit that mixes one in, and an authority this store did not publish
// through a noted commit, retire them exactly as an exact token comparison
// does.
func TestTransferNeutralCommits(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, zoneStateOptions)
	r, _ := zoneStateRepository(t, s)
	id := zoneStateID(s, "neutral")
	v := zonestatetest.Initialize(t, r, id)
	// A data commit arms the notification intent that the acknowledgment
	// below clears.
	p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"new.example. 60 IN A 192.0.2.8"}))
	if err != nil {
		t.Fatal(err)
	}
	if v, err = r.Commit(ctx, v.Expected, p, nil); err != nil {
		t.Fatal(err)
	}
	if v.Head.Spec.NotifySequence == 0 {
		t.Fatal("the data commit armed no notification intent")
	}
	primary := zone("home", "example.")
	if _, err := s.Apply(ctx, primary, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	forward := zone("fwd", "forward.example.")
	forward.Spec.Type, forward.Spec.Forward = v1alpha1.ZoneTypeForward, &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
	if _, err := s.Apply(ctx, forward, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	lifecycle := &zonekinds.ZoneLifecycle{ObjectMeta: metav1.ObjectMeta{Name: zonekinds.LifecycleName(id.ClusterID, id.ObjectName)}, Spec: zonekinds.ZoneLifecycleSpec{
		FormatVersion: zonekinds.FormatVersion, Zone: id, Type: v1alpha1.ZoneTypePrimary, Incarnation: 1, Sequence: 1, Phase: zonekinds.PhaseActive,
		Source: zonekinds.LifecycleSource{UID: id.UID, ResourceVersion: "1", IntentDigest: zonekinds.Digest([]byte("intent"))},
	}}
	if _, err := s.Apply(ctx, lifecycle, store.ApplyOptions{Manager: "primaryruntime"}); err != nil {
		t.Fatal(err)
	}

	token := func(t *testing.T) GenerationToken {
		t.Helper()
		_, tok, err := s.ReadServingCandidate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	neutral := func(t *testing.T, tok GenerationToken) bool {
		t.Helper()
		a := s.Authority()
		if tok.Matches(a) {
			t.Fatal("no commit moved the authority past the token")
		}
		return s.TransferNeutral(tok, a)
	}
	statusWrite := func(t *testing.T, name string, change func(*v1alpha1.DNSZoneStatus)) {
		t.Helper()
		var z v1alpha1.DNSZone
		if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
			t.Fatal(err)
		}
		change(&z.Status)
		if err := s.UpdateStatus(ctx, &z); err != nil {
			t.Fatal(err)
		}
	}
	realign := func(t *testing.T, change func(*zonekinds.ZoneLifecycleSpec)) {
		t.Helper()
		var l zonekinds.ZoneLifecycle
		if err := s.Get(ctx, store.Key{Kind: zonekinds.KindZoneLifecycle, Name: lifecycle.Name}, &l); err != nil {
			t.Fatal(err)
		}
		change(&l.Spec)
		if _, err := s.Apply(ctx, &l, store.ApplyOptions{Manager: "primaryruntime", ExpectedResourceVersion: l.ResourceVersion}); err != nil {
			t.Fatal(err)
		}
	}
	notifyAck := func(t *testing.T) {
		t.Helper()
		ack, err := zonestate.Prepare(v.Aggregate, v.Head, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		ack.Head.Spec.NotifySequence, ack.Head.Spec.NotifySerial = 0, 0
		if v, err = r.Commit(ctx, v.Expected, ack, nil); err != nil {
			t.Fatal(err)
		}
	}

	base := token(t)
	notifyAck(t)
	if !neutral(t, base) {
		t.Fatal("a NOTIFY acknowledgment retired the transfer answers")
	}
	statusWrite(t, "home", func(st *v1alpha1.DNSZoneStatus) { st.Serial, st.RecordCount = 7, 3 })
	realign(t, func(l *zonekinds.ZoneLifecycleSpec) { l.Sequence++; l.Source.ResourceVersion = "2" })
	if !neutral(t, base) {
		t.Fatal("a primary status projection and its receipt realignment retired the transfer answers")
	}
	if !s.TransferNeutral(token(t), s.Authority()) {
		t.Fatal("the current token is not transfer-current")
	}

	// Each of these changes something a transfer, or its refusal, may be
	// computed from, or is not one of the recognized bookkeeping shapes.
	retiring := []struct {
		name  string
		write func(t *testing.T)
	}{
		{"zone data", func(t *testing.T) {
			p, err := zonestatetest.Candidate(v, zonestatetest.Message(t, nil, []string{"newer.example. 60 IN A 192.0.2.9"}))
			if err != nil {
				t.Fatal(err)
			}
			if v, err = r.Commit(ctx, v.Expected, p, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"name server addresses", func(t *testing.T) {
			statusWrite(t, "home", func(st *v1alpha1.DNSZoneStatus) { st.NameserverAddresses = append(st.NameserverAddresses, "192.0.2.1") })
		}},
		{"nonprimary status", func(t *testing.T) {
			statusWrite(t, "fwd", func(st *v1alpha1.DNSZoneStatus) { st.RecordCount++ })
		}},
		{"zone configuration", func(t *testing.T) {
			var z v1alpha1.DNSZone
			if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
				t.Fatal(err)
			}
			z.Spec.Policy.TransferACL = []string{"192.0.2.0/24"}
			if _, err := s.Apply(ctx, &z, store.ApplyOptions{Manager: "test"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"lifecycle phase", func(t *testing.T) {
			realign(t, func(l *zonekinds.ZoneLifecycleSpec) { l.Sequence++; l.Phase = zonekinds.PhaseRetiring })
		}},
		{"unrelated object", func(t *testing.T) {
			if _, err := s.Apply(ctx, zone("other", "other.example."), store.ApplyOptions{Manager: "test"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"neutral and other write in one commit", func(t *testing.T) {
			if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				var home v1alpha1.DNSZone
				if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &home); err != nil {
					return err
				}
				home.Status.RecordCount++
				if err := tx.UpdateStatus(ctx, &home); err != nil {
					return err
				}
				var z v1alpha1.DNSZone
				if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: "other"}, &z); err != nil {
					return err
				}
				z.Labels = map[string]string{"changed": "yes"}
				_, err := tx.Apply(ctx, &z, store.ApplyOptions{Manager: "test"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range retiring {
		t.Run(tc.name, func(t *testing.T) {
			before := token(t)
			tc.write(t)
			if neutral(t, before) {
				t.Fatal("the commit kept the transfer answers current")
			}
			if neutral(t, base) {
				t.Fatal("a neutral run continued across the commit")
			}
		})
	}

	// A neutral commit after a retiring one starts a new run: only tokens
	// from the retiring commit on are covered.
	after := token(t)
	statusWrite(t, "home", func(st *v1alpha1.DNSZoneStatus) { st.RecordCount++ })
	if !neutral(t, after) || neutral(t, base) {
		t.Fatal("the neutral run after a retiring commit has the wrong start")
	}
}
