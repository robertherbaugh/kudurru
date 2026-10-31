package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// commandExceptionRules opens only the original publication's body receipt.
// This runs before current command/owner pins; no source is fetched on demand.
func (s *Server) commandExceptionRules(ctx context.Context, bl *public.BlockList, idx int, keys map[string]bool) (map[string][]string, error) {
	src := bl.Spec.Sources[idx]
	if blocking.IsRPZSource(src) {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, "RPZ sources do not have editable line entries", "")
	}
	if src.URL == "" && src.ConfigMapRef == nil {
		return s.sourceRules(ctx, bl, idx, keys)
	}
	if s.opts.SourceBodyReads == nil {
		return nil, sourceReadUnavailable()
	}
	name := blocking.SourceName(idx, src)
	digest := ""
	for _, status := range bl.Status.Sources {
		if status.Name == name {
			digest = status.Digest
		}
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return nil, sourceReadUnavailable()
	}
	release, err := s.acquireBodyScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	body, actual, size, err := s.opts.SourceBodyReads.OpenSourceBody(ctx, bl.Namespace, bl.Name, name)
	if body != nil {
		defer func() { _ = body.Close() }()
	}
	if err != nil || body == nil {
		return nil, sourceReadUnavailable()
	}
	if actual != digest || size < 0 || size > maxCapturedSourceBytes {
		return nil, sourceReadUnavailable()
	}
	limited := &io.LimitedReader{R: body, N: size + 1}
	out := map[string][]string{}
	count := 0
	matchedBytes := 0
	_, err = parser.Parse(limited, parser.Options{Format: parser.Format(src.Format), HostsSubdomains: src.Subdomains, KeepInvalid: -1, KeepUnsupported: -1}, func(rule parser.Rule) error {
		count++
		if count > 1000000 {
			return domainCommandTooLarge()
		}
		if count%scanCheck == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if keys[rule.Pattern] {
			pattern := strings.Clone(rule.Pattern)
			value := rule.String()
			if !slices.Contains(out[pattern], value) {
				matchedBytes += len(value)
				if matchedBytes > maxDomainCommandBytes {
					return domainCommandTooLarge()
				}
				out[pattern] = append(out[pattern], value)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if limited.N != 1 {
		return nil, sourceReadUnavailable()
	}
	return out, nil
}

func (s *Server) commandCreateExceptions(ctx context.Context, a domainlist.Action, in *DomainEntriesCreate) (*domainWriteOutput, error) {
	if s.opts.DomainCommand == nil {
		return nil, errUnavailable("current domain command unavailable")
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	if len(in.Groups) > 0 {
		return nil, errInvalid("groups: a list exception applies to the clients of the list's policies; leave groups empty")
	}
	ns, list := s.namespaceOr(in.Namespace), in.List
	if lns, lname, ok := strings.Cut(in.List, "/"); ok {
		ns, list = lns, lname
	}
	bl, readErr := s.capturedDomainList(ctx, ns, list)
	// Preflight errors are returned only after current proof and final admission.
	var preflightErr error
	if readErr != nil {
		preflightErr = readErr
	}
	if bl != nil && exceptionAction(bl) != a {
		preflightErr = newError(http.StatusUnprocessableEntity, KeyUnprocessable, fmt.Sprintf("BlockList %s/%s is a %s list; switching its entries off does not %s them", ns, list, bl.Spec.Mode, a), "")
	}
	if _, _, ui := domainlist.ParseListName(list); ui {
		preflightErr = newError(http.StatusUnprocessableEntity, KeyUnprocessable, "the Allowlist and Denylist lists have no exceptions", "edit or switch off their entries instead")
	}
	t, err := domainlist.ParseType(in.Type)
	if err != nil {
		return nil, domainError(err)
	}
	processed := ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}
	keys := make([]string, len(in.Values))
	keyErrs := make([]error, len(in.Values))
	want := map[string]bool{}
	for i, value := range in.Values {
		keys[i], keyErrs[i] = exceptionKey(t, value)
		if keyErrs[i] == nil {
			want[keys[i]] = true
		}
	}
	type sourceHits struct {
		name  string
		rules map[string][]string
	}
	var sources []sourceHits
	if preflightErr == nil && len(want) > 0 {
		for idx, src := range bl.Spec.Sources {
			name := blocking.SourceName(idx, src)
			if len(src.Entries) > 0 || in.Source != "" && in.Source != name {
				continue
			}
			rules, err := s.commandExceptionRules(ctx, bl, idx, want)
			if err != nil {
				preflightErr = err
				break
			}
			sources = append(sources, sourceHits{name, rules})
		}
	}
	var ids []string
	for i, value := range in.Values {
		if keyErrs[i] != nil {
			processed.Errors = append(processed.Errors, ProcessedItem{Item: value, Error: keyErrs[i].Error()})
			continue
		}
		found := false
		for _, src := range sources {
			for _, rule := range src.rules[keys[i]] {
				id := domainlist.ExceptionID(ns, list, src.name, rule)
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
				found = true
			}
		}
		if found {
			processed.Success = append(processed.Success, ProcessedItem{Item: value})
		} else {
			processed.Errors = append(processed.Errors, ProcessedItem{Item: value, Error: fmt.Sprintf("%q is not an entry of %s/%s", value, ns, list)})
		}
	}
	d, err := s.runDomainCommand(ctx, DomainCommandTarget{a, "create"}, func(ctx context.Context, d *domainEdits) error {
		if preflightErr != nil {
			return preflightErr
		}
		current, err := currentDomainList(ctx, d, ns, list)
		if err != nil {
			return err
		}
		if current.UID != bl.UID || current.ResourceVersion != bl.ResourceVersion {
			return sourceReadUnavailable()
		}
		for _, id := range ids {
			if err := d.setException(ctx, s, a, id, true); err != nil {
				return err
			}
		}
		return nil
	})
	s.recordDomainAction(ctx, a, "exception", ns, nil, err, map[string]any{"list": list, "values": in.Values})
	if err != nil {
		return nil, err
	}
	out := &DomainWriteResult{Items: []DomainEntry{}, Processed: processed, Attached: []string{}, Skipped: []string{}, Flushed: s.flushNames(d.names)}
	for _, id := range ids {
		v, err := d.exceptionView(ctx, s, a, id)
		if err != nil {
			return nil, domainError(err)
		}
		out.Items = append(out.Items, *v)
	}
	return &domainWriteOutput{Body: out}, nil
}

func (s *Server) commandQueryAction(ctx context.Context, in *queryActionInput) (*processedOutput, error) {
	if s.opts.DomainCommand == nil {
		return nil, errUnavailable("current domain command unavailable")
	}
	// Query history is read outside the object transaction. Its selected name is
	// immutable input to the subsequent bounded domain command, never authority.
	reader := s.opts.CentralQuerylog
	var readErr error
	if in.Scope != sqlitelog.ScopeCluster || reader == nil {
		reader, readErr = s.reader()
	}
	var value string
	if readErr == nil {
		page, err := reader.Queries(ctx, sqlitelog.QueryFilter{Cursor: strconv.FormatUint(in.ID+1, 10), Limit: 1})
		if err != nil {
			readErr = readerError(err)
		} else if len(page.Queries) == 0 || page.Queries[0].ID != in.ID {
			readErr = errNotFound(fmt.Sprintf("query %d", in.ID))
		} else {
			value = page.Queries[0].Domain
		}
	}
	if readErr != nil {
		_, err := s.runDomainCommand(ctx, DomainCommandTarget{domainlist.Action(in.Body.Action), "query"}, func(context.Context, *domainEdits) error { return readErr })
		return nil, err
	}
	return s.commandAddDomains(ctx, &domainsInput{Action: in.Body.Action, Kind: "exact", Body: DomainsRequest{Domains: []string{value}, Namespace: in.Body.Namespace}})
}

// Discovery uses the original captured object, never getBlockList's legacy
// Store path. The command later binds this exact UID/resourceVersion to tx.
func (s *Server) capturedDomainList(ctx context.Context, ns, name string) (*public.BlockList, error) {
	if s.opts.ObjectReads == nil {
		return nil, sourceReadUnavailable()
	}
	objects, _, err := s.capturedObjects(ctx)
	if err != nil {
		return nil, err
	}
	obj, found := objects.Get(store.Key{Kind: "BlockList", Namespace: ns, Name: name})
	if !found {
		return nil, errNotFound(ns + "/" + name)
	}
	bl, ok := obj.(*public.BlockList)
	if !ok || bl.UID == "" || bl.ResourceVersion == "" || len(bl.Spec.Sources) > 256 || len(bl.Status.Sources) > 256 {
		return nil, sourceReadUnavailable()
	}
	raw, err := json.Marshal(bl)
	if err != nil || len(raw) > MaxObjectBytes {
		return nil, domainCommandTooLarge()
	}
	return bl, nil
}
