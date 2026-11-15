package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests execute the actual Host, scoped RequestHost budget/fence,
// repository and WithLease writer. Only the remote provider is a test sink.
// Cloudflare's SDK fixture is package-private and remains frozen.
type pendingRecoverySink struct {
	*fakePublisher
	host                     providerHost
	beforeRead, beforeEffect func()
	recoveries, mutations    int
	now                      *time.Time
}

func (p *pendingRecoverySink) Records(ctx context.Context, d kinds.Destination) (RemoteSnapshot, error) {
	var out RemoteSnapshot
	err := p.host.Request(ctx, "test-provider", "account", false, func(context.Context) error {
		original := p.remote[d]
		out.Complete = original.Complete
		for _, row := range original.Records {
			row.Endpoint = *row.Endpoint.DeepCopy()
			row.IDs = slices.Clone(row.IDs)
			out.Records = append(out.Records, row)
		}
		if p.beforeRead != nil {
			p.beforeRead()
		}
		return nil
	})
	return out, err
}
func (p *pendingRecoverySink) Apply(ctx context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
	var receipt kinds.Receipt
	err := p.host.Request(ctx, "test-provider", "account", true, func(ctx context.Context) error {
		p.mutations++
		var err error
		receipt, err = p.fakePublisher.Apply(ctx, o)
		return err
	})
	return receipt, err
}
func (p *pendingRecoverySink) Recover(ctx context.Context, o *kinds.IntegrationOperation, s RemoteSnapshot) (kinds.Receipt, error) {
	p.recoveries++
	if !s.Complete {
		return kinds.Receipt{}, plan.ErrIncomplete
	}
	if p.beforeEffect != nil {
		p.beforeEffect()
	}
	var receipt kinds.Receipt
	err := p.host.Request(ctx, "test-provider", "account", true, func(ctx context.Context) error {
		saved, err := p.host.h.opts.Ledger.ReadOperation(ctx, o.Spec.OperationID)
		if err != nil || saved.Spec.Receipt == nil || saved.Spec.Receipt.Complete {
			return ErrInvalid
		}
		l, err := p.host.h.opts.Ledger.ReadLedger(ctx, o.Spec.Record)
		if err != nil || l.Spec.Pending == nil {
			return ErrInvalid
		}
		p.mutations++
		owned := plan.RemoteOwner{ClusterID: o.Spec.Record.Owner.ClusterID, IntegrationUID: o.Spec.Record.Owner.UID}
		ids := []string{}
		rows := []plan.RemoteRecord{}
		if o.Spec.After != nil {
			for i, target := range o.Spec.After.Targets {
				id := []string{"a", "b"}[i]
				e := o.Spec.After.DeepCopy()
				e.Targets = []string{target}
				rows = append(rows, plan.RemoteRecord{Endpoint: *e, IDs: []string{id}, Version: "new-" + id, Owner: owned})
				ids = append(ids, id)
			}
		}
		p.remote[o.Spec.Record.Destination] = RemoteSnapshot{Complete: true, Records: rows}
		receipt = kinds.Receipt{Complete: true, ContentHash: o.Spec.AfterHash, ProviderIDs: ids, ObservedAt: *p.now}
		if o.Spec.Receipt != nil {
			receipt.Targets = slices.Clone(o.Spec.Receipt.Targets)
		}
		return nil
	})
	return receipt, err
}

type pendingFixture struct {
	h   *Host
	j   *job
	p   *pendingRecoverySink
	st  *memory.Store
	now *time.Time
	op  *kinds.IntegrationOperation
}

func pendingRecoveryFixture(t *testing.T, adoption string) *pendingFixture {
	t.Helper()
	h, j, p, st, now := engineFixture(t)
	h.mu.Lock()
	j.frame.input.Sources = sources()
	desired := &j.frame.input.Sources[0].Records.Spec.Endpoints[0]
	desired.RecordTTL = 600
	desired.ProviderSpecific = []public.ProviderSpecific{{Name: "provider/phase", Value: "new"}}
	h.mu.Unlock()
	f, g, err := h.captured(j)
	if err != nil {
		t.Fatal(err)
	}
	own := plan.RemoteOwner{ClusterID: g.Ledger.Owner.ClusterID, IntegrationUID: g.Ledger.Owner.UID}
	beforeOwner := own
	switch adoption {
	case "unowned":
		p.pubs[0].AdoptUnowned = true
		beforeOwner = plan.RemoteOwner{}
	case "heritage":
		p.pubs[0].AdoptExternalDNSOwner = "legacy"
		beforeOwner = plan.RemoteOwner{ExternalDNSOwner: "legacy"}
	}
	d := p.pubs[0].Destination
	for i, target := range []string{"192.0.2.1", "192.0.2.2"} {
		id := []string{"a", "b"}[i]
		p.remote[d] = RemoteSnapshot{Complete: true, Records: append(p.remote[d].Records, plan.RemoteRecord{Endpoint: public.Endpoint{DNSName: "a.example.com.", RecordType: "A", RecordTTL: 300, Targets: []string{target}, ProviderSpecific: []public.ProviderSpecific{{Name: "provider/phase", Value: "old"}}}, IDs: []string{id}, Version: "old-" + id, Owner: beforeOwner})}
	}
	sink := &pendingRecoverySink{fakePublisher: p, host: providerHost{h, j}, now: now}
	h.mu.Lock()
	j.provider = sink
	h.mu.Unlock()
	fixture := &pendingFixture{h: h, j: j, p: sink, st: st, now: now}
	p.apply = func(ctx context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		fixture.op = o.DeepCopy()
		durable, err := h.opts.Ledger.ReadOperation(ctx, o.Spec.OperationID)
		if err != nil || durable.ResourceVersion == "" || durable.Spec.Receipt != nil {
			t.Fatal("effect before durable intent", durable, err)
		}
		reserved, err := h.opts.Ledger.ReadLedger(ctx, o.Spec.Record)
		if err != nil || reserved.Spec.Pending == nil {
			t.Fatal("effect before ledger reservation", reserved, err)
		}
		if adoption != "" && o.Spec.Action != "adopt" || adoption == "" && o.Spec.Action != "update" {
			t.Fatal("wrong fixture action", o.Spec.Action)
		}
		row := p.remote[d].Records[0]
		row.Endpoint = *o.Spec.After.DeepCopy()
		row.Endpoint.Targets = row.Endpoint.Targets[:1]
		row.Owner = own
		row.Version = "new-a"
		p.remote[d].Records[0] = row
		return kinds.Receipt{ObservedAt: *now, Targets: []kinds.TargetResult{{Target: o.Spec.After.Targets[0], ProviderID: "a", Applied: true}}}, nil
	}
	if result, err := h.sync(scoped(context.Background(), j, g), j, f, g, ""); !errors.Is(err, ledger.ErrPending) || result.Pending != 1 {
		t.Fatal("partial operation not reserved", result, err)
	}
	*now = now.Add(time.Second)
	return fixture
}
func (f *pendingFixture) sync(t *testing.T) (*Result, error) {
	t.Helper()
	frame, guard, err := f.h.captured(f.j)
	if err != nil {
		return nil, err
	}
	return f.h.sync(scoped(context.Background(), f.j, guard), f.j, frame, guard, "")
}
func (f *pendingFixture) assertPending(t *testing.T) {
	t.Helper()
	l, err := f.h.opts.Ledger.ReadLedger(context.Background(), f.op.Spec.Record)
	if err != nil || l.Spec.Pending == nil || l.Spec.ContentHash != "" {
		t.Fatal("invalid recovery advanced baseline", l, err)
	}
	op, err := f.h.opts.Ledger.ReadOperation(context.Background(), f.op.Spec.OperationID)
	if err != nil || op.Spec.Receipt == nil || op.Spec.Receipt.Complete || !op.Spec.AbandonedAt.IsZero() {
		t.Fatal("partial operation fabricated completion/abandonment", op, err)
	}
}

func TestGuardedPendingMixedAndExplicitAdoptionRecovery(t *testing.T) {
	for _, mode := range []string{"owned", "unowned", "heritage"} {
		t.Run(mode, func(t *testing.T) {
			adoption := mode
			if mode == "owned" {
				adoption = ""
			}
			f := pendingRecoveryFixture(t, adoption)
			// Ordinary preview/planning still rejects the pending operation; only Sync
			// enters the exact recovery path, then rebuilds original complete observations.
			frame, g, _ := f.h.captured(f.j)
			if _, err := f.h.preview(scoped(context.Background(), f.j, g), f.j, frame, g); !errors.Is(err, plan.ErrIncomplete) {
				t.Fatal("pending projection became executable preview", err)
			}
			result, err := f.sync(t)
			if err != nil || result.Pending != 0 || f.p.recoveries != 1 || f.p.mutations != 2 || f.p.calls != 1 {
				t.Fatal("mixed recovery failed", result, err, f.p.recoveries, f.p.mutations)
			}
			l, err := f.h.opts.Ledger.ReadLedger(context.Background(), f.op.Spec.Record)
			if err != nil || l.Spec.Pending != nil || l.Spec.ContentHash != f.op.Spec.AfterHash || !slices.Equal(l.Spec.ProviderIDs, []string{"a", "b"}) {
				t.Fatal("recovery receipt not confirmed", l, err)
			}
			budgets, err := f.st.List(context.Background(), kinds.KindIntegrationBudget, store.ListOptions{})
			if err != nil || len(budgets.Items) != 1 || len(budgets.Items[0].(*kinds.IntegrationBudget).Spec.Reservations) < 5 {
				t.Fatal("requests bypassed guarded shared budget", budgets, err)
			}
		})
	}
}
func TestGuardedPendingRecoveryRejectsUnknownIDsContentAndOwnership(t *testing.T) {
	cases := []struct {
		name, adoption string
		mutate         func(*pendingFixture, *plan.RemoteRecord)
	}{
		{"foreign_uid", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Owner.IntegrationUID = "another" }},
		{"foreign_cluster", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Owner.ClusterID = "another" }},
		{"unknown_old_owner", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Owner = plan.RemoteOwner{} }},
		{"new_unowned_id", "unowned", func(_ *pendingFixture, r *plan.RemoteRecord) { r.IDs = []string{"unobserved"} }},
		{"new_owned_before_content", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.IDs = []string{"unobserved"} }},
		{"wrong_heritage", "heritage", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Owner.ExternalDNSOwner = "other-owner" }},
		{"cluster_only_marker", "unowned", func(f *pendingFixture, r *plan.RemoteRecord) {
			r.Owner = plan.RemoteOwner{ClusterID: f.op.Spec.Record.Owner.ClusterID}
		}},
		{"third_ttl", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Endpoint.RecordTTL = 900 }},
		{"third_options", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Endpoint.ProviderSpecific[0].Value = "unexpected" }},
		{"unknown_target", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Endpoint.Targets = []string{"192.0.2.99"} }},
		{"adoption_after_metadata_without_marker", "unowned", func(f *pendingFixture, r *plan.RemoteRecord) {
			r.Endpoint = *f.op.Spec.After.DeepCopy()
			r.Endpoint.Targets = r.Endpoint.Targets[1:]
		}},
		{"adoption_version_changed", "unowned", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Version = "unobserved" }},
		{"adoption_version_missing", "unowned", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Version = "" }},
		{"adoption_disabled", "unowned", func(f *pendingFixture, _ *plan.RemoteRecord) { f.p.pubs[0].AdoptUnowned = false }},
		{"heritage_adoption_disabled", "heritage", func(f *pendingFixture, _ *plan.RemoteRecord) { f.p.pubs[0].AdoptExternalDNSOwner = "" }},
		{"owned_and_heritage_claim", "", func(_ *pendingFixture, r *plan.RemoteRecord) { r.Owner.ExternalDNSOwner = "legacy" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := pendingRecoveryFixture(t, tc.adoption)
			d := f.p.pubs[0].Destination
			tc.mutate(f, &f.p.remote[d].Records[1])
			_, err := f.sync(t)
			if !errors.Is(err, ledger.ErrPending) || f.p.recoveries != 0 || f.p.mutations != 1 {
				t.Fatal("unsafe observation recovered", err, f.p.recoveries)
			}
			f.assertPending(t)
		})
	}
}
func TestGuardedPendingObservationStructuralAndCNAMEProtection(t *testing.T) {
	for _, mode := range []string{"duplicate_id", "empty_id", "long_version", "unrelated_mixed", "owned_cname", "foreign_sibling", "malformed_target", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			f := pendingRecoveryFixture(t, "")
			d := f.p.pubs[0].Destination
			s := f.p.remote[d]
			switch mode {
			case "duplicate_id":
				extra := s.Records[0]
				extra.Endpoint = *extra.Endpoint.DeepCopy()
				extra.Endpoint.DNSName = "other.example.com."
				s.Records = append(s.Records, extra)
			case "empty_id":
				s.Records[1].IDs = nil
			case "long_version":
				s.Records[1].Version = strings.Repeat("v", 513)
			case "unrelated_mixed":
				a := s.Records[0]
				a.Endpoint = *a.Endpoint.DeepCopy()
				a.Endpoint.DNSName = "other.example.com."
				a.IDs = []string{"other-a"}
				b := s.Records[1]
				b.Endpoint = *b.Endpoint.DeepCopy()
				b.Endpoint.DNSName = a.Endpoint.DNSName
				b.IDs = []string{"other-b"}
				s.Records = append(s.Records, a, b)
			case "owned_cname", "foreign_sibling":
				extra := s.Records[0]
				extra.Endpoint = *extra.Endpoint.DeepCopy()
				extra.IDs = []string{"other"}
				if mode == "owned_cname" {
					extra.Endpoint.RecordType = "CNAME"
					extra.Endpoint.Targets = []string{"target.example.com."}
				} else {
					extra.Endpoint.RecordType = "TXT"
					extra.Endpoint.Targets = []string{`"foreign"`}
					extra.Owner.IntegrationUID = "other"
				}
				s.Records = append(s.Records, extra)
			case "malformed_target":
				s.Records[1].Endpoint.Targets = []string{"bad address"}
			case "incomplete":
				s.Complete = false
			}
			f.p.remote[d] = s
			_, err := f.sync(t)
			if err == nil || f.p.recoveries != 0 || f.p.mutations != 1 {
				t.Fatal("unsafe projected observation recovered", mode, err)
			}
			f.assertPending(t)
		})
	}
}

func TestGuardedPendingRecoveryConfigurationAndSourceAdmission(t *testing.T) {
	for _, mode := range []string{"config", "credential", "source_target", "source_ttl", "source_options", "source_removed", "source_expired", "policy_create_only", "uid"} {
		t.Run(mode, func(t *testing.T) {
			f := pendingRecoveryFixture(t, "unowned")
			f.h.mu.Lock()
			switch mode {
			case "config":
				f.j.guard.ConfigDigest = kinds.Digest([]byte("new config"))
			case "credential":
				f.j.guard.CredentialDigest = kinds.Digest([]byte("rotated credential"))
			case "source_target":
				f.j.frame.input.Sources[0].Records.Spec.Endpoints[0].Targets = []string{"192.0.2.99"}
			case "source_ttl":
				f.j.frame.input.Sources[0].Records.Spec.Endpoints[0].RecordTTL = 900
			case "source_options":
				f.j.frame.input.Sources[0].Records.Spec.Endpoints[0].ProviderSpecific[0].Value = "other"
			case "source_removed":
				f.j.frame.input.Sources = nil
			case "source_expired":
				f.j.frame.input.Sources[0].Records.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &metav1.Time{Time: f.now.Add(-time.Second)}}
			case "policy_create_only":
				f.p.pubs[0].Policy = plan.PolicyCreateOnly
			case "uid":
				f.j.guard.Owner.UID = "same-name-recreated"
			}
			f.h.mu.Unlock()
			_, err := f.sync(t)
			if err == nil || f.p.recoveries != 0 || f.p.mutations != 1 {
				t.Fatal("obsolete operation resumed", mode, err)
			}
			f.assertPending(t)
		})
	}
}
func TestGuardedPendingRecoveryFencesChangesAfterObservation(t *testing.T) {
	for _, point := range []string{"read", "effect"} {
		for _, mode := range []string{"config", "credential", "bundle", "term", "lease", "license", "replacement", "root_fence"} {
			t.Run(point+"_"+mode, func(t *testing.T) {
				f := pendingRecoveryFixture(t, "unowned")
				allowed := true
				rootAllowed := true
				f.h.opts.Allowed = func() bool { return allowed }
				f.h.opts.Fence = func(context.Context, ExecutionGuard) error {
					if !rootAllowed {
						return ErrFenced
					}
					return nil
				}
				change := func() {
					f.h.mu.Lock()
					defer f.h.mu.Unlock()
					switch mode {
					case "config":
						f.j.guard.ConfigDigest = kinds.Digest([]byte("next"))
					case "credential":
						f.j.guard.CredentialDigest = kinds.Digest([]byte("next"))
					case "bundle":
						f.j.frame.input.BundleID = "new source publication"
					case "term":
						f.j.guard.Term.Epoch++
					case "lease":
						f.j.cancel()
					case "license":
						allowed = false
					case "replacement":
						replacement := *f.j
						replacement.guard.Owner.UID = "replacement"
						f.h.jobs[f.j.key] = &replacement
					case "root_fence":
						rootAllowed = false
					}
				}
				if point == "read" {
					f.p.beforeRead = change
				} else {
					f.p.beforeEffect = change
				}
				_, err := f.sync(t)
				expected := ErrFenced
				if point == "effect" {
					expected = ErrProvider
				} // Provider errors are sanitized; no request was loaned.
				if !errors.Is(err, expected) || f.p.mutations != 1 {
					t.Fatal("obsolete capture loaned outbound effect", mode, err, f.p.mutations)
				}
				if point == "read" && f.p.recoveries != 0 {
					t.Fatal("stale observation reached recovery")
				}
				f.assertPending(t)
			})
		}
	}
}
func TestGuardedPendingRecoveryDurabilityFailureAndNewLease(t *testing.T) {
	for _, failedKind := range []store.Kind{kinds.KindIntegrationOperation, kinds.KindIntegrationLedger} {
		t.Run(string(failedKind), func(t *testing.T) {
			f := pendingRecoveryFixture(t, "heritage")
			reject := true
			repo, err := ledger.New(ledger.Options{Reader: f.st, Now: func() time.Time { return *f.now }, Write: func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
				var result store.Object
				err := f.h.WithLease(ctx, g, func(ctx context.Context) error {
					if reject && store.KindOf(o) == failedKind {
						return store.ErrClosed
					}
					var err error
					result, err = f.st.Apply(ctx, o, a)
					return err
				})
				return result, err
			}})
			if err != nil {
				t.Fatal(err)
			}
			f.h.opts.Ledger = repo
			result, err := f.sync(t)
			if !errors.Is(err, store.ErrClosed) || result.Pending != 1 || f.p.mutations != 2 || f.p.recoveries != 1 {
				t.Fatal("durability failure ignored", result, err)
			}
			saved, err := repo.ReadOperation(context.Background(), f.op.Spec.OperationID)
			if err != nil || saved.Spec.Receipt.Complete != (failedKind == kinds.KindIntegrationLedger) {
				t.Fatal("receipt ordering violated", saved, err)
			}
			l, err := repo.ReadLedger(context.Background(), f.op.Spec.Record)
			if err != nil || l.Spec.Pending == nil || l.Spec.ContentHash != "" {
				t.Fatal("failed commit published baseline", l, err)
			}
			reject = false
			*f.now = f.now.Add(time.Second)
			f.h.mu.Lock()
			f.j.guard.LeaseID = "restarted-lease"
			f.h.mu.Unlock()
			result, err = f.sync(t)
			if err != nil || result.Pending != 0 || f.p.mutations != 2 || f.p.recoveries != 1 {
				t.Fatal("restart replayed effects", result, err, f.p.mutations)
			}
			l, err = repo.ReadLedger(context.Background(), f.op.Spec.Record)
			if err != nil || l.Spec.Pending != nil || l.Spec.ContentHash != f.op.Spec.AfterHash {
				t.Fatal("restart did not confirm observed IDs", l, err)
			}
		})
	}
}
func TestGuardedPendingProjectionNeverChangesOrdinaryPlanning(t *testing.T) {
	f := pendingRecoveryFixture(t, "")
	frame, g, _ := f.h.captured(f.j)
	inputs, err := f.h.publications(scoped(context.Background(), f.j, g), f.j, frame, g)
	if err != nil {
		t.Fatal(err)
	}
	in := inputs[0].planInput
	for i, l := range in.Ledger {
		in.Ledger[i] = l.DeepCopy()
		in.Ledger[i].Spec.Pending = nil
	}
	if _, err := observationPlan(in); !errors.Is(err, plan.ErrInvalid) {
		t.Fatal("ordinary observation accepted mixed attributes", err)
	}
	if _, err := validatePendingObservation(in); !errors.Is(err, plan.ErrInvalid) {
		t.Fatal("projection relaxed a nonpending RRset", err)
	}
	if _, err := plan.Build(in); !errors.Is(err, plan.ErrInvalid) {
		t.Fatal("ordinary planner changed", err)
	}
	f.assertPending(t)
}

func TestGuardedMixedDeleteCannotFabricateAbsenceHash(t *testing.T) {
	h, j, p, _, now := engineFixture(t)
	h.mu.Lock()
	j.frame.input.Sources = nil
	h.mu.Unlock()
	frame, g, _ := h.captured(j)
	d := p.pubs[0].Destination
	own := plan.RemoteOwner{ClusterID: g.Ledger.Owner.ClusterID, IntegrationUID: g.Ledger.Owner.UID}
	for i, target := range []string{"192.0.2.1", "192.0.2.2"} {
		id := []string{"a", "b"}[i]
		p.remote[d] = RemoteSnapshot{Complete: true, Records: append(p.remote[d].Records, plan.RemoteRecord{Endpoint: public.Endpoint{DNSName: "a.example.com.", RecordType: "A", RecordTTL: 300, Targets: []string{target}}, IDs: []string{id}, Owner: own, Version: "before-" + id})}
	}
	sink := &pendingRecoverySink{fakePublisher: p, host: providerHost{h, j}, now: now}
	j.provider = sink
	var op *kinds.IntegrationOperation
	p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		op = o.DeepCopy()
		p.remote[d].Records[0].Endpoint.RecordTTL = 600
		return kinds.Receipt{ObservedAt: *now, Targets: []kinds.TargetResult{{Target: o.Spec.Before.Targets[0], ErrorClass: "ambiguous"}}}, nil
	}
	if result, err := h.sync(scoped(context.Background(), j, g), j, frame, g, ""); !errors.Is(err, ledger.ErrPending) || result.Pending != 1 {
		t.Fatal(result, err)
	}
	*now = now.Add(time.Second)
	if result, err := h.sync(scoped(context.Background(), j, g), j, frame, g, ""); !errors.Is(err, ledger.ErrPending) || result.Pending != 1 || sink.recoveries != 0 {
		t.Fatal("mixed rows confirmed absence", result, err)
	}
	l, err := h.opts.Ledger.ReadLedger(context.Background(), op.Spec.Record)
	if err != nil || l.Spec.Pending == nil || !l.Spec.LastApplied.IsZero() {
		t.Fatal("mixed delete published completion", l, err)
	}
}
func TestGuardedCurrentMarkerRequiredForNewRecoveryIDs(t *testing.T) {
	f := pendingRecoveryFixture(t, "unowned")
	d := f.p.pubs[0].Destination
	row := &f.p.remote[d].Records[0]
	row.IDs = []string{"created-after-intent"}
	row.Version = "new-version"
	// This ID was never in Before, but its exact After content and current UID
	// marker provide recovery evidence. Remaining unowned b still needs old proof.
	result, err := f.sync(t)
	if err != nil || result.Pending != 0 || f.p.recoveries != 1 {
		t.Fatal("current marked new ID was rejected", result, err)
	}
}

func TestGuardedPendingSiblingCannotHideForeignOwnerFromCompletion(t *testing.T) {
	f := pendingRecoveryFixture(t, "")
	d := f.p.pubs[0].Destination
	_, g, _ := f.h.captured(f.j)
	owner := plan.RemoteOwner{ClusterID: g.Ledger.Owner.ClusterID, IntegrationUID: g.Ledger.Owner.UID}
	sibling := plan.RemoteRecord{Endpoint: public.Endpoint{DNSName: f.op.Spec.Record.Key.Name, RecordType: "TXT", RecordTTL: 300, Targets: []string{`"sibling"`}}, IDs: []string{"sibling"}, Version: "before-sibling", Owner: owner}
	built, err := plan.Build(plan.Input{Owner: g.Ledger.Owner, Destination: d, ConfigDigest: g.Ledger.ConfigDigest, CredentialDigest: g.Ledger.CredentialDigest, SourceDigest: kinds.Digest([]byte("sibling source")), Current: []plan.RemoteRecord{sibling}, DesiredComplete: true, CurrentComplete: true})
	if err != nil || len(built.Operations) != 1 {
		t.Fatal(built, err)
	}
	if _, err = f.h.opts.Ledger.Begin(scoped(context.Background(), f.j, g), g.Ledger, built.Intent(0, *f.now), ""); err != nil {
		t.Fatal(err)
	}
	// The A set is fully After, but the neighboring pending TXT has changed to
	// another owner. Both keys disappear only from the validation projection.
	row := f.p.remote[d].Records[1]
	row.Endpoint = *f.op.Spec.After.DeepCopy()
	row.Endpoint.Targets = row.Endpoint.Targets[1:]
	row.Owner = owner
	f.p.remote[d].Records[1] = row
	sibling.Owner.IntegrationUID = "foreign"
	s := f.p.remote[d]
	s.Records = append(s.Records, sibling)
	f.p.remote[d] = s
	if result, err := f.sync(t); !errors.Is(err, ledger.ErrPending) || result.Pending != 1 || f.p.recoveries != 0 {
		t.Fatal("pending sibling hid foreign ownership", result, err)
	}
	f.assertPending(t)
}

func TestGuardedPendingRecoveryKeepsExactDurableCASTokens(t *testing.T) {
	for _, kind := range []store.Kind{kinds.KindIntegrationOperation, kinds.KindIntegrationLedger} {
		t.Run(string(kind), func(t *testing.T) {
			f := pendingRecoveryFixture(t, "unowned")
			_, g, _ := f.h.captured(f.j)
			f.p.beforeEffect = func() {
				ctx := scoped(context.Background(), f.j, g)
				if kind == kinds.KindIntegrationOperation {
					old, err := f.h.opts.Ledger.ReadOperation(ctx, f.op.Spec.OperationID)
					if err != nil {
						t.Fatal(err)
					}
					receipt := *old.Spec.Receipt
					receipt.ObservedAt = *f.now
					if _, err = f.h.opts.Ledger.RecordReceipt(ctx, g.Ledger, old, receipt); err != nil {
						t.Fatal(err)
					}
				} else {
					old, err := f.h.opts.Ledger.ReadLedger(ctx, f.op.Spec.Record)
					if err != nil {
						t.Fatal(err)
					}
					old.Spec.Sequence++
					if err = f.h.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
						_, err := f.st.Apply(ctx, old, store.ApplyOptions{Manager: "integration-ledger", ExpectedResourceVersion: old.ResourceVersion})
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := f.sync(t)
			if !errors.Is(err, store.ErrConflict) || result.Pending != 1 || f.p.mutations != 2 {
				t.Fatal("recovery silently substituted current CAS tokens", kind, result, err)
			}
			l, err := f.h.opts.Ledger.ReadLedger(context.Background(), f.op.Spec.Record)
			if err != nil || l.Spec.Pending == nil || l.Spec.ContentHash != "" {
				t.Fatal("conflicted confirmation advanced baseline", l, err)
			}
			f.p.beforeEffect = nil
			*f.now = f.now.Add(time.Second)
			result, err = f.sync(t)
			if err != nil || result.Pending != 0 || f.p.mutations != 2 || f.p.recoveries != 1 {
				t.Fatal("CAS recovery replayed effects", result, err)
			}
		})
	}
}
