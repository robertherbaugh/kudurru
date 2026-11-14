package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// integrationFinalizer adds or removes one finalizer of the stored object by
// its exact identity, as store.FinalizerWriter does for any caller.
func integrationFinalizer(t *testing.T, s *servingRuntime, d *public.Integration, name string, remove bool) *public.Integration {
	t.Helper()
	out, err := s.opts.Store.UpdateFinalizer(t.Context(), store.KeyOf(d), store.FinalizerOptions{Finalizer: name, Remove: remove, ExpectedUID: string(d.UID), ExpectedResourceVersion: d.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*public.Integration)
}

// TestIntegrationRootRefusesAModeTheStoredObjectDoesNotAllow proves the
// root's own commit-boundary rule (D367) at each place it stands, against
// objects stored in a real SQLite database: the captured cut (current), the
// stored object before an effect (fence, through storedCurrent), and the
// object read inside the transaction of an effect
// (integrationInputsCurrent). Every guard here names the stored object's
// exact UID, resource version and configuration, so the mode is the only
// thing that can refuse it: a guard is admitted exactly when the object is
// what its mode claims, and a refused one commits nothing.
//
// The host derives a job's mode from the same object and never asks for
// anything else, so no job can reach these refusals; they are root's
// independent check, and only a direct call shows each one.
func TestIntegrationRootRefusesAModeTheStoredObjectDoesNotAllow(t *testing.T) {
	s, path := integrationFixture(t, nil, nil)
	r := s.integrations
	// No job may run: the host would end every terminating object below.
	options, err := r.hostOptions()
	if err != nil {
		t.Fatal(err)
	}
	options.Capture = func(context.Context) (ih.Input, error) { return ih.Input{}, ih.ErrUnavailable }
	if r.host, err = ih.New(options); err != nil {
		t.Fatal(err)
	}
	const hold = "example.com/hold"
	declare := func(name, policy string, suspend bool) *public.Integration {
		d := integrationDefinition(name, path)
		d.Spec.DeletionPolicy, d.Spec.Suspend = policy, suspend
		return integrationApply(t, s, d)
	}
	deleting := func(d *public.Integration) {
		integrationRequestDelete(t, s, d)
	}
	// Each object is brought into its state through the store's own writes.
	// deleting, armed, policy and suspend say what must then be stored; admits
	// is the one mode that state allows, if any.
	states := []struct {
		name            string
		deleting, armed bool
		policy          string
		suspend         bool
		admits          ih.GuardMode
		none            bool
		build           func(name string)
		otherFinalizers int
	}{
		{name: "retained", policy: public.IntegrationDeletionRetain, admits: ih.GuardNormal, build: func(name string) { declare(name, public.IntegrationDeletionRetain, false) }},
		{name: "armed", armed: true, policy: public.IntegrationDeletionDelete, admits: ih.GuardNormal, build: func(name string) { declare(name, public.IntegrationDeletionDelete, false) }},
		{name: "unarmed", policy: public.IntegrationDeletionDelete, none: true, build: func(name string) {
			integrationFinalizer(t, s, declare(name, public.IntegrationDeletionDelete, false), store.IntegrationCleanupFinalizer, true)
		}},
		{name: "suspended", policy: public.IntegrationDeletionRetain, suspend: true, none: true, build: func(name string) { declare(name, public.IntegrationDeletionRetain, true) }},
		{name: "cleanup", deleting: true, armed: true, policy: public.IntegrationDeletionDelete, admits: ih.GuardCleanup, build: func(name string) { deleting(declare(name, public.IntegrationDeletionDelete, false)) }},
		{name: "frozen", deleting: true, armed: true, policy: public.IntegrationDeletionDelete, suspend: true, none: true, build: func(name string) { deleting(declare(name, public.IntegrationDeletionDelete, true)) }},
		{name: "release", deleting: true, armed: true, policy: public.IntegrationDeletionRetain, admits: ih.GuardRelease, build: func(name string) {
			deleting(integrationFinalizer(t, s, declare(name, public.IntegrationDeletionRetain, false), store.IntegrationCleanupFinalizer, false))
		}},
		{name: "held", deleting: true, policy: public.IntegrationDeletionDelete, none: true, otherFinalizers: 1, build: func(name string) {
			d := integrationFinalizer(t, s, declare(name, public.IntegrationDeletionDelete, false), hold, false)
			deleting(integrationFinalizer(t, s, d, store.IntegrationCleanupFinalizer, true))
		}},
	}
	for _, state := range states {
		state.build(state.name)
	}
	integrationRun(t, s)

	// The writer of one elected term, as every job's lease carries it.
	terms := make(chan context.Context, 1)
	electorCtx, stopElector := context.WithCancel(t.Context())
	electorDone := make(chan error, 1)
	go func() {
		electorDone <- s.opts.Store.Elector("integration-guard-test").Run(electorCtx, func(term context.Context) {
			select {
			case terms <- term:
			default:
			}
			<-term.Done()
		}, nil)
	}()
	t.Cleanup(func() {
		stopElector()
		if err := <-electorDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	var term context.Context
	select {
	case term = <-terms:
	case <-time.After(15 * time.Second):
		t.Fatal("no elected term")
	}
	token, ok := sqlite.WriterTokenFromContext(term)
	if !ok {
		t.Fatal("the elected term carries no writer token")
	}

	// One captured cut that holds every object exactly as it is stored.
	stored := map[string]*public.Integration{}
	var cut *integrationCut
	awaitServingRuntime(t, s, func() bool {
		if _, err := r.capture(t.Context()); err != nil {
			return false
		}
		r.mu.RLock()
		cut = r.cut
		r.mu.RUnlock()
		for _, state := range states {
			d := integrationStored(t, s, integrationDefinition(state.name, path))
			captured, present := cut.input.Objects.Get(store.Key{Kind: "Integration", Namespace: "default", Name: state.name})
			if d == nil || !present || captured.GetResourceVersion() != d.ResourceVersion {
				return false
			}
			stored[state.name] = d
		}
		return true
	})
	// The fixture is what the cases say it is.
	for _, state := range states {
		d := stored[state.name]
		armed := store.HasFinalizer(d, store.IntegrationCleanupFinalizer)
		others := len(d.Finalizers)
		if armed {
			others--
		}
		if (d.DeletionTimestamp != nil) != state.deleting || armed != state.armed || d.Spec.DeletionPolicy != state.policy || d.Spec.Suspend != state.suspend || others != state.otherFinalizers {
			t.Fatalf("%s is stored as deleting=%v finalizers=%v policy=%q suspend=%v", state.name, d.DeletionTimestamp != nil, d.Finalizers, d.Spec.DeletionPolicy, d.Spec.Suspend)
		}
	}

	marker := func(owner kinds.Owner, name string) *kinds.IntegrationLedger {
		record := kinds.RecordIdentity{Owner: owner, Destination: kinds.Destination{Provider: "cloudflare", Account: "account", Zone: "zone"}, Key: kinds.RecordKey{Name: name, Type: "A"}}
		return &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(record)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: record, Sequence: 1}}
	}
	written := func(row *kinds.IntegrationLedger) bool {
		err := s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationLedger, Name: row.Name}, &kinds.IntegrationLedger{})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	admitted := 0
	for _, state := range states {
		d := stored[state.name]
		owner := integrationOwner(s, d)
		instance := cut.instances[owner]
		if instance == nil {
			t.Fatalf("%s: not captured", state.name)
		}
		ledgerGuard, err := instance.TerminatingGuard("guard-test")
		if err != nil {
			t.Fatal(err)
		}
		captured, _ := cut.input.Objects.Get(store.KeyOf(d))
		for _, mode := range []ih.GuardMode{ih.GuardNormal, ih.GuardCleanup, ih.GuardRelease} {
			g := ih.ExecutionGuard{Ledger: ledgerGuard, BundleID: instance.BundleID(), Mode: mode}
			want := !state.none && mode == state.admits
			claim := map[ih.GuardMode]string{ih.GuardNormal: "normal", ih.GuardCleanup: "cleanup", ih.GuardRelease: "release"}[mode]
			label := state.name + " under mode " + claim
			if want {
				admitted++
			}
			verdict := func(site string, err error) {
				t.Helper()
				if want && err != nil {
					t.Errorf("%s: %s refused the object's own mode: %v", label, site, err)
				}
				if !want && !errors.Is(err, ih.ErrFenced) {
					t.Errorf("%s: %s did not fence: %v", label, site, err)
				}
			}
			// The captured cut.
			r.mu.RLock()
			_, err := r.current(g)
			r.mu.RUnlock()
			verdict("current", err)
			// The stored object, before an effect.
			verdict("fence", r.storedCurrent(t.Context(), captured, g))
			// The object read in the transaction of an effect: a write staged
			// behind the check exists exactly when the check admitted it.
			inside := marker(owner, "transaction."+claim+".example.")
			err = s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := integrationInputsCurrent(ctx, tx, cut, g); err != nil {
					return err
				}
				_, err := tx.Apply(ctx, inside, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true})
				return err
			})
			verdict("the transaction", err)
			if written(inside) != want {
				t.Errorf("%s: the write behind the transaction check exists=%v", label, !want)
			}
			// The command every effect of a job goes through (ledger intent,
			// budget charge, receipt, source commit, status, forget, purge,
			// finalization), with the term's writer and the original cut.
			effect := marker(owner, "command."+claim+".example.")
			commandCtx := context.WithValue(sqlite.WithWriterToken(t.Context(), token), integrationRequestKey{}, cut)
			err = r.command(commandCtx, g, func(ctx context.Context, tx *sqlite.CommandTx, _ *integrationCut) error {
				_, err := tx.Apply(ctx, effect, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true})
				return err
			})
			verdict("the command", err)
			if written(effect) != want {
				t.Errorf("%s: the effect of the command exists=%v", label, !want)
			}
		}
	}
	// Four objects admit one mode each; nothing else was written.
	if rows := integrationRows(t, s, kinds.KindIntegrationLedger); admitted != 4 || len(rows) != 2*admitted {
		t.Fatalf("%d guards admitted, %d writes committed", admitted, len(rows))
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationOperation, kinds.KindIntegrationBudget, kinds.KindIntegrationSourceHead} {
		if rows := integrationRows(t, s, kind); len(rows) != 0 {
			t.Errorf("%d %s rows exist", len(rows), kind)
		}
	}
}

// integrationCountedProbe counts what reached the provider.
type integrationCountedProbe struct {
	*integrationProbe
	provisions *atomic.Int32
}

func (p integrationCountedProbe) Provision(ctx context.Context, h integration.Host, spec json.RawMessage, secrets integration.SecretResolver) error {
	p.provisions.Add(1)
	return p.integrationProbe.Provision(ctx, h, spec, secrets)
}

// integrationMisreport returns the capture with one Integration changed, and
// everything else, including root's own cut of the stored objects, as it is.
func integrationMisreport(in ih.Input, name string, lie func(*public.Integration)) (ih.Input, error) {
	var objects []store.Object
	for _, kind := range in.Objects.Kinds() {
		for _, obj := range in.Objects.List(kind) {
			copied := obj.DeepCopyObject().(store.Object)
			if d, ok := copied.(*public.Integration); ok && d.Name == name {
				lie(d)
			}
			objects = append(objects, copied)
		}
	}
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: in.Objects.Kinds(), Now: in.Objects.Now()}, objects, nil)
	if err != nil {
		return ih.Input{}, err
	}
	in.Objects = snapshot.Objects
	return in, nil
}

// TestIntegrationRuntimeRootRefusesAJobTheStoredObjectDoesNotAllow removes
// the host's own check and shows what is left: root. The host decides a job's
// mode from the object it was shown, and pauses an unarmed Delete instance
// before root is ever asked, so the mounted lifecycle tests cannot tell
// whether root would refuse. Here the host is shown one Integration in a
// state it is not stored in, with the same UID, resource version and spec,
// and runs the job that state calls for. Root reads the object itself and
// gives that job nothing: no provider call, no source, no budget charge, no
// purge and no finalization.
func TestIntegrationRuntimeRootRefusesAJobTheStoredObjectDoesNotAllow(t *testing.T) {
	type world struct {
		s          *servingRuntime
		d          *public.Integration
		owner      kinds.Owner
		lie        atomic.Bool
		polls      atomic.Int32
		provisions atomic.Int32
	}
	// start mounts the runtime with a host whose capture misreports the
	// Integration while w.lie is set.
	start := func(t *testing.T, prepare func(*testing.T, *world, string), lie func(*public.Integration)) *world {
		t.Helper()
		w := &world{}
		var path string
		w.s, path = integrationFixture(t, nil, func(string) (integration.Integration, error) {
			return integrationCountedProbe{&integrationProbe{observe: func(_ context.Context, h ih.OwnerHost) (ih.SourceSnapshot, error) {
				w.polls.Add(1)
				return ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{integrationClient(h.IntegrationOwner())}}, nil
			}}, &w.provisions}, nil
		})
		t.Cleanup(func() { integrationTerminatingLog(t, w.s, nil) })
		prepare(t, w, path)
		w.owner = integrationOwner(w.s, w.d)
		options, err := w.s.integrations.hostOptions()
		if err != nil {
			t.Fatal(err)
		}
		capture := options.Capture
		options.Capture = func(ctx context.Context) (ih.Input, error) {
			in, err := capture(ctx)
			if err != nil || !w.lie.Load() {
				return in, err
			}
			return integrationMisreport(in, w.d.Name, lie)
		}
		if w.s.integrations.host, err = ih.New(options); err != nil {
			t.Fatal(err)
		}
		integrationRun(t, w.s)
		return w
	}
	reported := func(w *world) (string, string) {
		for _, status := range w.s.integrations.host.Statuses() {
			if status.Owner == w.owner {
				return status.Phase, status.Reason
			}
		}
		return "", ""
	}
	actions := func(t *testing.T, w *world) []string {
		t.Helper()
		var out []string
		for _, entry := range integrationAudit(t, w.s, w.d) {
			out = append(out, entry.Action)
		}
		return out
	}

	t.Run("an ordinary job for an unarmed Delete object", func(t *testing.T) {
		w := start(t, func(t *testing.T, w *world, path string) {
			definition := integrationDefinition("claimed", path)
			definition.Spec.DeletionPolicy = public.IntegrationDeletionDelete
			w.d = integrationFinalizer(t, w.s, integrationApply(t, w.s, definition), store.IntegrationCleanupFinalizer, true)
			// The host is told the object carries its cleanup finalizer.
			w.lie.Store(true)
		}, func(d *public.Integration) { d.Finalizers = append(d.Finalizers, store.IntegrationCleanupFinalizer) })
		// The host runs the instance instead of pausing it, and root fences
		// the job's first step, before the provider is provisioned.
		awaitServingRuntime(t, w.s, func() bool {
			phase, reason := reported(w)
			return phase == "Degraded" && reason == "ProvisionFailed"
		})
		time.Sleep(300 * time.Millisecond)
		if w.provisions.Load() != 0 || w.polls.Load() != 0 {
			t.Fatalf("an unarmed Delete instance reached its provider: provisions=%d polls=%d", w.provisions.Load(), w.polls.Load())
		}
		integrationNoSource(t, w.s, w.owner)
		for _, kind := range []store.Kind{kinds.KindIntegrationBudget, kinds.KindIntegrationLedger, kinds.KindIntegrationOperation, kinds.KindIntegrationSourceChunk, kinds.KindIntegrationNameAllocation} {
			if rows := integrationRows(t, w.s, kind); len(rows) != 0 {
				t.Errorf("an unarmed Delete instance owns %d %s rows", len(rows), kind)
			}
		}
		if current := integrationStored(t, w.s, w.d); current == nil || current.ResourceVersion != w.d.ResourceVersion {
			t.Fatal("root wrote to an object it refused")
		}
	})

	t.Run("a cleanup job for a live object", func(t *testing.T) {
		w := start(t, func(t *testing.T, w *world, path string) {
			definition := integrationDefinition("claimed", path)
			definition.Spec.DeletionPolicy = public.IntegrationDeletionDelete
			w.d = integrationApply(t, w.s, definition)
		}, func(d *public.Integration) {
			at := metav1.NewTime(time.Unix(1700000000, 0))
			d.DeletionTimestamp = &at
		})
		// Shown as it is, the armed instance runs through the same capture
		// path and commits its source: the misreport is all that differs.
		address := netip.MustParseAddr("192.0.2.19")
		awaitServingRuntime(t, w.s, func() bool {
			snapshot := integrationInventory(t, w.s)
			return snapshot != nil && snapshot.Lookup(address, time.Now()) != nil
		})
		integrationReadyCommand(t, w.s, w.owner)
		var head kinds.IntegrationSourceHead
		headKey := store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(w.owner)}
		if err := w.s.opts.Store.Get(t.Context(), headKey, &head); err != nil || w.polls.Load() != 1 {
			t.Fatal("the armed instance did not commit its source", err != nil, w.polls.Load())
		}
		// Now the host is told the object is being deleted. It joins the
		// ordinary job and elects a cleanup job, whose every run root fences.
		w.lie.Store(true)
		w.s.integrations.host.Wake()
		awaitServingRuntime(t, w.s, func() bool {
			phase, reason := reported(w)
			return phase == ih.PhaseTerminating && reason == ih.ReasonAuthorityChanged
		})
		time.Sleep(500 * time.Millisecond)
		after := integrationStored(t, w.s, w.d)
		if after == nil || after.DeletionTimestamp != nil || !store.HasFinalizer(after, store.IntegrationCleanupFinalizer) || after.UID != w.d.UID {
			t.Fatal("a cleanup job finalized or changed a live object")
		}
		if err := w.s.opts.Store.Get(t.Context(), headKey, &kinds.IntegrationSourceHead{}); err != nil {
			t.Fatal("a cleanup job purged the source of a live object")
		}
		if snapshot := integrationInventory(t, w.s); snapshot != nil && snapshot.Lookup(address, time.Now()) == nil {
			t.Fatal("a cleanup job withdrew the inventory of a live object")
		}
		for _, action := range actions(t, w) {
			if slices.Contains([]string{"integration.purge", "integration.cleanup", "integration.release", "integration.finalize"}, action) {
				t.Fatalf("a refused cleanup job left the audit row %s", action)
			}
		}
		if phase, reason := reported(w); phase != ih.PhaseTerminating || reason != ih.ReasonAuthorityChanged {
			t.Fatalf("status of the refused cleanup job: %s/%s", phase, reason)
		}
	})
}
