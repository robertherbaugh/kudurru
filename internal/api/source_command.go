package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// commandPatchSourceEntries retains one current transaction for source lookup,
// authorization, validation and staging. No-op responses cross the same checks;
// a failed command never retries against a newer object or the live Store.
func (s *Server) commandPatchSourceEntries(ctx context.Context, in *patchSourceEntriesInput) (*patchSourceEntriesOutput, error) {
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	k, _ := kindByPlural("blocklists")
	key := keyFor(k, in.Namespace, in.Name)
	var result *SourceEntriesResult
	var format parser.Format
	err := s.opts.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize ObjectAuthorize) error {
		if tx == nil || authorize == nil {
			return errUnavailable("current source command unavailable")
		}
		if err := authorize(ctx, commandWriteAttributes(k, key, nil, nil)); err != nil {
			return err
		}
		var existing public.BlockList
		if err := tx.Get(ctx, key, &existing); err != nil {
			return err
		}
		if existing.DeletionTimestamp != nil {
			return errConflict("the BlockList is being deleted", "wait for deletion to finish")
		}
		if in.IfMatch != "" && in.IfMatch != existing.ResourceVersion {
			return errPrecondition(in.IfMatch, existing.ResourceVersion)
		}
		idx, err := findSource(&existing, in.Source)
		if err != nil {
			return err
		}
		candidate := existing.DeepCopy()
		src := &candidate.Spec.Sources[idx]
		if src.RPZ != nil {
			return newError(http.StatusUnprocessableEntity, KeyUnprocessable, "RPZ sources do not have editable line entries", "edit the RPZ feed at its origin")
		}
		result = &SourceEntriesResult{Added: []string{}, Removed: []string{}, Enabled: []string{}, Disabled: []string{}}
		if err := applySourcePatch(src, &in.Body, result, s.opts.Now); err != nil {
			return err
		}
		body, err := typedObject(candidate)
		if err != nil {
			return err
		}
		u := &unstructured.Unstructured{Object: body}
		u.SetAPIVersion(public.GroupVersion.String())
		u.SetKind(string(key.Kind))
		sanitizeDocument(u)
		raw, err := json.Marshal(u.Object)
		if err != nil {
			return err
		}
		if len(raw) > MaxObjectBytes {
			return store.ErrTooLarge
		}
		prepared, err := s.prepareCommandWrite(ctx, writeRequest{kind: k, ifMatch: in.IfMatch}, store.ManagerAPI, u, key, &existing)
		if err != nil {
			return err
		}
		written, err := prepared.apply(ctx, tx)
		if err != nil {
			return err
		}
		result.ResourceVersion = written.obj.GetResourceVersion()
		format = parser.Format(src.Format)
		return nil
	})
	if err != nil {
		return nil, storeError(err)
	}
	if result == nil || result.ResourceVersion == "" {
		return nil, errUnavailable("current source command returned no result")
	}
	// Cache effects happen only after the command commits and releases its pins.
	for _, value := range append(slices.Clone(result.Enabled), result.Disabled...) {
		if kind, name, ok := domainlist.FromRule(value, format); ok {
			s.flushName(domainlist.MatchName(kind, name))
		}
	}
	return &patchSourceEntriesOutput{ETag: result.ResourceVersion, Body: result}, nil
}
