package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// commandWriteObject receives only the normalized, sanitized document. All
// decisions that depend on existing state belong to the concrete command.
func (s *Server) commandWriteObject(ctx context.Context, req writeRequest, manager string, u *unstructured.Unstructured, key store.Key) (writeResult, error) {
	var result writeResult
	err := s.opts.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize ObjectAuthorize) error {
		if tx == nil || authorize == nil {
			return errUnavailable("current object command unavailable")
		}
		existing := req.kind.newObject()
		if err := tx.Get(ctx, key, existing); errors.Is(err, store.ErrNotFound) {
			existing = nil
		} else if err != nil {
			return err
		}
		if err := authorize(ctx, commandWriteAttributes(req.kind, key, u.Object, existing)); err != nil {
			return err
		}
		force := false
		if existing != nil {
			if req.createOnly {
				return errConflict(key.String()+" already exists", "PUT it to replace it")
			}
			switch owner := store.ManagedBy(existing); owner {
			case "", manager:
			case store.ManagerBootstrap:
				force = true
			default:
				return errManagedBy(key, owner)
			}
			if req.ifMatch != "" && req.ifMatch != existing.GetResourceVersion() {
				return errPrecondition(req.ifMatch, existing.GetResourceVersion())
			}
		} else if req.ifMatch != "" {
			return errPrecondition(req.ifMatch, "")
		}
		var old *unstructured.Unstructured
		if existing != nil {
			m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
			if err != nil {
				return err
			}
			old = &unstructured.Unstructured{Object: m}
		}
		if err := s.opts.Validator.ValidateUnstructured(ctx, u, old); err != nil {
			return err
		}
		obj := req.kind.newObject()
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(u.Object, obj, true); err != nil {
			return errInvalid("%v", err)
		}
		gvk, _, _ := s.opts.Validator.GroupVersionKind(req.kind.kind)
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		if err := s.opts.Validator.Validate(ctx, obj, existing); err != nil {
			return err
		}
		if existing != nil && !force && store.ManagedBy(existing) == manager && sameContent(existing, obj, manager) {
			result = writeResult{obj: existing, result: ResultUnchanged, key: key}
			return nil
		}
		if existing == nil && req.kind.kind == "Policy" {
			if err := s.commandPolicyLimit(ctx, tx, key); err != nil {
				return err
			}
		}
		opts := store.ApplyOptions{Manager: manager, Force: force, CreateOnly: existing == nil}
		if existing != nil {
			opts.ExpectedResourceVersion = existing.GetResourceVersion()
		}
		written, err := tx.Apply(ctx, obj, opts)
		if err != nil {
			return err
		}
		outcome := ResultUpdated
		if existing == nil {
			outcome = ResultCreated
		}
		result = writeResult{obj: written, result: outcome, key: key}
		return nil
	})
	if err != nil {
		return writeResult{}, storeError(err)
	}
	if result.obj == nil {
		return writeResult{}, errUnavailable("current object command returned no result")
	}
	return result, nil
}

func commandWriteAttributes(k kindInfo, key store.Key, body map[string]any, existing store.Object) []rbac.Attributes {
	attribute := func(scope rbac.Scope) rbac.Attributes {
		return rbac.Attributes{Resource: k.resource, Verb: "write", Scope: scope, Namespace: key.Namespace, Name: key.Name}
	}
	switch k.kind {
	case "DNSZone":
		if existing == nil {
			return []rbac.Attributes{attribute(clusterScope)}
		}
		return []rbac.Attributes{attribute(objectScope(k, existing))}
	case "DNSRecordSet":
		zone, _ := nestedString(body, "spec", "zoneRef", "name")
		scope := clusterScope
		if zone != "" {
			scope = rbac.Scope{Kind: rbac.ScopeZone, Name: zone}
		}
		attrs := []rbac.Attributes{attribute(scope)}
		if existing != nil {
			if oldScope := objectScope(k, existing); oldScope != scope {
				attrs = append(attrs, attribute(oldScope))
			}
		}
		return attrs
	default:
		return []rbac.Attributes{attribute(clusterScope)}
	}
}

func (s *Server) commandDeleteObject(ctx context.Context, k kindInfo, key store.Key, ifMatch string) error {
	if err := s.readOnly(); err != nil {
		return err
	}
	err := s.opts.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize ObjectAuthorize) error {
		if tx == nil || authorize == nil {
			return errUnavailable("current object command unavailable")
		}
		obj := k.newObject()
		err := tx.Get(ctx, key, obj)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		scope := clusterScope
		if err == nil {
			scope = objectScope(k, obj)
		} else if k.kind == "DNSZone" {
			scope = rbac.Scope{Kind: rbac.ScopeZone, Name: key.Name}
		}
		if authErr := authorize(ctx, []rbac.Attributes{{Resource: k.resource, Verb: "delete", Scope: scope, Namespace: key.Namespace, Name: key.Name}}); authErr != nil {
			return authErr
		}
		if err != nil {
			return err
		}
		owner := store.ManagedBy(obj)
		if !apiMayDelete(owner) {
			return errManagedBy(key, owner)
		}
		if ifMatch != "" && ifMatch != obj.GetResourceVersion() {
			return errPrecondition(ifMatch, obj.GetResourceVersion())
		}
		if zone, ok := obj.(*public.DNSZone); ok {
			if err := commandZoneDeletable(ctx, tx, zone); err != nil {
				return err
			}
		}
		// Command holds SQLite's writer lock from the exact read through delete;
		// no separate DeleteOptions RV is necessary or available in this seam.
		return tx.Delete(ctx, key, apiDeleteOptions(owner))
	})
	return storeError(err)
}

// commandScan bounds transaction-local scans shared by zone and Policy guards.
// The query uses finite pages; no live Store or zonecache can join this cut.
func commandScan(ctx context.Context, tx *sqlite.CommandTx, kind store.Kind, visit func(store.Object) error) error {
	count, bytes := 0, 0
	seen := map[string]bool{}
	for cursor := ""; ; {
		page, err := tx.List(ctx, kind, store.ListOptions{Limit: 500, Continue: cursor})
		if err != nil {
			return err
		}
		for _, obj := range page.Items {
			count++
			if count > 100000 {
				return errUnavailable("current %s inventory exceeds command bounds", kind)
			}
			raw, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			bytes += len(raw)
			if bytes > 64<<20 {
				return errUnavailable("current %s inventory exceeds command bounds", kind)
			}
			if err := visit(obj); err != nil {
				return err
			}
		}
		if page.Continue == "" {
			return nil
		}
		if len(page.Continue) > 4096 || seen[page.Continue] || len(page.Items) == 0 {
			return errUnavailable("current %s inventory is incomplete", kind)
		}
		seen[page.Continue] = true
		cursor = page.Continue
	}
}

func commandZoneDeletable(ctx context.Context, tx *sqlite.CommandTx, zone *public.DNSZone) error {
	if isRootZone(zone) {
		return errDefaultZone(zone)
	}
	var references []ObjectOwner
	err := commandScan(ctx, tx, "DNSRecordSet", func(obj store.Object) error {
		rs, ok := obj.(*public.DNSRecordSet)
		if !ok {
			return errUnavailable("invalid current record set")
		}
		if rs.Spec.ZoneRef.Name == zone.Name {
			references = append(references, recordSetOwner(rs))
		}
		return nil
	})
	if err != nil || len(references) == 0 {
		return err
	}
	e := newError(http.StatusConflict, KeyZoneNotEmpty,
		fmt.Sprintf("zone %s is referenced by %d record sets (%s); deleting the DNSZone alone would orphan them", zone.Name, len(references), listedObjects(references, false)),
		"DELETE "+BasePath+"/zones/"+zone.Name+" deletes the zone with its record sets (?dryRun=true lists them first)")
	e.Body.Objects = references
	return e
}

func (s *Server) commandPolicyLimit(ctx context.Context, tx *sqlite.CommandTx, key store.Key) error {
	if s.opts.License == nil {
		return nil
	}
	// Production supplies the same Manager already held by the root host pin.
	// Capture is lock-free; Status/CheckPolicyCreate would reenter its RLock.
	manager, ok := s.opts.License.(*license.Manager)
	if !ok {
		return errUnavailable("current policy license capability unavailable")
	}
	cut := manager.Capture()
	if !cut.Current() {
		return errUnavailable("current policy license unavailable")
	}
	if err := tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !cut.Current() {
			return errUnavailable("current policy license expired")
		}
		return nil
	}); err != nil {
		return err
	}
	status := cut.Status()
	if status.PolicyLimit == 0 {
		return nil
	}
	count := 0
	if err := commandScan(ctx, tx, "Policy", func(store.Object) error { count++; return nil }); err != nil {
		return err
	}
	if key.Namespace != s.opts.DefaultNamespace || key.Name != license.CommunityPolicyName || count >= status.PolicyLimit {
		return newError(http.StatusPaymentRequired, KeyLicense,
			"the current license does not permit creating this Policy",
			"without a valid license key only the default policy ("+s.opts.DefaultNamespace+"/"+license.CommunityPolicyName+") exists: change it instead, or add a license key; existing policies keep working")
	}
	return nil
}
