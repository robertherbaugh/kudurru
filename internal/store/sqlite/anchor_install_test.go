package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

func anchorTestStaticPair(t *testing.T) (*Store, *Store, *replication.Image) {
	t.Helper()
	primary := openTemp(t)
	anchorTestBind(t, primary)
	anchorTestDormant(t, primary)
	follower := mirror(t, primary)
	source, err := follower.BindAnchorLifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !source.cursor.Load().awaiting {
		t.Fatal("fresh follower invented owner")
	}
	base := imageOf(t, primary)
	install(t, follower, base)
	if source.cursor.Load().awaiting || source.cursor.Load().gate == nil {
		t.Fatal("actual static install did not bind committed history")
	}
	return primary, follower, base
}

func anchorTestInstallContext(ctx context.Context, t *testing.T, s *Store) context.Context {
	t.Helper()
	original, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	captured, err := s.CaptureAnchorInstall(original)
	if err != nil {
		t.Fatal(err)
	}
	return captured
}

func TestAnchorInstallStaticLegacyAndCapturedNoop(t *testing.T) {
	_, follower, base := anchorTestStaticPair(t)
	before := follower.anchors.Load().cursor.Load()
	// Static no-op remains compatible without a fabricated finite context.
	install(t, follower, base)
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	validated, err := follower.ValidateImage(ctx, *base)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()})
	if err != nil || !generation.Same(base.Generation()) || follower.anchors.Load().cursor.Load() != before {
		t.Fatal("captured no-op changed source", err)
	}
	if _, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()}); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("no-op retained reusable original install", err)
	}
}

func TestAnchorInstallOriginalCancellationSurvivesDetachment(t *testing.T) {
	_, follower, base := anchorTestStaticPair(t)
	original, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	captured, err := follower.CaptureAnchorInstall(original)
	if err != nil {
		t.Fatal(err)
	}
	detached := context.WithoutCancel(captured)
	cancel()
	validated, err := follower.ValidateImage(detached, *base)
	if err != nil {
		t.Fatal(err)
	}
	before := imageOf(t, follower).Generation()
	_, err = follower.InstallReplica(detached, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("detachment replaced original cancellation", err)
	}
	if !before.Same(imageOf(t, follower).Generation()) {
		t.Fatal("canceled install changed source")
	}
	if _, err := follower.InstallReplica(detached, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()}); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("canceled original install was reusable", err)
	}
}

func TestAnchorInstallFiniteOriginalAndForeignStore(t *testing.T) {
	primary, follower, base := anchorTestStaticPair(t)
	if _, err := follower.CaptureAnchorInstall(t.Context()); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("unbounded capture admitted", err)
	}
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	foreign := mirror(t, primary)
	validated, err := follower.ValidateImage(ctx, *base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: foreign.Authority().Installed}); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("foreign store used original install", err)
	}
	if _, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()}); err != nil {
		t.Fatal("foreign refusal consumed owner's original", err)
	}
}

func TestAnchorInstallCannotAdoptSuccessorBaseline(t *testing.T) {
	primary, follower, base := anchorTestStaticPair(t)
	ctx := anchorTestInstallContext(t.Context(), t, follower) // BEFORE later source change.
	mustApply(t, primary, zone("later", "later.example."), store.ManagerAPI)
	later := imageOf(t, primary)
	install(t, follower, later) // Independent static source event, not a refused retry.
	validated, err := follower.ValidateImage(ctx, *later)
	if err != nil {
		t.Fatal(err)
	}
	_, err = follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: later.Term, ExpectedInstalled: later.Generation()})
	if !errors.Is(err, replication.ErrCAS) {
		t.Fatal("original operation adopted successor installed baseline", err)
	}
	if !later.Generation().Same(imageOf(t, follower).Generation()) || base.Generation().Same(later.Generation()) {
		t.Fatal("baseline fixture or refusal changed source")
	}
}

func TestAnchorInstallManagedNeedsDurableLocalDeny(t *testing.T) {
	primary := openTemp(t)
	anchorTestBind(t, primary)
	dormant := anchorTestDormant(t, primary)
	follower := mirror(t, primary)
	static := imageOf(t, primary)
	install(t, follower, static)
	if _, err := follower.BindAnchorLifecycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	anchorTestManaged(t, primary, dormant)
	managed := imageOf(t, primary)
	validated, err := follower.ValidateImage(ctx, *managed)
	if err != nil {
		t.Fatal(err)
	}
	before := imageOf(t, follower).Generation()
	_, err = follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: managed.Term, ExpectedInstalled: before})
	if !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("managed image inferred missing local deny capability", err)
	}
	if !before.Same(imageOf(t, follower).Generation()) {
		t.Fatal("missing capability installed managed proposal")
	}
	unarmed := mirror(t, primary)
	if _, err := unarmed.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: managed.Term, ExpectedInstalled: unarmed.Authority().Installed}); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("unarmed follower installed managed history", err)
	}
	// Retain original complete managed restart/tombstone acceptance as PENDING.
	// This negative case cannot substitute for that required future acceptance.
}

func TestAnchorInstallUnknownVersionRefusesWithoutOwnerValidators(t *testing.T) {
	primary := openTemp(t)
	follower := mirror(t, primary)
	image := *imageOf(t, primary)
	image.Payload.Runtime = append(slices.Clone(image.Payload.Runtime), replication.Entry{Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)})
	if err := replication.Seal(&image); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term}); !errors.Is(err, accepted.ErrInvalid) {
		t.Fatal("unknown reserved version installed without validators", err)
	}
}

func TestAnchorApplyClusterReservedPrefixNeverBypasses(t *testing.T) {
	for _, change := range []SectionChange{
		{Section: "runtime", Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)},
		{Section: "runtime", Key: "trustanchors/v99/root", Delete: true},
		{Section: "runtime", Key: accepted.RootKey, Data: json.RawMessage(accepted.AwaitingOwnerJSON)},
		{Section: "runtime", Key: accepted.RootKey, Delete: true},
	} {
		t.Run(change.Key+map[bool]string{false: "/put", true: "/delete"}[change.Delete], func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s).Generation()
			_, err := s.ApplyCluster(t.Context(), nil, store.ApplyOptions{}, []SectionChange{{Section: "runtime", Key: "foreign/witness", Data: json.RawMessage(`{}`)}, change})
			if !errors.Is(err, ErrAnchorUnavailable) {
				t.Fatal("ApplyCluster bypassed reserved owner lane", err)
			}
			if !before.Same(imageOf(t, s).Generation()) {
				t.Fatal("prefix refusal partially committed")
			}
		})
	}
}

func TestAnchorResolveConflictPreservesStaticOwnerAndMetadataEffect(t *testing.T) {
	_, follower, base := anchorTestStaticPair(t)
	cursor := follower.anchors.Load().cursor.Load()
	different := *base
	different.Payload.Runtime = append(slices.Clone(base.Payload.Runtime), replication.Entry{Key: "foreign/branch", Data: json.RawMessage(`{"selected":true}`)})
	if err := replication.Seal(&different); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), different)
	if err != nil {
		t.Fatal(err)
	}
	_, err = follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()})
	var completed *AnchorCompletionError
	if !errors.Is(err, replication.ErrDivergent) || !errors.As(err, &completed) || completed.Completion().Outcome() != AnchorCommitted {
		t.Fatal("durable divergence effect reclassified", err)
	}
	if follower.anchors.Load().cursor.Load() != cursor || !base.Generation().Same(imageOf(t, follower).Generation()) {
		t.Fatal("metadata-only divergence installed incoming owner")
	}
	if _, err := follower.ReadQuarantined(t.Context(), base.Generation()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("metadata-only divergence quarantined or replaced current owner", err)
	}
	req := replication.ResolveRequest{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation(), ExpectedDivergence: follower.Authority().Divergence, RequestID: "anchor-static-branch"}
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	resolved, err := follower.ResolveConflict(ctx, validated, req)
	if err != nil || resolved.Mode != replication.Primary || resolved.Parent == nil || !resolved.Parent.Same(different.Generation()) {
		t.Fatal("original selected branch not installed", err)
	}
	if follower.anchors.Load().cursor.Load() != cursor {
		t.Fatal("static resolution reset unchanged anchor identity")
	}
	quarantined, err := follower.ReadQuarantined(t.Context(), base.Generation())
	if err != nil || !quarantined.Generation().Same(base.Generation()) {
		t.Fatal("successful resolution lost exact old branch quarantine", err)
	}
	// Existing static dedup remains a no-op; this is a dedicated idempotence
	// assertion, not runtime replay of a refused mutation.
	again, err := follower.ResolveConflict(t.Context(), validated, req)
	if err != nil || !again.Term.Same(resolved.Term) {
		t.Fatal("static RequestID no-op changed", err)
	}
}

func TestAnchorResolveCapturedRequestNoopRequiresOriginalInstalledBaseline(t *testing.T) {
	for _, kind := range []string{"zero", "stale", "successor", "exact"} {
		t.Run(kind, func(t *testing.T) {
			_, follower, base := anchorTestStaticPair(t)
			chosen := *base
			chosen.Payload.Runtime = append(slices.Clone(base.Payload.Runtime), replication.Entry{Key: "foreign/branch", Data: json.RawMessage(`{}`)})
			if err := replication.Seal(&chosen); err != nil {
				t.Fatal(err)
			}
			validated, err := follower.ValidateImage(t.Context(), chosen)
			if err != nil {
				t.Fatal(err)
			}
			_, err = follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()})
			if !errors.Is(err, replication.ErrDivergent) {
				t.Fatal("fixture did not durably diverge", err)
			}
			req := replication.ResolveRequest{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation(), ExpectedDivergence: follower.Authority().Divergence, RequestID: "original-selected-branch"}
			resolved, err := follower.ResolveConflict(t.Context(), validated, req)
			if err != nil {
				t.Fatal(err)
			}
			before := imageOf(t, follower).Generation()
			ctx := anchorTestInstallContext(t.Context(), t, follower) // Capture this exact committed baseline.
			switch kind {
			case "zero":
				req.ExpectedInstalled = replication.Generation{}
			case "stale":
				req.ExpectedInstalled = base.Generation()
			case "successor":
				req.ExpectedInstalled = before
				req.ExpectedInstalled.Revision++
			case "exact":
				req.ExpectedInstalled = before
			}
			after, err := follower.ResolveConflict(ctx, validated, req)
			if kind == "exact" {
				if err != nil || !after.Term.Same(resolved.Term) {
					t.Fatal("exact captured no-op refused", err)
				}
			} else if !errors.Is(err, replication.ErrCAS) {
				t.Fatal("RequestID no-op adopted substituted original baseline", err)
			}
			if !before.Same(imageOf(t, follower).Generation()) {
				t.Fatal("captured no-op/refusal changed durable image")
			}
		})
	}
}

func TestAnchorResolveRollbackKeepsDivergenceAndNoQuarantine(t *testing.T) {
	_, follower, base := anchorTestStaticPair(t)
	cursor := follower.anchors.Load().cursor.Load()
	chosen := *base
	chosen.Payload.Runtime = append(slices.Clone(base.Payload.Runtime), replication.Entry{Key: "foreign/branch", Data: json.RawMessage(`{}`)})
	if err := replication.Seal(&chosen); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), chosen)
	if err != nil {
		t.Fatal(err)
	}
	_, err = follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()})
	if !errors.Is(err, replication.ErrDivergent) {
		t.Fatal("fixture did not durably diverge", err)
	}
	req := replication.ResolveRequest{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation(), ExpectedDivergence: follower.Authority().Divergence, RequestID: "controlled-refused-branch"}
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	if _, err := follower.w.ExecContext(t.Context(), `CREATE TEMP TRIGGER anchor_reject_resolution BEFORE INSERT ON replication_sections BEGIN SELECT RAISE(ABORT,'controlled owner refusal'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = follower.ResolveConflict(ctx, validated, req) // Original operation invoked ONCE.
	var completed *AnchorCompletionError
	if err == nil || errors.As(err, &completed) {
		t.Fatal("rolled-back selection received committed-effect credit")
	}
	after := follower.Authority()
	if after.Divergence == nil || !after.Divergence.Same(*req.ExpectedDivergence) || after.Mode != replication.Fenced || !after.Term.Same(base.Term) {
		t.Fatal("failed resolution erased divergence fence")
	}
	if !base.Generation().Same(imageOf(t, follower).Generation()) || follower.anchors.Load().cursor.Load() != cursor {
		t.Fatal("failed resolution changed actual source")
	}
	if _, err := follower.ReadQuarantined(t.Context(), base.Generation()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("rolled-back resolution leaked quarantine", err)
	}
}

func TestAnchorInstallRequiresAlreadyBoundedOriginalAcquisition(t *testing.T) {
	_, follower, _ := anchorTestStaticPair(t)
	original, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	before := follower.anchors.Load().cursor.Load()
	if _, err := follower.CaptureAnchorInstall(original); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long finite original falsely claimed bounded acquisition", err)
	}
	if follower.anchors.Load().cursor.Load() != before {
		t.Fatal("long-context refusal changed accepted owner")
	}
	finite, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	captured, err := follower.CaptureAnchorInstall(finite)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := captured.Deadline()
	want, _ := finite.Deadline()
	cell, _ := captured.Value(anchorInstallKey{}).(*anchorInstall)
	if !ok || !got.Equal(want) || cell == nil || cell.original != finite || captured.Done() != finite.Done() {
		t.Fatal("capture replaced original deadline or Done")
	}
}
