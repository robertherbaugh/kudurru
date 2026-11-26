package sqlite

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
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

// A transfer-neutral run ends at every authority change the store does not
// note as neutral (D419): an ApplyCluster commit with a section row (noted as
// not neutral), an authority transition and a fence (never noted, so the
// published authority differs from the run's). A neutral commit after one of
// them starts a new run at its own predecessor revision, so only tokens from
// that revision on are covered.
func TestTransferNeutralRunEndsAtSectionsTransitionsAndFences(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Store, func() GenerationToken, func()) {
		t.Helper()
		s := openTemp(t, zoneStateOptions)
		if _, err := s.Apply(ctx, zone("home", "example."), store.ApplyOptions{Manager: "test"}); err != nil {
			t.Fatal(err)
		}
		token := func() GenerationToken {
			t.Helper()
			_, tok, err := s.ReadServingCandidate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}
		statusWrite := func() {
			t.Helper()
			var z v1alpha1.DNSZone
			if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
				t.Fatal(err)
			}
			z.Status.RecordCount++
			if err := s.UpdateStatus(ctx, &z); err != nil {
				t.Fatal(err)
			}
		}
		return s, token, statusWrite
	}
	covered := func(s *Store, tok GenerationToken) bool { return s.TransferNeutral(tok, s.Authority()) }
	// newRun checks that the latest (neutral) commit started a new run at its
	// predecessor revision and that the run does not reach back before the
	// change.
	newRun := func(t *testing.T, s *Store, before GenerationToken) {
		t.Helper()
		run, a := s.transfer.Load(), s.Authority()
		if run == nil || !reflect.DeepEqual(run.authority, a) || run.from != a.Installed.Revision-1 {
			t.Fatalf("the neutral commit after the change did not start a new run at its predecessor: %+v at %d", run, a.Installed.Revision)
		}
		if covered(s, before) {
			t.Fatal("the new run covers a token from before the change")
		}
	}
	// startRun commits one neutral write and checks that it covers base.
	startRun := func(t *testing.T, s *Store, base GenerationToken, statusWrite func()) {
		t.Helper()
		statusWrite()
		if !covered(s, base) {
			t.Fatal("the status projection did not start a run")
		}
	}

	t.Run("ApplyCluster section row", func(t *testing.T) {
		s, token, statusWrite := setup(t)
		base := token()
		startRun(t, s, base, statusWrite)
		if _, err := s.ApplyCluster(ctx, nil, store.ApplyOptions{}, []SectionChange{{Section: "runtime", Key: "foreign/witness", Data: json.RawMessage(`{"intact":true}`)}}); err != nil {
			t.Fatal(err)
		}
		if covered(s, base) {
			t.Fatal("a run continued across an ApplyCluster section row")
		}
		if run := s.transfer.Load(); run == nil || !reflect.DeepEqual(run.authority, s.Authority()) || run.from != s.Authority().Installed.Revision {
			t.Fatal("the section commit was not noted as an empty run at its own revision", run)
		}
		after := token()
		statusWrite()
		newRun(t, s, base)
		if !covered(s, after) {
			t.Fatal("the run after the section row does not cover its own start")
		}
	})

	t.Run("authority transition", func(t *testing.T) {
		s, token, statusWrite := setup(t)
		base := token()
		startRun(t, s, base, statusWrite)
		before := s.Authority()
		if _, err := s.EnablePrimary(ctx, imageOf(t, s).Generation()); err != nil {
			t.Fatal(err)
		}
		if a := s.Authority(); a.Mode != replication.Primary || a.Incarnation == before.Incarnation {
			t.Fatal("EnablePrimary did not transition the authority", a)
		}
		if covered(s, base) || reflect.DeepEqual(s.transfer.Load().authority, s.Authority()) {
			t.Fatal("a run continued across an authority transition")
		}
		transitioned := token()
		statusWrite()
		newRun(t, s, base)
		// The transition stamped the installed digest that the first commit
		// of the new authority replaces, so a token from the transition
		// itself stays uncovered (strict); later tokens are covered.
		if covered(s, transitioned) {
			t.Fatal("a token whose installed digest the commit replaced was covered")
		}
		after := token()
		statusWrite()
		if !covered(s, after) || covered(s, base) {
			t.Fatal("the run after the transition has the wrong start")
		}
	})

	t.Run("fence", func(t *testing.T) {
		s, token, statusWrite := setup(t)
		base := token()
		startRun(t, s, base, statusWrite)
		term := s.Authority().Term
		term.Epoch++
		term.PrimaryID, term.PromotionID = "other-primary", "other-promotion"
		if _, err := s.ObserveAuthority(ctx, term); err != nil {
			t.Fatal(err)
		}
		if a := s.Authority(); a.Mode != replication.Fenced {
			t.Fatal("the observation did not fence the store", a)
		}
		if covered(s, base) || reflect.DeepEqual(s.transfer.Load().authority, s.Authority()) {
			t.Fatal("a run continued across a fence")
		}
	})
}
