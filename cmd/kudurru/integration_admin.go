package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/util/validation"
)

func integrationAdminError(err error) error {
	if refusal, ok := asPrimaryRequired(err); ok {
		return refusal
	}
	status, key, message := http.StatusServiceUnavailable, api.KeyUnavailable, "original integration operation unavailable"
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, key, message = http.StatusNotFound, api.KeyNotFound, "integration not found"
	case errors.Is(err, ih.ErrInvalid):
		status, key, message = http.StatusBadRequest, api.KeyInvalid, "invalid integration review or command"
	case errors.Is(err, servingauth.ErrCredentials):
		status, key, message = http.StatusUnauthorized, api.KeyUnauthenticated, "original credential is unavailable"
	case errors.Is(err, servingauth.ErrDenied):
		status, key, message = http.StatusForbidden, api.KeyForbidden, "integration permission denied"
	case errors.Is(err, ih.ErrFenced), errors.Is(err, ih.ErrStalePreview), errors.Is(err, servingauth.ErrDependency):
		status, key, message = http.StatusConflict, api.KeyConflict, "original integration review or authority changed"
	case errors.Is(err, ih.ErrUnsupported):
		status, key, message = http.StatusNotImplemented, api.KeyNotImplemented, "integration capability is not implemented"
	case errors.Is(err, ih.ErrCapacity):
		status, key, message = http.StatusRequestEntityTooLarge, api.KeyTooLarge, "integration projection exceeds its bound"
	case errors.Is(err, ih.ErrBusy):
		status, key, message = http.StatusServiceUnavailable, api.KeyUnavailable, "integration worker is busy"
	}
	return &api.Error{Status: status, Body: api.ErrorBody{Key: key, Message: message}}
}

func (r *integrationRuntime) adminObjects(ctx context.Context, namespace string) (zonecache.ObjectSet, error) {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return zonecache.ObjectSet{}, integrationAdminError(ih.ErrInvalid)
	}
	v, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return zonecache.ObjectSet{}, integrationAdminError(err)
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return zonecache.ObjectSet{}, integrationAdminError(err)
	}
	now := r.s.opts.Now()
	principal, err := v.Authenticate(ctx, raw, now)
	if err != nil {
		return zonecache.ObjectSet{}, integrationAdminError(err)
	}
	decision, err := v.Authorize(principal, rbac.Attributes{Resource: "integrations", Verb: "read", Namespace: namespace, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}, now)
	if err != nil || !decision.Allowed {
		return zonecache.ObjectSet{}, integrationAdminError(servingauth.ErrDenied)
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return zonecache.ObjectSet{}, integrationAdminError(err)
	}
	return objects.Objects, nil
}

func (r *integrationRuntime) IntegrationList(ctx context.Context, namespace string) ([]api.IntegrationSummary, error) {
	objects, err := r.adminObjects(ctx, namespace)
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
		d := obj.(*public.Integration)
		if d.Namespace == namespace {
			out = append(out, r.integrationSummary(d, statuses))
		}
	}
	slices.SortFunc(out, func(a, b api.IntegrationSummary) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (r *integrationRuntime) IntegrationGet(ctx context.Context, namespace, name string) (*api.IntegrationSummary, error) {
	objects, err := r.adminObjects(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	obj, ok := objects.Get(store.Key{Kind: "Integration", Namespace: namespace, Name: name})
	if !ok {
		return nil, integrationAdminError(store.ErrNotFound)
	}
	out := r.integrationSummary(obj.(*public.Integration), r.host.Statuses())
	return &out, nil
}

func (r *integrationRuntime) integrationSummary(d *public.Integration, statuses []ih.InstanceStatus) api.IntegrationSummary {
	out := api.IntegrationSummary{Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), Revision: d.ResourceVersion, Type: d.Spec.Type, Suspended: d.Spec.Suspend, DeletionPolicy: d.Spec.DeletionPolicy, Phase: "Unknown", Reason: "NoMatchingObservation"}
	if out.DeletionPolicy == "" {
		out.DeletionPolicy = public.IntegrationDeletionRetain
	}
	if d.DeletionTimestamp != nil {
		// A published object that is being deleted is held by a finalizer:
		// without one the store would have removed it in the same write.
		at := d.DeletionTimestamp.UTC()
		out.Terminating, out.DeletionRequestedAt = true, &at
	}
	for _, status := range statuses {
		if status.Owner.ClusterID != r.s.opts.Store.Info().ClusterID || status.Owner.UID != out.UID || status.Owner.Namespace != out.Namespace || status.Owner.Name != out.Name || status.Revision != out.Revision {
			continue
		}
		out.StatusKnown = true
		out.Phase = status.Phase
		out.Reason = status.Reason
		out.Leader = status.Leader
		if status.Phase == ih.PhaseTerminating {
			out.Remaining, out.Blocked, out.Uncertain = status.Result.Remaining, status.Result.Conflicts, status.Result.Pending
		}
		if !status.LastSync.IsZero() {
			at := status.LastSync
			out.LastSync = &at
		}
		break
	}
	return out
}

func (r *integrationRuntime) IntegrationExecute(ctx context.Context, in api.IntegrationExecution) (*api.IntegrationExecutionResult, error) {
	if len(validation.IsDNS1123Label(in.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(in.Name)) != 0 || in.Review.UID == "" || len(in.Review.UID) > 128 || in.Review.Revision == "" || len(in.Review.Revision) > 256 || len(in.Review.Params) > 64<<10 {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	verb := "read"
	switch in.Command {
	case "check":
		verb = "check"
	case "preview", "actions":
	case "sync":
		verb = "sync"
	case "act":
		verb = "act"
	default:
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	if in.Command != "sync" && (in.Review.Mode != "" || in.Review.PreviewDigest != "") || in.Command != "act" && (in.Action != "" || len(in.Review.Params) != 0) {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	owner := ih.InstanceID{ClusterID: r.s.opts.Store.Info().ClusterID, Namespace: in.Namespace, Name: in.Name, UID: in.Review.UID}
	op, err := r.beginOperation(ctx, owner, in.Review.Revision, verb)
	if err != nil {
		return nil, integrationAdminError(err)
	}
	defer op.close()
	out := &api.IntegrationExecutionResult{}
	refusedBeforeEffect := false
	switch in.Command {
	case "check":
		checked, e := op.capture.Check(op.ctx)
		err = e
		out.Reachable, out.Authenticated = checked.Reachable, checked.Authenticated
	case "preview":
		var preview *ih.Preview
		preview, err = op.capture.Preview(op.ctx)
		if err == nil {
			out.Preview, err = integrationPublicPreview(preview)
		}
	case "sync":
		publisher := integrationPublishes(op.cut.instances[owner].Definition())
		if publisher && (in.Review.Mode != "publish" || !kinds.ValidDigest(in.Review.PreviewDigest)) || !publisher && (in.Review.Mode != "observe" || in.Review.PreviewDigest != "") {
			return nil, integrationAdminError(ih.ErrInvalid)
		}
		var result *ih.Result
		result, err = op.capture.Sync(op.ctx, in.Review.PreviewDigest)
		// A stale review with no begun effect is a conflict. Once any intent
		// is pending or applied, retain the uncertain partial outcome instead.
		refusedBeforeEffect = errors.Is(err, ih.ErrUnsupported) || errors.Is(err, ih.ErrStalePreview) && (result == nil || result.Applied == 0 && result.Pending == 0 && result.Source == nil)
		if result != nil {
			out.Applied, out.Unchanged, out.Pending, out.Conflicts, out.Clients = result.Applied, result.Unchanged, result.Pending, result.Conflicts, result.Clients
			if result.Source != nil {
				out.SourceCommitted, out.SourcePublished = result.Source.Committed, result.Source.Published
			}
		}
	case "actions":
		actions, e := op.capture.Actions(op.ctx)
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
		_, err = op.capture.Invoke(op.ctx, in.Action, in.Review.Params)
		// These errors originate in host capability/parameter admission, before Invoke.
		refusedBeforeEffect = errors.Is(err, ih.ErrUnsupported) || errors.Is(err, ih.ErrInvalid)
	}
	if err == nil {
		err = integrationProjectionSafe(op, out)
	}
	var audit *integrationAuditRow
	if in.Command == "sync" || in.Command == "act" {
		audit = &integrationAuditRow{action: "integration." + in.Command, name: in.Action}
	}
	if err == nil {
		err = op.finish(audit)
	}
	if err != nil {
		if !refusedBeforeEffect && audit != nil {
			out.Uncertain = true
			// An effect that failed before the final transaction is still
			// audited as uncertain when that transaction can commit. A
			// refused final transaction leaves no row, as it leaves no status.
			if !op.finishStarted.Load() {
				audit.uncertain = true
				_ = op.finish(audit)
			}
			return nil, &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: "integration_uncertain", Message: "integration operation was not acknowledged; effects may have occurred and were not replayed", Hint: out}}
		}
		return nil, integrationAdminError(err)
	}
	out.Completed = true
	return out, nil
}

func integrationPublicPreview(in *ih.Preview) (*api.IntegrationPreview, error) {
	if in == nil || !kinds.ValidDigest(in.Digest) || len(in.Plans) > 256 || len(in.Plans) != len(in.Domains) {
		return nil, ih.ErrInvalid
	}
	out := &api.IntegrationPreview{Digest: in.Digest, Operations: []api.IntegrationPlanOperation{}, Conflicts: []api.IntegrationPlanConflict{}}
	for index, p := range in.Plans {
		if p == nil || len(out.Operations)+len(out.Conflicts)+len(p.Operations)+len(p.Conflicts) > 1000 {
			return nil, ih.ErrCapacity
		}
		for _, o := range p.Operations {
			out.Operations = append(out.Operations, api.IntegrationPlanOperation{Domains: slices.Clone(in.Domains[index]), Name: o.Key.Name, Type: o.Key.Type, Action: o.Action, Policy: string(o.Policy), Before: integrationPublicRecord(o.Before), After: integrationPublicRecord(o.After)})
		}
		for _, c := range p.Conflicts {
			out.Conflicts = append(out.Conflicts, api.IntegrationPlanConflict{Domains: slices.Clone(in.Domains[index]), Name: c.Name, Type: c.Type, Reason: c.Reason})
		}
		out.Unchanged += p.Unchanged
	}
	return out, nil
}

func integrationPublicRecord(in *public.Endpoint) *api.IntegrationRecord {
	if in == nil {
		return nil
	}
	return &api.IntegrationRecord{Name: in.DNSName, Type: in.RecordType, TTL: int64(in.RecordTTL), Targets: slices.Clone(in.Targets)}
}

func integrationProjectionSafe(op *integrationOperation, out *api.IntegrationExecutionResult) error {
	return integrationProjectionSafeFor(op.ctx, op.cut.instances[op.guard.Ledger.Owner], out)
}

// integrationProjectionSafeFor bounds a command result and refuses one that
// echoes any named credential of the captured instance (both drivers).
func integrationProjectionSafeFor(ctx context.Context, i *ih.Instance, out *api.IntegrationExecutionResult) error {
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > 1<<20 {
		return ih.ErrCapacity
	}
	if i == nil {
		return ih.ErrFenced
	}
	secrets := make([]string, 0, len(i.Definition().Spec.Credentials))
	for name := range i.Definition().Spec.Credentials {
		secret, err := i.Secrets().Resolve(ctx, name)
		if err != nil {
			return ih.ErrProvider
		}
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	// Inspect decoded strings too: JSON escaping cannot hide a named capability.
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ih.ErrProvider
		}
		value, ok := token.(string)
		if !ok {
			continue
		}
		for _, secret := range secrets {
			if strings.Contains(value, secret) {
				return ih.ErrProvider
			}
		}
	}
	return nil
}

var _ api.IntegrationAdministration = (*integrationRuntime)(nil)
