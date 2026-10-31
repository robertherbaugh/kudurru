package host

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Narrow the embedded interface so inventory-only providers need no Publisher.
type sourceOnlyObserver struct {
	integration.Integration
	observe func(context.Context) (SourceSnapshot, error)
}

func (p sourceOnlyObserver) Observe(ctx context.Context) (SourceSnapshot, error) {
	return p.observe(ctx)
}

func preferredSourceFixture(t *testing.T) (*Host, *job, SourceSnapshot) {
	t.Helper()
	h, j, p, _, now := engineFixture(t)
	snapshot := SourceSnapshot{Complete: true, Clients: []inventory.Client{{Source: SourceID(j.guard.Owner), ID: "device", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.10")}, MACs: []string{"00:11:22:33:44:55"}, Tags: []string{"original"}, ObservedAt: *now, ExpiresAt: now.Add(time.Minute)}}}
	h.opts.BeforeObserve = func(ctx, _ context.Context, _ ExecutionGuard) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}
	h.opts.ValidateSource = func(ExecutionGuard, SourceSnapshot) error { return nil }
	h.opts.CommitSource = func(_ context.Context, _ ExecutionGuard, s SourceSnapshot) (*Result, error) {
		return &Result{Clients: len(s.Clients), Source: &SourceOutcome{ReceiptID: "receipt", Committed: true, Published: true}}, nil
	}
	j.provider = sourceOnlyObserver{Integration: p, observe: func(context.Context) (SourceSnapshot, error) { return snapshot, nil }}
	return h, j, snapshot
}

func TestPreferredSourceConfigurationRequiresCompletePairAndBinder(t *testing.T) {
	h, _, _, _, _ := engineFixture(t)
	for _, mode := range []string{"validate_only", "commit_only", "pair_without_binder", "complete"} {
		t.Run(mode, func(t *testing.T) {
			o := h.opts
			if mode != "commit_only" {
				o.ValidateSource = func(ExecutionGuard, SourceSnapshot) error { return nil }
			}
			if mode != "validate_only" {
				o.CommitSource = func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error) { return nil, nil }
			}
			if mode == "complete" {
				o.BeforeObserve = func(ctx, _ context.Context, _ ExecutionGuard) (context.Context, func(), error) {
					return ctx, func() {}, nil
				}
			}
			_, err := New(o)
			if (err == nil) != (mode == "complete") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestPreferredSourceNeedsNoLegacyInventoryOrPublishAndCopiesInput(t *testing.T) {
	h, j, snapshot := preferredSourceFixture(t)
	if h.opts.Inventory != nil || h.opts.Publish != nil {
		t.Fatal("fixture supplied legacy dependencies")
	}
	snapshot.Desired = &DesiredBatch{SourceID: SourceID(j.guard.Owner), Zones: []*public.DNSZone{{ObjectMeta: metav1.ObjectMeta{Name: "zone", Annotations: map[string]string{"note": "original"}}}}, RecordSets: []*public.DNSRecordSet{{Spec: public.DNSRecordSetSpec{Endpoints: []public.Endpoint{{Targets: []string{"192.0.2.1"}}}}}}, ClientGroups: []*public.ClientGroup{{Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{MAC: "00:11:22:33:44:55"}}}}}}
	j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(context.Context) (SourceSnapshot, error) { return snapshot, nil }}
	validations, commits := 0, 0
	h.opts.ValidateSource = func(g ExecutionGuard, got SourceSnapshot) error {
		validations++
		if got.Desired != snapshot.Desired || &got.Clients[0] != &snapshot.Clients[0] || got.Clients[0].Source != SourceID(g.Ledger.Owner) {
			t.Fatal("validator did not precede copying or identity changed")
		}
		return nil
	}
	h.opts.CommitSource = func(_ context.Context, g ExecutionGuard, got SourceSnapshot) (*Result, error) {
		commits++
		if !got.Clients[0].ObservedAt.Equal(snapshot.Clients[0].ObservedAt) || !got.Clients[0].ExpiresAt.Equal(snapshot.Clients[0].ExpiresAt) || got.Desired.SourceID != SourceID(g.Ledger.Owner) {
			t.Fatal("host renewed timestamps/rewrote identity")
		}
		got.Clients[0].Tags[0] = "changed"
		got.Clients[0].MACs[0] = "changed"
		got.Clients[0].IPs[0] = netip.MustParseAddr("192.0.2.99")
		got.Desired.Zones[0].Annotations["note"] = "changed"
		got.Desired.RecordSets[0].Spec.Endpoints[0].Targets[0] = "192.0.2.99"
		got.Desired.ClientGroups[0].Spec.Clients[0].MAC = "changed"
		return &Result{Clients: 1, Source: &SourceOutcome{ReceiptID: "receipt", Committed: true, Published: true}}, nil
	}
	a := h.execute(context.Background(), j, command{kind: "sync"})
	if a.err != nil || a.result.Clients != 1 || validations != 1 || commits != 1 {
		t.Fatal(a, validations, commits)
	}
	if snapshot.Clients[0].Tags[0] != "original" || snapshot.Clients[0].MACs[0] != "00:11:22:33:44:55" || snapshot.Clients[0].IPs[0].String() != "192.0.2.10" || snapshot.Desired.Zones[0].Annotations["note"] != "original" || snapshot.Desired.RecordSets[0].Spec.Endpoints[0].Targets[0] != "192.0.2.1" || snapshot.Desired.ClientGroups[0].Spec.Clients[0].MAC == "changed" {
		t.Fatal("defensive copy shared provider fields")
	}
	// Returned outcome/status/publication copies must not alias each other.
	a.result.Source.ReceiptID = "caller-mutated"
	status := h.Statuses()
	if status[0].Result.Source.ReceiptID != "receipt" {
		t.Fatal("returned result mutated status")
	}
	status[0].Result.Source.ReceiptID = "status-mutated"
	if h.Statuses()[0].Result.Source.ReceiptID != "receipt" {
		t.Fatal("status outcome not detached")
	}
}

func TestPreferredSourceRejectsIdentityAndBoundsBeforeValidationCopy(t *testing.T) {
	for _, mode := range []string{"foreign_client", "missing_client_source", "foreign_desired", "missing_desired_source", "incomplete", "clients", "zones", "large_client_string", "large_metadata", "zoned_ip", "tags", "raw_metadata", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			h, j, s := preferredSourceFixture(t)
			want := ErrCapacity
			switch mode {
			case "foreign_client":
				s.Clients[0].Source = "foreign"
				want = ErrInvalid
			case "missing_client_source":
				s.Clients[0].Source = ""
				want = ErrInvalid
			case "foreign_desired":
				s.Desired = &DesiredBatch{SourceID: "foreign"}
				want = ErrInvalid
			case "missing_desired_source":
				s.Desired = &DesiredBatch{}
				want = ErrInvalid
			case "incomplete":
				s.Complete = false
				want = ErrInvalid
			case "clients":
				s.Clients = make([]inventory.Client, kinds.MaxSourceClients+1)
			case "zones":
				s.Desired = &DesiredBatch{Zones: make([]*public.DNSZone, kinds.MaxSourceZones+1)}
			case "large_client_string":
				s.Clients[0].Name = strings.Repeat("x", 254)
			case "large_metadata":
				s.Desired = &DesiredBatch{Zones: []*public.DNSZone{{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"large": strings.Repeat("x", 8193)}}}}}
			case "zoned_ip":
				s.Clients[0].IPs = []netip.Addr{netip.MustParseAddr("fe80::1").WithZone(strings.Repeat("x", 1<<20))}
			case "tags":
				s.Clients[0].Tags = make([]string, 65)
			case "raw_metadata":
				s.Desired = &DesiredBatch{Zones: []*public.DNSZone{{ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{{FieldsV1: metav1.NewFieldsV1(strings.Repeat("x", (64<<10)+1))}}}}}}
			case "aggregate":
				r := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"many": strings.Repeat("x", 8192)}}}
				s.Desired = &DesiredBatch{RecordSets: make([]*public.DNSRecordSet, kinds.MaxSourceRecordSets)}
				for i := range s.Desired.RecordSets {
					s.Desired.RecordSets[i] = r
				}
			}
			j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(context.Context) (SourceSnapshot, error) { return s, nil }}
			h.opts.ValidateSource = func(ExecutionGuard, SourceSnapshot) error {
				t.Fatal("invalid input reached validator before bounds/identity")
				return nil
			}
			h.opts.CommitSource = func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error) {
				t.Fatal("invalid input reached commit")
				return nil, nil
			}
			if a := h.execute(context.Background(), j, command{kind: "sync"}); !errors.Is(a.err, want) {
				t.Fatal(mode, a.err)
			}
		})
	}
}

func TestPreferredSourceBinderOnceAndReleaseThroughAcknowledgment(t *testing.T) {
	for _, mode := range []string{"ok", "bind_error", "observe_error", "validation_error", "commit_error", "missing_release", "detached_scope", "renewed_deadline"} {
		t.Run(mode, func(t *testing.T) {
			h, j, s := preferredSourceFixture(t)
			boundCalls, releases, polls, commits := 0, 0, 0, 0
			sentinel := errors.New("root rejected")
			h.opts.BeforeObserve = func(ctx, lease context.Context, g ExecutionGuard) (context.Context, func(), error) {
				boundCalls++
				if lease != j.leaseCtx {
					t.Fatal("binder sampled another lease")
				}
				if _, err := h.SourceCapture(ctx, g); err != nil {
					t.Fatal("capture missing before binder", err)
				}
				release := func() { releases++ }
				switch mode {
				case "bind_error":
					return nil, release, sentinel
				case "missing_release":
					return ctx, nil, nil
				case "detached_scope":
					return context.Background(), release, nil
				case "renewed_deadline":
					long, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
					return long, func() { cancel(); release() }, nil
				default:
					return ctx, release, nil
				}
			}
			j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(context.Context) (SourceSnapshot, error) {
				polls++
				if boundCalls != 1 || releases != 0 {
					t.Fatal("acquisition outside binder lifetime")
				}
				if mode == "observe_error" {
					return SourceSnapshot{}, sentinel
				}
				return s, nil
			}}
			h.opts.ValidateSource = func(ExecutionGuard, SourceSnapshot) error {
				if mode == "validation_error" {
					return sentinel
				}
				return nil
			}
			h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
				commits++
				if releases != 0 {
					t.Fatal("binder released before commit/ack")
				}
				if _, err := h.SourceCapture(ctx, g); err != nil {
					t.Fatal(err)
				}
				if mode == "commit_error" {
					return nil, sentinel
				}
				return &Result{Source: &SourceOutcome{ReceiptID: "receipt", Committed: true, Published: true}}, nil
			}
			a := h.execute(context.Background(), j, command{kind: "sync"})
			if (a.err == nil) != (mode == "ok") || boundCalls != 1 {
				t.Fatal(a.err, boundCalls)
			}
			if mode == "missing_release" {
				if releases != 0 {
					t.Fatal(releases)
				}
			} else if releases != 1 {
				t.Fatal("release not exactly once", releases)
			}
			if (mode == "ok" || mode == "commit_error") != (commits == 1) {
				t.Fatal("unexpected commit count", commits)
			}
			if (mode == "bind_error" || mode == "missing_release" || mode == "detached_scope" || mode == "renewed_deadline") && polls != 0 {
				t.Fatal("bad binder reached acquisition", polls)
			}
		})
	}
}

func TestCombinedPreferredSourcePendingAndCommittedErrorsRemainVisible(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "pending_error"}[fails], func(t *testing.T) {
			h, j, p, _, _ := engineFixture(t)
			j.provider = &fakeObserver{fakePublisher: *p, observe: func(context.Context) (SourceSnapshot, error) { return SourceSnapshot{Complete: true}, nil }}
			h.opts.BeforeObserve = func(ctx, _ context.Context, _ ExecutionGuard) (context.Context, func(), error) {
				return ctx, func() {}, nil
			}
			h.opts.ValidateSource = func(ExecutionGuard, SourceSnapshot) error { return nil }
			sentinel := errors.New("ack pending")
			h.opts.CommitSource = func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error) {
				var err error
				if fails {
					err = sentinel
				}
				return &Result{Applied: 2, Unchanged: 3, Conflicts: 1, Source: &SourceOutcome{ReceiptID: "receipt", Committed: true}}, err
			}
			a := h.execute(context.Background(), j, command{kind: "sync"})
			if a.result == nil || a.result.Applied != 3 || a.result.Pending != 1 || a.result.Unchanged != 3 || a.result.Conflicts != 1 || !a.result.Source.Committed || a.result.Source.Published || fails != errors.Is(a.err, sentinel) {
				t.Fatal(a.result, a.err)
			}
			s := h.Statuses()[0]
			if s.Phase != "Degraded" || !s.LastSync.IsZero() || s.Result.Pending != 1 {
				t.Fatal("pending source reported ready", s)
			}
		})
	}
}

func TestCombinedSourceCannotSampleSameGuardSuccessorFrame(t *testing.T) {
	h, j, p, _, _ := engineFixture(t)
	j.provider = &fakeObserver{fakePublisher: *p, observe: func(context.Context) (SourceSnapshot, error) {
		t.Fatal("successor frame reached Observe")
		return SourceSnapshot{}, nil
	}}
	h.opts.BeforeObserve = func(context.Context, context.Context, ExecutionGuard) (context.Context, func(), error) {
		t.Fatal("binder sampled successor frame")
		return nil, nil, nil
	}
	h.opts.ValidateSource = func(ExecutionGuard, SourceSnapshot) error { return nil }
	h.opts.CommitSource = func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error) {
		t.Fatal("successor frame reached source commit")
		return nil, nil
	}
	observer := j.provider.(*fakeObserver)
	observer.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		h.mu.Lock()
		copyInstance := *j.frame.instance
		j.frame.instance = &copyInstance
		h.mu.Unlock()
		return confirmedReceipt(o, h.opts.Now()), nil
	}
	a := h.execute(context.Background(), j, command{kind: "sync"})
	if !errors.Is(a.err, ErrFenced) || a.result == nil || a.result.Applied != 1 {
		t.Fatal("outbound receipt hidden/source adopted new frame", a)
	}
	if s := h.Statuses()[0]; s.Phase != "Degraded" || !s.LastSync.IsZero() {
		t.Fatal("partial command reported ready", s)
	}
}

func TestPreferredSourceMalformedOutcomeFailsClosed(t *testing.T) {
	for _, mode := range []string{"nil_result", "nil_outcome", "missing_receipt", "bad_receipt", "published_uncommitted", "dedup_uncommitted", "negative_count", "uncommitted_success"} {
		t.Run(mode, func(t *testing.T) {
			h, j, _ := preferredSourceFixture(t)
			h.opts.CommitSource = func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error) {
				r := &Result{Source: &SourceOutcome{ReceiptID: "receipt", Committed: true, Published: true}}
				switch mode {
				case "nil_result":
					return nil, nil
				case "nil_outcome":
					r.Source = nil
				case "missing_receipt":
					r.Source.ReceiptID = ""
				case "bad_receipt":
					r.Source.ReceiptID = "private text\n"
				case "published_uncommitted":
					r.Source.Committed = false
				case "dedup_uncommitted":
					r.Source.Committed = false
					r.Source.Published = false
					r.Source.Deduplicated = true
				case "negative_count":
					r.Pending = -1
				case "uncommitted_success":
					r.Source = &SourceOutcome{}
				}
				return r, nil
			}
			a := h.execute(context.Background(), j, command{kind: "sync"})
			if !errors.Is(a.err, ErrInvalid) || h.Statuses()[0].Phase != "Degraded" {
				t.Fatal("malformed outcome reported success", a)
			}
		})
	}
}

func TestPreferredSourceRejectsCredentialEchoBeforeAdmission(t *testing.T) {
	h, j, snapshot := preferredSourceFixture(t)
	j.frame.instance.secrets = map[string][]byte{"apiKey": []byte("private-observation-capability")}
	snapshot.Clients[0].Name = "prefix-private-observation-capability-suffix"
	j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(context.Context) (SourceSnapshot, error) { return snapshot, nil }}
	h.opts.ValidateSource = func(ExecutionGuard, SourceSnapshot) error { t.Fatal("credential echo reached validator"); return nil }
	h.opts.CommitSource = func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error) {
		t.Fatal("credential echo reached command")
		return nil, nil
	}
	answer := h.execute(t.Context(), j, command{kind: "sync"})
	if answer.err != ErrProvider {
		t.Fatal("credential echo did not fail safely")
	}
}
