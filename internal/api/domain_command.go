package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const maxDomainCommandObjects = 10000
const maxDomainCommandBytes = 32 << 20

// DomainCommandTarget identifies a closed convenience command. Its implicit
// policy reference edits and empty-list cleanup retain blocklists.write.
type DomainCommandTarget struct {
	Action    domainlist.Action
	Operation string
}
type DomainCommandPrepare func(context.Context, *sqlite.CommandTx) (*DomainCommandPlan, error)
type DomainCommand func(context.Context, DomainCommandTarget, DomainCommandPrepare) error

// DomainCommandPlan seals a bounded edit to its original transaction. Only the
// domain planners can construct it; root authorizes before invoking Apply.
type DomainCommandPlan struct {
	tx     *sqlite.CommandTx
	target DomainCommandTarget
	apply  func(context.Context, *sqlite.CommandTx) error
	state  atomic.Uint32
}

func (p *DomainCommandPlan) PreparedFor(tx *sqlite.CommandTx, target DomainCommandTarget) bool {
	return p != nil && tx != nil && p.tx == tx && p.target == target && p.apply != nil && p.state.Load() == 0
}
func (p *DomainCommandPlan) Requirements() []rbac.Attributes {
	if p == nil || p.state.Load() == 2 {
		return nil
	}
	return []rbac.Attributes{{Resource: "blocklists", Verb: "write", Scope: clusterScope}}
}
func (p *DomainCommandPlan) Apply(ctx context.Context, tx *sqlite.CommandTx) error {
	if p == nil || ctx == nil || tx == nil || tx != p.tx || p.apply == nil || !p.state.CompareAndSwap(0, 1) {
		return errUnavailable("current domain plan unavailable")
	}
	defer p.Release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.apply(ctx, tx)
}
func (p *DomainCommandPlan) Release() {
	if p != nil {
		p.state.Store(2)
	}
}

func domainCommandTooLarge() error {
	return newError(http.StatusRequestEntityTooLarge, KeyTooLarge, "domain command exceeds its object or aggregate byte bounds", "split the operation into smaller requests")
}

type domainEdits struct {
	objects *domainlist.Plan
	service *domainlist.Service
	names   []string
}

func (s *Server) currentDomainEdits(ctx context.Context, tx *sqlite.CommandTx) (*domainEdits, error) {
	if tx == nil {
		return nil, errUnavailable("current domain transaction unavailable")
	}
	var objects []store.Object
	total := 0
	for _, kind := range []store.Kind{"BlockList", "Policy", "ClientGroup"} {
		if err := commandScan(ctx, tx, kind, func(o store.Object) error {
			raw, err := json.Marshal(o)
			if err != nil {
				return err
			}
			if len(objects) >= maxDomainCommandObjects || len(raw) > MaxObjectBytes || len(raw) > maxDomainCommandBytes-total {
				return domainCommandTooLarge()
			}
			total += len(raw)
			objects = append(objects, o)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	p := domainlist.NewPlan(objects)
	return &domainEdits{objects: p, service: domainlist.New(domainlist.Options{Store: p, Now: s.opts.Now, Logger: s.log})}, nil
}

// runDomainCommand plans only against detached objects from tx. No legacy Store
// mutation, cache operation, aggregate audit or source body I/O occurs here.
func (s *Server) runDomainCommand(ctx context.Context, target DomainCommandTarget, edit func(context.Context, *domainEdits) error) (*domainEdits, error) {
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	if s.opts.DomainCommand == nil {
		return nil, errUnavailable("current domain command unavailable")
	}
	var edits *domainEdits
	var responseErr error
	err := s.opts.DomainCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*DomainCommandPlan, error) {
		var err error
		edits, err = s.currentDomainEdits(ctx, tx)
		if err != nil {
			return nil, err
		}
		responseErr = edit(ctx, edits)
		var changes []domainlist.Change
		if responseErr == nil {
			changes = edits.objects.Changes()
		}
		if len(changes) > maxDomainCommandObjects {
			return nil, domainCommandTooLarge()
		}
		total := 0
		for _, change := range changes {
			for _, o := range []store.Object{change.Original, change.Desired} {
				if o == nil {
					continue
				}
				raw, err := json.Marshal(o)
				if err != nil {
					return nil, err
				}
				if len(raw) > MaxObjectBytes || len(raw) > maxDomainCommandBytes-total {
					return nil, domainCommandTooLarge()
				}
				total += len(raw)
			}
		}
		return &DomainCommandPlan{tx: tx, target: target, apply: func(ctx context.Context, tx *sqlite.CommandTx) error {
			prepared := make([]preparedCommandWrite, len(changes))
			for i, change := range changes {
				if change.Key.Kind != "BlockList" && change.Key.Kind != "Policy" {
					return errUnavailable("invalid domain command kind")
				}
				if change.Original != nil && change.Original.GetDeletionTimestamp() != nil {
					return errConflict("the object is being deleted", "wait for deletion to finish")
				}
				if change.Key.Kind == "Policy" {
					old, ok := change.Original.(*public.Policy)
					next, good := change.Desired.(*public.Policy)
					if !ok || !good {
						return errUnavailable("domain command may only edit existing policy references")
					}
					expected := old.DeepCopy()
					expected.Spec.AllowListRefs = next.Spec.AllowListRefs
					expected.Spec.BlockListRefs = next.Spec.BlockListRefs
					store.SetManagedBy(expected, store.ManagedBy(next))
					if !reflect.DeepEqual(expected, next) {
						return errUnavailable("domain command changed non-reference policy fields")
					}
				}
				if change.Desired == nil {
					if change.Original == nil {
						return errUnavailable("missing domain deletion target")
					}
					if owner := store.ManagedBy(change.Original); !apiMayDelete(owner) {
						return errManagedBy(change.Key, owner)
					}
					continue
				}
				body, err := typedObject(change.Desired)
				if err != nil {
					return err
				}
				u := &unstructured.Unstructured{Object: body}
				u.SetAPIVersion(public.GroupVersion.String())
				u.SetKind(string(change.Key.Kind))
				sanitizeDocument(u)
				k, _ := kindByPlural(strings.ToLower(string(change.Key.Kind)) + "s")
				// Policy plural is irregular.
				if change.Key.Kind == "Policy" {
					k, _ = kindByPlural("policies")
				}
				prepared[i], err = s.prepareCommandWrite(ctx, writeRequest{kind: k}, store.ManagerAPI, u, change.Key, change.Original)
				if err != nil {
					return err
				}
			}
			for i, change := range changes {
				if change.Desired == nil {
					if err := tx.Delete(ctx, change.Key, apiDeleteOptions(store.ManagedBy(change.Original))); err != nil {
						return err
					}
				} else {
					if _, err := prepared[i].apply(ctx, tx); err != nil {
						return err
					}
				}
			}
			return nil
		}}, nil
	})
	if err != nil {
		return nil, domainError(err)
	}
	if responseErr != nil {
		return nil, domainError(responseErr)
	}
	if edits == nil {
		return nil, errUnavailable("current domain command returned no result")
	}
	return edits, nil
}

func (s *Server) useDomainCommand() bool {
	return s.opts.DomainCommand != nil || s.opts.ObjectCommand != nil
}

func (d *domainEdits) result(ctx context.Context, s *Server, a domainlist.Action, ns string, res *domainlist.Result, processed ProcessedItems) (*DomainWriteResult, error) {
	out := &DomainWriteResult{Items: []DomainEntry{}, Processed: processed, Attached: nonNil(res.Attached), Skipped: nonNil(res.Skipped), Flushed: s.flushNames(res.Names)}
	set, err := d.service.Load(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, id := range res.IDs {
		_, t, v, err := domainlist.ParseID(id)
		if err != nil {
			return nil, err
		}
		if e := set.Find(a, ns, t, v); e != nil {
			out.Items = append(out.Items, s.domainView(e, set))
		}
	}
	return out, nil
}

func processedDomainValues(values []string, res *domainlist.Result) ProcessedItems {
	out := ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}
	failed := map[string]bool{}
	for _, e := range res.Errors {
		failed[e.Item] = true
		out.Errors = append(out.Errors, ProcessedItem{Item: e.Item, Error: itemError(e.Err)})
	}
	for _, v := range values {
		if !failed[v] {
			out.Success = append(out.Success, ProcessedItem{Item: v})
		}
	}
	return out
}

func currentDomainList(ctx context.Context, d *domainEdits, ns, name string) (*public.BlockList, error) {
	var bl public.BlockList
	if err := d.objects.Get(ctx, store.Key{Kind: "BlockList", Namespace: ns, Name: name}, &bl); err != nil {
		return nil, err
	}
	if bl.DeletionTimestamp != nil {
		return nil, errConflict("the BlockList is being deleted", "wait for deletion to finish")
	}
	if owner := store.ManagedBy(&bl); !apiMayDelete(owner) {
		return nil, errManagedBy(store.KeyOf(&bl), owner)
	}
	return &bl, nil
}

func (d *domainEdits) setException(ctx context.Context, s *Server, a domainlist.Action, id string, on bool) error {
	ns, list, source, value, ok := domainlist.ParseExceptionID(id)
	if !ok {
		return domainlist.ErrInvalid
	}
	bl, err := currentDomainList(ctx, d, ns, list)
	if err != nil {
		return err
	}
	if exceptionAction(bl) != a {
		return errNotFound("exception " + id)
	}
	if _, _, ui := domainlist.ParseListName(list); ui {
		return newError(http.StatusUnprocessableEntity, KeyUnprocessable, "the Allowlist and Denylist lists have no exceptions", "")
	}
	idx, err := findSource(bl, source)
	if err != nil {
		return err
	}
	if blocking.IsRPZSource(bl.Spec.Sources[idx]) {
		return newError(http.StatusUnprocessableEntity, KeyUnprocessable, "RPZ sources do not have editable line entries", "edit the RPZ feed at its origin")
	}
	patch := SourceEntriesPatch{Enable: []string{value}}
	if on {
		patch = SourceEntriesPatch{Disable: []string{value}}
	}
	if err := applySourcePatch(&bl.Spec.Sources[idx], &patch, &SourceEntriesResult{}, s.opts.Now); err != nil {
		return err
	}
	_, err = d.objects.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI, Force: store.ManagedBy(bl) == store.ManagerBootstrap})
	if t, v, ok := domainlist.FromRule(value, parser.Format(bl.Spec.Sources[idx].Format)); ok {
		d.names = append(d.names, domainlist.MatchName(t, v))
	}
	return err
}

func (d *domainEdits) exceptionView(ctx context.Context, s *Server, a domainlist.Action, id string) (*DomainEntry, error) {
	ns, list, source, value, _ := domainlist.ParseExceptionID(id)
	set, err := d.service.Load(ctx, ns)
	if err != nil {
		return nil, err
	}
	e := set.FindException(ns, list, source, value)
	if e == nil {
		var bl public.BlockList
		if err := d.objects.Get(ctx, store.Key{Kind: "BlockList", Namespace: ns, Name: list}, &bl); err != nil {
			return nil, err
		}
		idx, err := findSource(&bl, source)
		if err != nil {
			return nil, err
		}
		t, _, ok := domainlist.FromRule(value, parser.Format(bl.Spec.Sources[idx].Format))
		if !ok {
			t = public.EntryExact
		}
		off := false
		e = &domainlist.Entry{Kind: domainlist.KindException, Namespace: ns, Action: a, Type: t, Value: value, Copies: []domainlist.Copy{{List: ns + "/" + list, Source: source, ManagedBy: store.ManagedBy(&bl), Entry: public.ListEntry{Value: value, Type: t, Enabled: &off}}}}
	}
	v := s.domainView(e, set)
	return &v, nil
}
