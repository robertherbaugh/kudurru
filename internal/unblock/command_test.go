package unblock

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func (f *fixture) command(r *public.UnblockRequest, action Action, d Decision, check func(context.Context) error) (*Outcome, error) {
	f.t.Helper()
	st := f.st.(*sqlite.Store)
	var out *Outcome
	err := st.Command(f.t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if check != nil {
			if err := tx.RequireCommitCheck(check); err != nil {
				return err
			}
		}
		var err error
		out, err = ExecuteCurrent(ctx, tx, Target{Namespace: r.Namespace, Name: r.Name, Action: action}, d, f.clk.Now())
		return err
	})
	return out, err
}
func (f *fixture) current(r *public.UnblockRequest) *public.UnblockRequest {
	f.t.Helper()
	var out public.UnblockRequest
	if err := f.st.Get(f.t.Context(), store.KeyOf(r), &out); err != nil {
		f.t.Fatal(err)
	}
	return &out
}
func (f *fixture) sweepCurrent() (SweepResult, error) {
	st := f.st.(*sqlite.Store)
	var out SweepResult
	err := st.Command(f.t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		out, err = SweepCurrent(ctx, tx, f.clk.Now(), f.set.PendingExpiry.Std(), f.set.Retention.Std())
		return err
	})
	return out, err
}
func TestCurrentUnblockSharedOwnershipRevokeAndExpiry(t *testing.T) {
	f := newFixture(t)
	first := f.submit("192.0.2.1", "shared.example", "")
	second := f.submit("192.0.2.2", "shared.example", "")
	a, err := f.command(first, Approve, Decision{Duration: time.Hour, Actor: "User:decider"}, nil)
	if err != nil || !a.Request.Status.Allow.Added || len(a.Attached) != 2 {
		t.Fatal(a, err)
	}
	b, err := f.command(second, Approve, Decision{Duration: 2 * time.Hour}, nil)
	if err != nil || b.Request.Status.Allow.Added {
		t.Fatal(b, err)
	}
	if _, err := f.command(first, Deny, Decision{Note: "revoked"}, nil); err != nil {
		t.Fatal(err)
	}
	heir := f.current(second)
	if !heir.Status.Allow.Added || len(f.list(EveryoneList)) != 1 || f.current(first).EffectiveState() != public.UnblockDenied {
		t.Fatal("shared entry ownership was lost")
	}
	f.clk.Add(2 * time.Hour)
	res, err := f.sweepCurrent()
	if err != nil || res.Ended != 1 || len(f.list(EveryoneList)) != 0 || f.current(second).Status.Allow.Removed == nil {
		t.Fatal("atomic expiry failed", res, err)
	}
	for _, name := range []string{"default", "kids"} {
		var pol public.Policy
		if err := f.st.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: name}, &pol); err != nil || len(pol.Spec.AllowListRefs) != 0 {
			t.Fatal("empty list reference survived", err)
		}
	}
	before, _ := f.st.(*sqlite.Store).ReadGeneration(t.Context())
	res, err = f.sweepCurrent()
	after, _ := f.st.(*sqlite.Store).ReadGeneration(t.Context())
	if err != nil || res != (SweepResult{}) || !before.Generation().Same(after.Generation()) {
		t.Fatal("empty sweep changed durable state", err)
	}
}
func TestCurrentUnblockAtomicRollbackAndManagedOwners(t *testing.T) {
	for _, action := range []Action{Approve, Deny, Delete} {
		t.Run(string(action), func(t *testing.T) {
			f := newFixture(t)
			r := f.submit("192.0.2.1", "rollback.example", "")
			if action != Approve {
				if _, err := f.command(r, Approve, Decision{}, nil); err != nil {
					t.Fatal(err)
				}
			}
			st := f.st.(*sqlite.Store)
			before, _ := st.ReadGeneration(t.Context())
			want := f.current(r)
			refusal := errors.New("original writer expired")
			if _, err := f.command(r, action, Decision{Duration: time.Hour}, func(context.Context) error { return refusal }); !errors.Is(err, refusal) {
				t.Fatal("final check omitted", err)
			}
			after, _ := st.ReadGeneration(t.Context())
			if !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(want, f.current(r)) {
				t.Fatal("failed command committed partial effects")
			}
		})
	}
	f := newFixture(t)
	r := f.submit("192.0.2.1", "managed.example", "")
	blocked := &public.BlockList{ObjectMeta: meta(EveryoneList), Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Inline: []string{"other.example"}}}}}
	if _, err := f.st.Apply(t.Context(), blocked, store.ApplyOptions{Manager: store.ManagerFile("lists.yaml")}); err != nil {
		t.Fatal(err)
	}
	before, _ := f.st.(*sqlite.Store).ReadGeneration(t.Context())
	var managed *store.ErrManagedBy
	if _, err := f.command(r, Approve, Decision{}, nil); !errors.As(err, &managed) {
		t.Fatal("managed list accepted", err)
	}
	after, _ := f.st.(*sqlite.Store).ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("manager refusal changed state")
	}
	// Status-only commands cannot decide objects belonging to arbitrary managers.
	if _, err := f.st.Apply(t.Context(), r, store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.command(r, Deny, Decision{}, nil); !errors.As(err, &managed) {
		t.Fatal("foreign request owner accepted", err)
	}
}
func TestCurrentUnblockManualEntryGroupAndSkippedPolicy(t *testing.T) {
	f := newFixture(t)
	r := f.submit("192.0.2.1", "manual.example", "")
	bl := &public.BlockList{ObjectMeta: meta(GroupListPrefix + "kids"), Spec: public.BlockListSpec{Mode: "allow", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Value: "manual.example", Type: public.EntryExact, Origin: public.OriginManual, Comment: "keep me"}}}}}}
	if _, err := f.st.Apply(t.Context(), bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var pol public.Policy
	if err := f.st.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "kids"}, &pol); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Apply(t.Context(), &pol, store.ApplyOptions{Manager: store.ManagerFile("policy.yaml"), Force: true}); err != nil {
		t.Fatal(err)
	}
	out, err := f.command(r, Approve, Decision{Scope: ScopeGroup, Duration: time.Hour}, nil)
	if err != nil || out.Request.Status.Allow.Added || len(out.Attached) != 0 || !reflect.DeepEqual(out.Skipped, []string{"default/kids"}) {
		t.Fatal(out, err)
	}
	if _, err := f.command(r, Deny, Decision{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.list(GroupListPrefix + "kids"); len(got) != 1 || got[0] != "manual.example" {
		t.Fatal("manual entry removed", got)
	}
}
func TestCurrentUnblockReapproveExpiredAndDeleteSafety(t *testing.T) {
	f := newFixture(t)
	r := f.submit("192.0.2.1", "renew.example", "")
	if _, err := f.command(r, Approve, Decision{Duration: time.Hour}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.command(r, Approve, Decision{}, nil); !errors.Is(err, ErrState) {
		t.Fatal("active approval overwritten", err)
	}
	if _, err := f.command(r, Delete, Decision{}, nil); !errors.Is(err, ErrState) {
		t.Fatal("live timed approval deleted", err)
	}
	f.clk.Add(time.Hour)
	out, err := f.command(r, Approve, Decision{Scope: ScopeGroup, Match: MatchSubdomains, Duration: time.Hour}, nil)
	if err != nil || !out.Request.Status.Allow.Added || out.Request.Status.Allow.Entry != "*.renew.example" || len(f.list(EveryoneList)) != 0 || len(f.list(GroupListPrefix+"kids")) != 1 {
		t.Fatal("expired renewal failed", out, err)
	}
	f.clk.Add(time.Hour)
	if _, err := f.command(r, Delete, Decision{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.list(GroupListPrefix+"kids")) != 0 {
		t.Fatal("deleting unswept expired approval orphaned its entry")
	}
}
func TestCurrentUnblockRejectsForgedDerivedEffects(t *testing.T) {
	f := newFixture(t)
	r := f.submit("192.0.2.1", "bounded.example", "")
	out, err := f.command(r, Approve, Decision{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Request.Status.Allow.List = "default/unrelated"
	if err := f.st.UpdateStatus(t.Context(), out.Request); err != nil {
		t.Fatal(err)
	}
	before, _ := f.st.(*sqlite.Store).ReadGeneration(t.Context())
	if _, err := f.command(r, Deny, Decision{}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("forged list granted removal authority", err)
	}
	after, _ := f.st.(*sqlite.Store).ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("forged effects changed state")
	}
}
func TestCurrentUnblockExpiryRollbackAndPendingRetention(t *testing.T) {
	f := newFixture(t)
	r := f.submit("192.0.2.1", "expire.example", "")
	if _, err := f.command(r, Approve, Decision{Duration: time.Hour}, nil); err != nil {
		t.Fatal(err)
	}
	pending := f.submit("192.0.2.2", "pending.example", "")
	f.clk.Add(f.set.PendingExpiry.Std())
	st := f.st.(*sqlite.Store)
	before, _ := st.ReadGeneration(t.Context())
	refusal := errors.New("system writer ended")
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.RequireCommitCheck(func(context.Context) error { return refusal }); err != nil {
			return err
		}
		_, err := SweepCurrent(ctx, tx, f.clk.Now(), f.set.PendingExpiry.Std(), f.set.Retention.Std())
		return err
	})
	after, _ := st.ReadGeneration(t.Context())
	if !errors.Is(err, refusal) || !before.Generation().Same(after.Generation()) || len(f.list(EveryoneList)) != 1 {
		t.Fatal("failed sweep committed partial effects", err)
	}
	res, err := f.sweepCurrent()
	if err != nil || res.Ended != 1 || res.Expired != 1 || f.current(pending).EffectiveState() != public.UnblockExpired {
		t.Fatal(res, err)
	}
	f.clk.Add(f.set.Retention.Std())
	res, err = f.sweepCurrent()
	if err != nil || res.Pruned != 2 {
		t.Fatal(res, err)
	}
}

func TestCurrentUnblockSweepReusesBoundedRequestCut(t *testing.T) {
	f := newFixture(t)
	for i := range 64 {
		r := f.submit(fmt.Sprintf("192.0.2.%d", i+1), "shared-expiry.example", "")
		if _, err := f.command(r, Approve, Decision{Duration: time.Hour}, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.clk.Add(time.Hour)
	res, err := f.sweepCurrent()
	if err != nil || res.Ended != 64 || len(f.list(EveryoneList)) != 0 {
		t.Fatal("bounded shared-entry expiry failed", res, err)
	}
}

func TestCurrentUnblockLateStatusFailureRollsBackDerivedWrites(t *testing.T) {
	f := newFixture(t)
	r := f.submit("192.0.2.1", "late-failure.example", "")
	st := f.st.(*sqlite.Store)
	before, _ := st.ReadGeneration(t.Context())
	// The list and policy stages precede the request's schema validation. A late
	// status refusal must roll all of them back in the real SQLite transaction.
	if _, err := f.command(r, Approve, Decision{Actor: strings.Repeat("x", 301)}, nil); err == nil {
		t.Fatal("invalid late request status accepted")
	}
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || len(f.list(EveryoneList)) != 0 || f.current(r).EffectiveState() != public.UnblockPending {
		t.Fatal("late request failure leaked derived writes")
	}
	if _, err := f.command(r, Approve, Decision{Duration: time.Hour}, nil); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(time.Hour)
	before, _ = st.ReadGeneration(t.Context())
	if _, err := f.command(r, Approve, Decision{Scope: ScopeGroup, Group: "missing"}, nil); err == nil {
		t.Fatal("missing scope accepted")
	}
	after, _ = st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || len(f.list(EveryoneList)) != 1 || f.current(r).Status.Allow.Removed != nil {
		t.Fatal("failed renewal released prior ownership")
	}
}
