package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

type sourceEffectProbe struct {
	inner  objects.RoleBindingEffects
	before func(context.Context, objects.RoleBindingPlan)
	after  func(error)
}

func (p sourceEffectProbe) ApplyRoleBinding(ctx context.Context, plan objects.RoleBindingPlan) (*public.RoleBinding, error) {
	if p.before != nil {
		p.before(ctx, plan)
	}
	out, err := p.inner.ApplyRoleBinding(ctx, plan)
	if p.after != nil {
		p.after(err)
	}
	return out, err
}
func (p sourceEffectProbe) DeleteRoleBinding(ctx context.Context, plan objects.RoleBindingPlan) error {
	if p.before != nil {
		p.before(ctx, plan)
	}
	err := p.inner.DeleteRoleBinding(ctx, plan)
	if p.after != nil {
		p.after(err)
	}
	return err
}

func sourceBindingFile(t *testing.T, dir string, present bool, role string) {
	t.Helper()
	var raw []byte
	if present {
		desired := roleBindingManifest("source-operator", role, "corp:alice")
		desired.Spec.Scope.Kind = "cluster"
		var err error
		raw, err = json.Marshal(desired)
		if err != nil {
			t.Fatal("source encode")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), raw, 0o600); err != nil {
		t.Fatal("source write")
	}
}

func sourceLoader(t *testing.T, st *sqlite.Store, effects objects.RoleBindingEffects, dir string) *objects.Loader {
	t.Helper()
	l, err := objects.New(objects.Options{Store: st, RoleBindings: effects, RequireRoleBindings: true, Dir: dir, PollInterval: -1})
	if err != nil {
		t.Fatal("source loader construction")
	}
	return l
}
func sourceWriter(t *testing.T, st *sqlite.Store) context.Context {
	t.Helper()
	token, err := st.WriterToken()
	if err != nil {
		t.Fatal("source original writer")
	}
	return sqlite.WithWriterToken(t.Context(), token)
}
func sourceBinding(t *testing.T, st *sqlite.Store) *public.RoleBinding {
	t.Helper()
	var b public.RoleBinding
	err := st.Get(t.Context(), store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "source-operator"}, &b)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal("source binding read")
	}
	return &b
}

func TestSQLiteObjectSourceExactTargetPlans(t *testing.T) {
	for _, mode := range []string{"create-only", "rv-change", "replacement", "manager-change", "delete-rv-change"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			st := f.s.opts.Store
			ctx := sourceWriter(t, st)
			dir := t.TempDir()
			sourceBindingFile(t, dir, true, "operator")
			adapter := newSQLiteObjectSources(st)
			if mode != "create-only" {
				if rep := sourceLoader(t, st, adapter, dir).Load(ctx); rep.Created != 1 || rep.Count(objects.SeverityError) != 0 {
					t.Fatal("initial source apply")
				}
			}
			if mode == "delete-rv-change" {
				sourceBindingFile(t, dir, false, "")
			}
			var reviewed, after *replication.Image
			calls := 0
			probe := sourceEffectProbe{inner: adapter, before: func(c context.Context, p objects.RoleBindingPlan) {
				calls++
				// Competing current target changes AFTER the loader's original Get/List
				// and before its transaction. The original plan must not be recomputed.
				token, ok := sqlite.WriterTokenFromContext(c)
				if !ok {
					t.Fatal("lost writer")
				}
				err := st.Command(c, token.Term(), func(c context.Context, tx *sqlite.CommandTx) error {
					if mode == "create-only" {
						desired := p.Desired()
						desired.Labels = map[string]string{"competitor": "created"}
						_, err := tx.Apply(c, desired, store.ApplyOptions{Manager: p.Manager(), CreateOnly: true})
						return err
					}
					current := p.Before()
					if mode == "replacement" {
						if err := tx.Delete(c, p.Key(), store.DeleteOptions{Manager: p.Manager()}); err != nil {
							return err
						}
						current.UID = ""
						current.ResourceVersion = ""
						current.CreationTimestamp.Time = time.Time{}
					}
					current.Labels = map[string]string{"competitor": "changed"}
					manager := p.Manager()
					if mode == "manager-change" {
						manager = store.ManagerAPI
					}
					_, err := tx.Apply(c, current, store.ApplyOptions{Manager: manager, Force: true})
					return err
				})
				if err != nil {
					t.Fatal("competing fixture target update")
				}
				reviewed = roleBindingImage(t, st)
			}, after: func(err error) {
				if !errors.Is(err, store.ErrConflict) {
					t.Fatal("original target mismatch did not refuse as conflict")
				}
				after = roleBindingImage(t, st)
			}}
			rep := sourceLoader(t, st, probe, dir).Load(ctx)
			if calls != 1 || rep.Count(objects.SeverityError) != 1 || rep.Created+rep.Updated+rep.Deleted+rep.Unchanged != 0 {
				t.Fatal("refused target was retried, hidden or ordinary CRUD used")
			}
			if reviewed == nil || after == nil || !reviewed.Generation().Same(after.Generation()) || !reflect.DeepEqual(reviewed.Payload, after.Payload) {
				t.Fatal("refusal changed target or security state")
			}
		})
	}
}

func TestSQLiteObjectSourceNoopAndOriginalWriter(t *testing.T) {
	for _, mode := range []string{"missing-token", "canceled-original", "stopped-original-context", "stale-incarnation"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			st := f.s.opts.Store
			original := sourceWriter(t, st)
			if mode == "stopped-original-context" {
				active := f.s.primary.writer.active.Load()
				if active == nil {
					t.Fatal("original elector context missing")
				}
				original = active.ctx
			}
			dir := t.TempDir()
			sourceBindingFile(t, dir, true, "operator")
			adapter := newSQLiteObjectSources(st)
			l := sourceLoader(t, st, adapter, dir)
			if rep := l.Load(original); rep.Created != 1 || rep.Count(objects.SeverityError) != 0 {
				t.Fatal("initial source")
			}
			before := roleBindingImage(t, st)
			if rep := l.Load(original); rep.Unchanged != 1 || rep.Count(objects.SeverityError) != 0 {
				t.Fatal("guarded no-op")
			}
			after := roleBindingImage(t, st)
			if !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
				t.Fatal("no-op changed canonical image")
			}
			// Each refusal is a distinct original invocation, never a retry of the
			// same refused operation with a newly sampled writer or context.
			attempted := original
			switch mode {
			case "missing-token":
				attempted = t.Context()
			case "canceled-original":
				canceled, cancel := context.WithCancel(original)
				cancel()
				attempted = canceled
			case "stopped-original-context":
				f.stop()
				if original.Err() == nil {
					t.Fatal("original elector context did not cancel")
				}
			case "stale-incarnation":
				f.stop()
				// Joining the elector alone does not replace durable authority.
				// Perform the actual same-term incarnation CAS, retaining the
				// original token and context for the refused source operation.
				oldToken, ok := sqlite.WriterTokenFromContext(original)
				if !ok {
					t.Fatal("original token missing")
				}
				generation := roleBindingImage(t, st).Generation()
				authority, err := st.EnablePrimary(t.Context(), generation)
				if err != nil || authority.Incarnation == oldToken.Incarnation() || !authority.Term.Same(oldToken.Term()) {
					t.Fatal("fixture did not replace actual writer incarnation")
				}
			}
			if rep := l.Load(attempted); rep.Count(objects.SeverityError) == 0 || rep.Unchanged != 0 {
				t.Fatal("invalid original writer/context bypassed no-op guard")
			}
			current := roleBindingImage(t, st)
			if !reflect.DeepEqual(after.Payload, current.Payload) {
				t.Fatal("refused no-op changed canonical objects/security")
			}
		})
	}
}

func TestSQLiteObjectSourceFinalTimeAndCancellation(t *testing.T) {
	for _, mode := range []string{"rollback", "cancel"} {
		for _, noop := range []bool{false, true} {
			label := mode + "/change"
			if noop {
				label = mode + "/noop"
			}
			t.Run(label, func(t *testing.T) {
				f := newServingCommandFixture(t)
				st := f.s.opts.Store
				original := sourceWriter(t, st)
				dir := t.TempDir()
				sourceBindingFile(t, dir, true, "operator")
				adapter := newSQLiteObjectSources(st)
				if rep := sourceLoader(t, st, adapter, dir).Load(original); rep.Created != 1 {
					t.Fatal("initial source")
				}
				if !noop {
					sourceBindingFile(t, dir, true, "viewer")
				}
				ctx, cancel := context.WithCancel(original)
				defer cancel()
				calls := 0
				var entered time.Time
				adapter.now = func() time.Time {
					calls++
					if calls == 1 {
						entered = time.Now().UTC()
						return entered
					}
					if mode == "cancel" {
						cancel()
						return entered
					}
					return entered.Add(-time.Nanosecond)
				}
				before := roleBindingImage(t, st)
				rep := sourceLoader(t, st, adapter, dir).Load(ctx)
				if rep.Count(objects.SeverityError) != 1 || rep.Updated+rep.Unchanged != 0 || calls != 2 {
					t.Fatal("final original time/context was not checked")
				}
				after := roleBindingImage(t, st)
				if !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
					t.Fatal("failed final check committed object/floor")
				}
			})
		}
	}
}

func TestSQLiteObjectSourceWaitUsesActualEntryTime(t *testing.T) {
	f := newServingCommandFixture(t)
	st := f.s.opts.Store
	ctx := sourceWriter(t, st)
	dir := t.TempDir()
	sourceBindingFile(t, dir, true, "operator")
	adapter := newSQLiteObjectSources(st)
	if rep := sourceLoader(t, st, adapter, dir).Load(ctx); rep.Created != 1 {
		t.Fatal("initial source")
	}
	sourceBindingFile(t, dir, false, "")
	entered, release := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	token, _ := sqlite.WriterTokenFromContext(ctx)
	go func() {
		lockDone <- st.Command(ctx, token.Term(), func(context.Context, *sqlite.CommandTx) error { close(entered); <-release; return nil })
	}()
	<-entered
	accepted := make(chan struct{})
	var observed time.Time
	probe := sourceEffectProbe{inner: adapter, before: func(_ context.Context, p objects.RoleBindingPlan) { observed = p.ObservedAt(); close(accepted) }}
	done := make(chan objects.Report, 1)
	l := sourceLoader(t, st, probe, dir)
	var actualEntry time.Time
	var sampled atomic.Bool
	adapter.now = func() time.Time {
		now := time.Now().UTC()
		if !sampled.Swap(true) {
			actualEntry = now
		}
		return now
	}
	go func() { done <- l.Load(ctx) }()
	<-accepted
	// The adapter cannot sample its floor clock until AFTER the writer mutex is
	// released. The accepted source's earlier timestamp is only a lower bound.
	if sampled.Load() {
		t.Fatal("floor clock sampled before actual writer lock")
	}
	releasedAt := time.Now().UTC()
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal("fixture writer barrier")
	}
	rep := <-done
	if rep.Count(objects.SeverityError) != 0 || rep.Deleted != 1 || actualEntry.Before(releasedAt) {
		t.Fatal("source removal did not use actual post-lock time")
	}
	floors := localUsersState(t, st).GrantFloors
	found := false
	for _, floor := range floors {
		if floor.Grant == "operator" {
			found = true
			if !floor.ValidAfter.Equal(actualEntry) || floor.ValidAfter.Before(observed) {
				t.Fatal("floor used pre-lock source time")
			}
		}
	}
	if !found {
		t.Fatal("missing actual-entry operator floor")
	}
}

func sourceReplicaRestart(t *testing.T, st *sqlite.Store, v *validator.Validator) {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal("scheme")
	}
	path := filepath.Join(t.TempDir(), "source-replica.db")
	open := func() *sqlite.Store {
		out, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "source-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal("source follower open")
		}
		return out
	}
	follower := open()
	if err := follower.Enroll(t.Context(), st.Authority().Term); err != nil {
		t.Fatal("source follower enrollment")
	}
	installCtx := captureServingReplica(t, follower)
	image, err := st.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := follower.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal("source image validation")
	}
	installed, err := follower.InstallReplica(installCtx, valid, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal("source replica installation")
	}
	dir := t.TempDir()
	sourceBindingFile(t, dir, true, "viewer")
	if rep := sourceLoader(t, follower, newSQLiteObjectSources(follower), dir).Load(t.Context()); rep.Count(objects.SeverityError) == 0 {
		t.Fatal("follower acquired local source writer")
	}
	if err := follower.Close(); err != nil {
		t.Fatal("source follower close")
	}
	follower = open()
	defer func() { _ = follower.Close() }()
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "source-floor-restart"}); err != nil {
		t.Fatal("source replica promotion")
	}
	state := localUsersState(t, st)
	replicated := localUsersState(t, follower)
	if !reflect.DeepEqual(state.GrantFloors, replicated.GrantFloors) || !reflect.DeepEqual(state.ExternalSessions, replicated.ExternalSessions) {
		t.Fatal("restart/promotion lost original revocation/proofs")
	}
	if sourceBinding(t, st).UID != sourceBinding(t, follower).UID {
		t.Fatal("restart/promotion changed source binding identity")
	}
	// A new source reconciliation uses a newly captured actual promoted writer;
	// it cannot erase the old floor or reuse the original replica's context.
	if rep := sourceLoader(t, follower, newSQLiteObjectSources(follower), dir).Load(sourceWriter(t, follower)); rep.Count(objects.SeverityError) != 0 || rep.Updated != 1 {
		t.Fatal("promoted source command")
	}
	var originalFloor time.Time
	for _, floor := range state.GrantFloors {
		if floor.Grant == "operator" {
			originalFloor = floor.ValidAfter
		}
	}
	if originalFloor.IsZero() {
		t.Fatal("source floor missing before promotion")
	}
	retained := false
	for _, floor := range localUsersState(t, follower).GrantFloors {
		if floor.Grant == "operator" {
			retained = true
			if floor.ValidAfter.Before(originalFloor) {
				t.Fatal("promotion moved grant floor backward")
			}
		}
	}
	if !retained {
		t.Fatal("promotion erased source floor")
	}
}

func TestSQLiteObjectSourceOwnershipAndFinalizers(t *testing.T) {
	for _, owner := range []string{store.ManagerBootstrap, store.ManagerAPI} {
		t.Run(owner, func(t *testing.T) {
			s := newServingRuntimeFixture(t)
			st := s.opts.Store
			ctx := sourceWriter(t, st)
			original := roleBindingManifest("source-operator", "operator", "corp:alice")
			original.Spec.Scope.Kind = "cluster"
			// This is explicit fixture seeding before enrollment, not a production
			// fallback from the guarded loader. No existing grant is removed here.
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal("seed encode")
			}
			var binding public.RoleBinding
			if json.Unmarshal(raw, &binding) != nil {
				t.Fatal("seed decode")
			}
			if _, err := st.Apply(ctx, &binding, store.ApplyOptions{Manager: owner}); err != nil {
				t.Fatal("initial owner seed")
			}
			initial := sourceBinding(t, st)
			dir := t.TempDir()
			sourceBindingFile(t, dir, true, "viewer")
			rep := sourceLoader(t, st, newSQLiteObjectSources(st), dir).Load(ctx)
			current := sourceBinding(t, st)
			if owner == store.ManagerAPI {
				if rep.Count(objects.SeverityError) != 1 || current.UID != initial.UID || current.ResourceVersion != initial.ResourceVersion || current.Spec.Role != "operator" || len(localUsersState(t, st).GrantFloors) != 0 {
					t.Fatal("API owner was stolen by file input")
				}
				return
			}
			if rep.Count(objects.SeverityError) != 0 || rep.Updated != 1 || current.UID != initial.UID || current.Spec.Role != "viewer" || store.ManagedBy(current) != objects.Manager(filepath.Base(dir)+"/binding.yaml") {
				t.Fatal("original adoption/bootstrap semantics changed")
			}
			floors := localUsersState(t, st).GrantFloors
			if len(floors) != 1 || floors[0].Grant != "operator" {
				t.Fatal("adoption lost same-tx previous grant floor")
			}
			// Native file metadata retains finalizer semantics, rather than accidentally
			// adopting the more restricted HTTP RoleBinding desired-state DTO behavior.
			current.Finalizers = []string{"kudurru.io/source-test"}
			raw, err = json.Marshal(current)
			if err != nil {
				t.Fatal("finalizer encode")
			}
			if os.WriteFile(filepath.Join(dir, "binding.yaml"), raw, 0o600) != nil {
				t.Fatal("finalizer source write")
			}
			l := sourceLoader(t, st, newSQLiteObjectSources(st), dir)
			if rep := l.Load(ctx); rep.Count(objects.SeverityError) != 0 {
				t.Fatal("manifest finalizer apply")
			}
			sourceBindingFile(t, dir, false, "")
			if rep := l.Load(ctx); rep.Count(objects.SeverityError) != 0 || rep.Deleted != 1 {
				t.Fatal("finalizer source deletion")
			}
			deleting := sourceBinding(t, st)
			if deleting == nil || deleting.DeletionTimestamp == nil || len(deleting.Finalizers) != 1 {
				t.Fatal("source deletion bypassed ordinary finalizers")
			}
			found := false
			for _, floor := range localUsersState(t, st).GrantFloors {
				found = found || floor.Grant == "viewer"
			}
			if !found {
				t.Fatal("finalized pending deletion omitted floor")
			}
		})
	}
}

func TestSQLiteObjectSourceAcceptedCutAndFutureDiskInput(t *testing.T) {
	s := newServingRuntimeFixture(t)
	st := s.opts.Store
	ctx := sourceWriter(t, st)
	dir := t.TempDir()
	sourceBindingFile(t, dir, true, "operator")
	calls := 0
	probe := sourceEffectProbe{inner: newSQLiteObjectSources(st), before: func(_ context.Context, _ objects.RoleBindingPlan) {
		calls++
		if calls == 1 {
			sourceBindingFile(t, dir, false, "")
		}
	}}
	l := sourceLoader(t, st, probe, dir)
	// A native filesystem replacement after acceptance is future input. The
	// current effect is tied to the detached original cut, not a claim that disk
	// mutation is atomically fenced together with SQLite. No reread rescues or
	// changes the original plan inside its command.
	if rep := l.Load(ctx); rep.Created != 1 || rep.Count(objects.SeverityError) != 0 {
		t.Fatal("original accepted cut was silently replaced")
	}
	if binding := sourceBinding(t, st); binding == nil || binding.Spec.Role != "operator" || len(localUsersState(t, st).GrantFloors) != 0 {
		t.Fatal("original source effect did not commit its accepted document")
	}
	if rep := l.Load(ctx); rep.Deleted != 1 || rep.Count(objects.SeverityError) != 0 || calls != 2 {
		t.Fatal("future source reconciliation did not remove accepted grant")
	}
	floors := localUsersState(t, st).GrantFloors
	if len(floors) != 1 || floors[0].Grant != "operator" {
		t.Fatal("future source effect omitted durable floor")
	}
}
