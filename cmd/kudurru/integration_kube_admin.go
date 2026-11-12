package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The Kubernetes integration administration adapter (F09b-K6, D384).
//
// Authorization is the route permission the API server already enforces on
// every request (integrations.read, check, sync, act, release). This store has
// no original-credential proof re-authorized inside each effect, as SQLite's
// current commands do: a permission withdrawn while a command runs does not
// stop that command. Status is read from the Integration object, written by
// the Lease holder, so every replica answers the same; it is known only when
// its Ready condition observed the object's current generation. An instance
// that no job runs for (paused, suspended) writes none; its local status,
// which every replica derives from the same object revision, is shown.
// Commands run only on the replica that holds the instance's Lease; any other
// replica answers 503 and names the holder (or, when no other replica holds
// it, the instance's phase and reason), and never forwards or replays the
// request. A reviewed sync or act is recorded as a structured log line on the
// holder, because this store has no kudurru audit table.

// kubeIntegrationHolderHint is the hint of a command this replica cannot run:
// Holder names another replica that holds the instance's Lease; otherwise
// Phase and Reason say why no job runs it here (for example Paused and
// Suspended, or LicenseUnavailable).
type kubeIntegrationHolderHint struct {
	Holder string `json:"holder,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (r *kubeIntegrationRuntime) adminSnapshot(namespace string) (zonecache.ObjectSet, error) {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return zonecache.ObjectSet{}, integrationAdminError(ih.ErrInvalid)
	}
	snap := r.opts.Cache.Snapshot()
	if snap == nil || snap.State != zonecache.Live {
		return zonecache.ObjectSet{}, integrationAdminError(ih.ErrUnavailable)
	}
	return snap.Objects, nil
}

// IntegrationList implements api.IntegrationAdministration.
func (r *kubeIntegrationRuntime) IntegrationList(_ context.Context, namespace string) ([]api.IntegrationSummary, error) {
	objects, err := r.adminSnapshot(namespace)
	if err != nil {
		return nil, err
	}
	definitions := objects.List("Integration")
	if len(definitions) > ih.MaxInstances {
		return nil, integrationAdminError(ih.ErrCapacity)
	}
	statuses := r.host.Statuses()
	out := make([]api.IntegrationSummary, 0, len(definitions))
	for _, obj := range definitions {
		if d, ok := obj.(*public.Integration); ok && d.Namespace == namespace {
			out = append(out, r.kubeIntegrationSummary(d, statuses))
		}
	}
	slices.SortFunc(out, func(a, b api.IntegrationSummary) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// IntegrationGet implements api.IntegrationAdministration.
func (r *kubeIntegrationRuntime) IntegrationGet(_ context.Context, namespace, name string) (*api.IntegrationSummary, error) {
	objects, err := r.adminSnapshot(namespace)
	if err != nil {
		return nil, err
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	obj, ok := objects.Get(store.Key{Kind: "Integration", Namespace: namespace, Name: name})
	d, typed := obj.(*public.Integration)
	if !ok || !typed {
		return nil, integrationAdminError(store.ErrNotFound)
	}
	out := r.kubeIntegrationSummary(d, r.host.Statuses())
	return &out, nil
}

// kubeIntegrationSummary reads the phase from the durable Ready condition
// and Leader from this replica's host.
func (r *kubeIntegrationRuntime) kubeIntegrationSummary(d *public.Integration, statuses []ih.InstanceStatus) api.IntegrationSummary {
	out := api.IntegrationSummary{Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), Revision: d.ResourceVersion, Type: d.Spec.Type, Suspended: d.Spec.Suspend, DeletionPolicy: d.Spec.DeletionPolicy, Phase: "Unknown", Reason: "NoMatchingObservation"}
	if out.DeletionPolicy == "" {
		out.DeletionPolicy = public.IntegrationDeletionRetain
	}
	if d.DeletionTimestamp != nil {
		at := d.DeletionTimestamp.UTC()
		out.Terminating, out.DeletionRequestedAt = true, &at
	}
	if c := meta.FindStatusCondition(d.Status.Conditions, "Ready"); c != nil && c.ObservedGeneration == d.Generation && d.Status.ObservedGeneration == d.Generation {
		out.StatusKnown, out.Reason = true, c.Reason
		switch {
		case c.Status == metav1.ConditionTrue:
			out.Phase = "Ready"
		case d.DeletionTimestamp != nil:
			out.Phase = ih.PhaseTerminating
			out.Remaining, out.Blocked, out.Uncertain = int(d.Status.Records.Managed), int(d.Status.Records.Conflicts), int(d.Status.Records.Drift)
		default:
			out.Phase = "Degraded"
		}
		if d.Status.LastSyncTime != nil {
			at := d.Status.LastSyncTime.UTC()
			out.LastSync = &at
		}
	}
	for _, status := range statuses {
		if status.Owner.ClusterID != r.opts.Store.Info().ClusterID || status.Owner.UID != out.UID || status.Owner.Namespace != out.Namespace || status.Owner.Name != out.Name {
			continue
		}
		out.Leader = status.Leader
		// An instance no job runs for (paused, suspended, waiting for its
		// license) writes no status on its object. Every replica derives
		// that local status from the same object, so it is shown when the
		// durable one does not describe this revision.
		if !out.StatusKnown && status.Revision == out.Revision && !status.Leader {
			out.StatusKnown, out.Phase, out.Reason = true, status.Phase, status.Reason
			if status.Phase == ih.PhaseTerminating {
				out.Remaining, out.Blocked, out.Uncertain = status.Result.Remaining, status.Result.Conflicts, status.Result.Pending
			}
		}
		break
	}
	return out
}

// holderError answers a command this replica's host has no running job for.
// Only when the instance's Lease is known to be held by another replica is
// the caller sent there; a suspended, unlicensed or otherwise paused
// instance, an instance between two jobs, or one whose Lease nobody holds is
// answered with its phase and reason instead.
func (r *kubeIntegrationRuntime) holderError(owner ih.InstanceID, d *public.Integration) error {
	elector := r.opts.Store.Elector(ih.ElectionKey(owner))
	if leader := elector.Leader(); leader != "" && leader != r.opts.Store.Identity() && !elector.IsLeader() {
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "this replica does not hold the integration's lease; send the command to the holder", Hint: kubeIntegrationHolderHint{Holder: leader}}}
	}
	summary := r.kubeIntegrationSummary(d, r.host.Statuses())
	return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "the integration is not running on this replica; retry once it is running", Hint: kubeIntegrationHolderHint{Phase: summary.Phase, Reason: summary.Reason}}}
}

// IntegrationExecute implements api.IntegrationAdministration on the replica
// that holds the instance's Lease.
func (r *kubeIntegrationRuntime) IntegrationExecute(ctx context.Context, in api.IntegrationExecution) (*api.IntegrationExecutionResult, error) {
	if len(validation.IsDNS1123Label(in.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(in.Name)) != 0 || in.Review.UID == "" || len(in.Review.UID) > 128 || in.Review.Revision == "" || len(in.Review.Revision) > 256 || len(in.Review.Params) > 64<<10 {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	switch in.Command {
	case "check", "preview", "actions", "sync", "act":
	default:
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	if in.Command != "sync" && (in.Review.Mode != "" || in.Review.PreviewDigest != "") || in.Command != "act" && (in.Action != "" || len(in.Review.Params) != 0) {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	objects, err := r.adminSnapshot(in.Namespace)
	if err != nil {
		return nil, err
	}
	reviewed, ok := objects.Get(store.Key{Kind: "Integration", Namespace: in.Namespace, Name: in.Name})
	if !ok {
		return nil, integrationAdminError(store.ErrNotFound)
	}
	if string(reviewed.GetUID()) != in.Review.UID || reviewed.GetResourceVersion() != in.Review.Revision {
		return nil, integrationAdminError(ih.ErrFenced)
	}
	owner := ih.InstanceID{ClusterID: r.opts.Store.Info().ClusterID, Namespace: in.Namespace, Name: in.Name, UID: in.Review.UID}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	capture, err := r.host.CaptureCommand(ctx, owner, in.Review.Revision)
	if errors.Is(err, ih.ErrUnavailable) {
		d, _ := reviewed.(*public.Integration)
		if d == nil {
			return nil, integrationAdminError(err)
		}
		return nil, r.holderError(owner, d)
	}
	if err != nil {
		return nil, integrationAdminError(err)
	}
	ctx, guard, err := capture.Bind(ctx)
	if err != nil {
		return nil, integrationAdminError(err)
	}
	ctx, _, _, cut, err := r.requestContext(ctx, guard.Ledger)
	if err != nil {
		return nil, integrationAdminError(err)
	}
	instance := cut.instances[owner]
	out := &api.IntegrationExecutionResult{}
	refusedBeforeEffect := false
	switch in.Command {
	case "check":
		checked, e := capture.Check(ctx)
		err = e
		out.Reachable, out.Authenticated = checked.Reachable, checked.Authenticated
	case "preview":
		var preview *ih.Preview
		preview, err = capture.Preview(ctx)
		if err == nil {
			out.Preview, err = integrationPublicPreview(preview)
		}
	case "sync":
		publisher := integrationPublishes(instance.Definition())
		if publisher && (in.Review.Mode != "publish" || !kinds.ValidDigest(in.Review.PreviewDigest)) || !publisher && (in.Review.Mode != "observe" || in.Review.PreviewDigest != "") {
			return nil, integrationAdminError(ih.ErrInvalid)
		}
		var result *ih.Result
		result, err = capture.Sync(ctx, in.Review.PreviewDigest)
		refusedBeforeEffect = errors.Is(err, ih.ErrUnsupported) || errors.Is(err, ih.ErrStalePreview) && (result == nil || result.Applied == 0 && result.Pending == 0 && result.Source == nil)
		if result != nil {
			out.Applied, out.Unchanged, out.Pending, out.Conflicts, out.Clients = result.Applied, result.Unchanged, result.Pending, result.Conflicts, result.Clients
			if result.Source != nil {
				out.SourceCommitted, out.SourcePublished = result.Source.Committed, result.Source.Published
				// A head that was never written, or whose write is known to
				// have lost its compare-and-swap, installed no source: chunks
				// staged for it are referenced by no head. A head write with
				// an unknown outcome comes back Pending and stays uncertain.
				refusedBeforeEffect = refusedBeforeEffect || !result.Source.Committed && result.Applied == 0 && result.Pending == 0
			}
		}
	case "actions":
		actions, e := capture.Actions(ctx)
		err = e
		out.Actions = make([]api.IntegrationAction, 0, len(actions))
		for _, a := range actions {
			out.Actions = append(out.Actions, api.IntegrationAction{Name: a.Name, Params: a.Params})
		}
	case "act":
		if in.Action == "" || len(in.Action) > 128 || strings.ContainsAny(in.Action, "/\\\r\n\x00") {
			return nil, integrationAdminError(ih.ErrInvalid)
		}
		if len(in.Review.Params) == 0 {
			in.Review.Params = json.RawMessage(`{}`)
		}
		_, err = capture.Invoke(ctx, in.Action, in.Review.Params)
		refusedBeforeEffect = errors.Is(err, ih.ErrUnsupported) || errors.Is(err, ih.ErrInvalid)
	}
	if err == nil {
		err = integrationProjectionSafeFor(ctx, instance, out)
	}
	effect := in.Command == "sync" || in.Command == "act"
	if effect {
		outcome := "completed"
		if err != nil && !refusedBeforeEffect {
			outcome = "uncertain: effects may have occurred and were not replayed"
		} else if err != nil {
			outcome = "refused before any effect"
		}
		r.opts.Logger.Info("integration."+in.Command, "integration", in.Namespace+"/"+in.Name, "uid", owner.UID, "revision", in.Review.Revision, "action", in.Action, "outcome", outcome)
	}
	if err != nil {
		if effect && !refusedBeforeEffect {
			out.Uncertain = true
			return nil, &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: "integration_uncertain", Message: "integration operation was not acknowledged; effects may have occurred and were not replayed", Hint: out}}
		}
		return nil, integrationAdminError(err)
	}
	out.Completed = true
	return out, nil
}

// IntegrationRelease implements the reviewed release command on Kubernetes
// (D367): the exact reviewed object, being deleted and holding the reserved
// finalizer, loses that finalizer by one metadata patch bound to its UID and
// resourceVersion, which removes it. It needs no license, credential,
// provider or Lease; a cleanup run in flight is fenced at its next effect.
// What the incarnation published and its private ledger stay.
func (r *kubeIntegrationRuntime) IntegrationRelease(ctx context.Context, namespace, name string, review api.IntegrationReview) (*api.IntegrationReleaseResult, error) {
	if ctx == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 || review.UID == "" || len(review.UID) > 128 || review.Revision == "" || len(review.Revision) > 256 || review.Mode != "" || review.PreviewDigest != "" || len(review.Params) != 0 {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key := store.Key{Kind: "Integration", Namespace: namespace, Name: name}
	var stored public.Integration
	if err := r.opts.Store.Get(ctx, key, &stored); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, integrationAdminError(ih.ErrFenced)
		}
		return nil, integrationAdminError(err)
	}
	if !integrationReleasable(&stored, review) {
		return nil, integrationAdminError(ih.ErrFenced)
	}
	owner := kinds.Owner{ClusterID: r.opts.Store.Info().ClusterID, Namespace: namespace, Name: name, UID: review.UID}
	out := &api.IntegrationReleaseResult{LedgerRows: -1, Operations: -1}
	retained := "rows the command did not count"
	if state, err := r.readKubeIntegrationState(ctx, owner, r.snapshotObjects); err == nil {
		out.LedgerRows, out.Operations, retained = len(state.ledgers), len(state.operations), state.String()
	} else if !errors.Is(err, ih.ErrCapacity) {
		return nil, integrationAdminError(err)
	}
	removed, err := r.opts.Store.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: true, ExpectedUID: review.UID, ExpectedResourceVersion: review.Revision})
	if err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTerminating) || errors.Is(err, store.ErrNotFound) {
			err = ih.ErrFenced
		}
		return nil, integrationAdminError(err)
	}
	out.Released = store.Terminated(removed)
	policy := stored.Spec.DeletionPolicy
	if policy == "" {
		policy = public.IntegrationDeletionRetain
	}
	r.opts.Logger.Info("integration.release", "integration", namespace+"/"+name, "uid", owner.UID, "reason", "released without cleanup by the reviewed release command: deletionPolicy is "+policy+"; removed "+store.IntegrationCleanupFinalizer, "retained", retained)
	r.host.Wake()
	return out, nil
}

var _ api.IntegrationAdministration = (*kubeIntegrationRuntime)(nil)
