package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
)

func (s *Server) commandCreateDomainEntries(ctx context.Context, a domainlist.Action, in *DomainEntriesCreate) (*domainWriteOutput, error) {
	if len(in.Values) == 0 || len(in.Values) > 1000 || len(in.Groups) > 100 {
		return nil, domainCommandTooLarge()
	}
	if in.List != "" {
		return s.commandCreateExceptions(ctx, a, in)
	}
	t, err := domainlist.ParseType(in.Type)
	if err != nil {
		return nil, domainError(err)
	}
	ns := s.namespaceOr(in.Namespace)
	var res *domainlist.Result
	d, err := s.runDomainCommand(ctx, DomainCommandTarget{a, "create"}, func(ctx context.Context, d *domainEdits) error {
		var err error
		res, err = d.service.Add(ctx, domainlist.AddRequest{Action: a, Namespace: ns, Type: t, Values: in.Values, Scope: domainlist.Scope{Groups: in.Groups}, Comment: in.Comment, Enabled: in.Enabled, Origin: public.EntryOrigin(in.Origin), Actor: s.actor(ctx)})
		return err
	})
	s.recordDomainAction(ctx, a, "add", ns, res, err, map[string]any{"type": t, "values": in.Values, "groups": in.Groups})
	if err != nil {
		return nil, err
	}
	out, err := d.result(ctx, s, a, ns, res, processedDomainValues(in.Values, res))
	if err != nil {
		return nil, err
	}
	return &domainWriteOutput{Body: out}, nil
}

func domainPatch(in *DomainEntryPatch, actor string) (domainlist.Patch, error) {
	p := domainlist.Patch{Enabled: in.Enabled, Comment: in.Comment, Value: in.Value, Actor: actor}
	if len(in.Groups) > 100 {
		return p, domainCommandTooLarge()
	}
	if in.Type != nil {
		t, err := domainlist.ParseType(*in.Type)
		if err != nil {
			return p, domainError(err)
		}
		p.Type = &t
	}
	switch {
	case in.Scope == nil && len(in.Groups) > 0:
		p.Scope = &domainlist.Scope{Groups: in.Groups}
	case in.Scope == nil:
	case *in.Scope == "everyone":
		p.Scope = &domainlist.Scope{}
	case *in.Scope != "groups":
		return p, errInvalid("scope: expected everyone or groups")
	case len(in.Groups) == 0:
		return p, errInvalid("groups: scope groups needs at least one ClientGroup")
	default:
		p.Scope = &domainlist.Scope{Groups: in.Groups}
	}
	return p, nil
}

func (s *Server) commandPatchDomainEntry(ctx context.Context, a domainlist.Action, id string, in *DomainEntryPatch) (*domainWriteOutput, error) {
	if _, _, _, _, ok := domainlist.ParseExceptionID(id); ok {
		if in.Comment != nil || in.Value != nil || in.Type != nil || in.Scope != nil || len(in.Groups) > 0 {
			return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, "a list exception has only its switch", "")
		}
		if in.Enabled == nil {
			return nil, errInvalid("body: enabled is required for a list exception")
		}
		d, err := s.runDomainCommand(ctx, DomainCommandTarget{a, "patch"}, func(ctx context.Context, d *domainEdits) error { return d.setException(ctx, s, a, id, *in.Enabled) })
		if err != nil {
			return nil, err
		}
		flushed := s.flushNames(d.names)
		v, err := d.exceptionView(ctx, s, a, id)
		if err != nil {
			return nil, domainError(err)
		}
		return &domainWriteOutput{Body: &DomainWriteResult{Items: []DomainEntry{*v}, Processed: ProcessedItems{Success: []ProcessedItem{{Item: id}}, Errors: []ProcessedItem{}}, Attached: []string{}, Skipped: []string{}, Flushed: flushed}}, nil
	}
	ns, t, v, err := domainlist.ParseID(id)
	if err != nil {
		return nil, domainError(err)
	}
	patch, err := domainPatch(in, s.actor(ctx))
	if err != nil {
		return nil, err
	}
	var res *domainlist.Result
	d, err := s.runDomainCommand(ctx, DomainCommandTarget{a, "patch"}, func(ctx context.Context, d *domainEdits) error {
		var err error
		res, err = d.service.Update(ctx, a, ns, t, v, patch)
		return err
	})
	s.recordDomainAction(ctx, a, "update", ns, res, err, map[string]any{"type": t, "value": v, "patch": in})
	if err != nil {
		return nil, err
	}
	out, err := d.result(ctx, s, a, ns, res, ProcessedItems{Success: []ProcessedItem{{Item: id}}, Errors: []ProcessedItem{}})
	if err != nil {
		return nil, err
	}
	return &domainWriteOutput{Body: out}, nil
}

func (s *Server) commandDeleteDomainEntry(ctx context.Context, a domainlist.Action, id string) (*emptyOutput, error) {
	var res *domainlist.Result
	ns := s.opts.DefaultNamespace
	d, err := s.runDomainCommand(ctx, DomainCommandTarget{a, "delete"}, func(ctx context.Context, d *domainEdits) error {
		if _, _, _, _, ok := domainlist.ParseExceptionID(id); ok {
			return d.setException(ctx, s, a, id, false)
		}
		var err error
		var t public.EntryType
		var v string
		ns, t, v, err = domainlist.ParseID(id)
		if err != nil {
			return err
		}
		res, err = d.service.Delete(ctx, a, ns, t, v)
		return err
	})
	s.recordDomainAction(ctx, a, "delete", ns, res, err, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	if res != nil {
		s.flushNames(res.Names)
	}
	s.flushNames(d.names)
	return &emptyOutput{}, nil
}

func (s *Server) commandBatchDomainEntries(ctx context.Context, a domainlist.Action, in *DomainEntriesBatch) (*domainWriteOutput, error) {
	if len(in.IDs) == 0 || len(in.IDs) > 1000 {
		return nil, domainCommandTooLarge()
	}
	if in.Op != "enable" && in.Op != "disable" && in.Op != "delete" {
		return nil, errInvalid("op: expected enable, disable or delete")
	}
	processed := ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}
	total := &domainlist.Result{}
	byNS := map[string][]string{}
	var exceptionIDs []string
	d, err := s.runDomainCommand(ctx, DomainCommandTarget{a, "batch"}, func(ctx context.Context, d *domainEdits) error {
		for _, id := range in.IDs {
			rollback := d.objects.Checkpoint()
			nameCount := len(d.names)
			var itemErr error
			if _, _, _, _, ok := domainlist.ParseExceptionID(id); ok {
				itemErr = d.setException(ctx, s, a, id, in.Op == "enable")
				if itemErr == nil && in.Op != "delete" {
					exceptionIDs = append(exceptionIDs, id)
				}
			} else {
				ns, t, v, err := domainlist.ParseID(id)
				itemErr = err
				if err == nil {
					var res *domainlist.Result
					if in.Op == "delete" {
						res, itemErr = d.service.Delete(ctx, a, ns, t, v)
					} else {
						on := in.Op == "enable"
						res, itemErr = d.service.Update(ctx, a, ns, t, v, domainlist.Patch{Enabled: &on, Actor: s.actor(ctx)})
					}
					if itemErr == nil {
						total.Names = append(total.Names, res.Names...)
						total.Lists = append(total.Lists, res.Lists...)
						total.Attached = append(total.Attached, res.Attached...)
						total.Skipped = append(total.Skipped, res.Skipped...)
						if in.Op != "delete" {
							byNS[ns] = append(byNS[ns], res.IDs...)
						}
					}
				}
			}
			if itemErr != nil {
				rollback()
				d.names = d.names[:nameCount]
				processed.Errors = append(processed.Errors, ProcessedItem{Item: id, Error: errorMessage(domainError(itemErr))})
			} else {
				processed.Success = append(processed.Success, ProcessedItem{Item: id})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(total.Attached)
	total.Attached = slices.Compact(total.Attached)
	slices.Sort(total.Skipped)
	total.Skipped = slices.Compact(total.Skipped)
	out := &DomainWriteResult{Items: []DomainEntry{}, Processed: processed, Attached: nonNil(total.Attached), Skipped: nonNil(total.Skipped), Flushed: s.flushNames(append(total.Names, d.names...))}
	for ns, ids := range byNS {
		result, err := d.result(ctx, s, a, ns, &domainlist.Result{IDs: ids}, processed)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, result.Items...)
	}
	for _, id := range exceptionIDs {
		v, err := d.exceptionView(ctx, s, a, id)
		if err != nil {
			return nil, domainError(err)
		}
		out.Items = append(out.Items, *v)
	}
	s.recordDomainAction(ctx, a, "batch", s.opts.DefaultNamespace, total, nil, map[string]any{"op": in.Op, "ids": in.IDs, "errors": len(processed.Errors)})
	return &domainWriteOutput{Body: out}, nil
}

func (s *Server) commandAddDomains(ctx context.Context, in *domainsInput) (*processedOutput, error) {
	if len(in.Body.Domains) == 0 || len(in.Body.Domains) > 1000 {
		return nil, domainCommandTooLarge()
	}
	action := domainlist.Action(in.Action)
	ns := s.namespaceOr(in.Body.Namespace)
	out := newProcessed()
	var names []string
	_, err := s.runDomainCommand(ctx, DomainCommandTarget{action, "add"}, func(ctx context.Context, d *domainEdits) error {
		// Preserve the old mixed exact/wildcard/regex and per-type result contract,
		// but checkpoint each type so a refused attachment cannot leak a list edit.
		byType := map[public.EntryType][]string{}
		var order []public.EntryType
		for _, value := range in.Body.Domains {
			t := public.EntryExact
			if in.Kind == "regex" {
				t = public.EntryRegex
			} else if v := strings.TrimSpace(value); strings.HasPrefix(v, "*.") || strings.HasPrefix(v, "||") {
				t = public.EntryWildcard
			}
			if _, ok := byType[t]; !ok {
				order = append(order, t)
			}
			byType[t] = append(byType[t], value)
		}
		for _, t := range order {
			rollback := d.objects.Checkpoint()
			req := domainlist.AddRequest{Action: action, Namespace: ns, Type: t, Values: byType[t], Origin: public.OriginAPI, Actor: s.actor(ctx)}
			var res *domainlist.Result
			var err error
			_, group, ui := domainlist.ParseListName(in.Body.List)
			if in.Body.List == "" || ui {
				if group != "" {
					req.Scope.Groups = []string{group}
				}
				res, err = d.service.Add(ctx, req)
			} else {
				res, err = d.service.AddToList(ctx, req, in.Body.List)
			}
			if err != nil {
				rollback()
				for _, value := range byType[t] {
					out.fail(value, domainError(err))
				}
				continue
			}
			processed := processedDomainValues(byType[t], res)
			out.Processed.Success = append(out.Processed.Success, processed.Success...)
			out.Processed.Errors = append(out.Processed.Errors, processed.Errors...)
			names = append(names, res.Names...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.flushNames(names)
	s.recordDomainAction(ctx, action, "add", ns, nil, nil, map[string]any{"values": in.Body.Domains, "list": in.Body.List})
	return &processedOutput{Body: out}, nil
}
