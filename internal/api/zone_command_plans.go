package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	maxZoneCommandObjects = 10000
	maxZoneCommandBytes   = 64 << 20
)

type zoneCommandMutation struct {
	key          store.Key
	old, desired store.Object // nil desired means deletion
	ifMatch      string
	createOnly   bool
}

func zoneRequirement(target ZoneCommandTarget, resource, verb string) rbac.Attributes {
	return rbac.Attributes{Resource: resource, Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: target.Zone}, Name: target.Zone}
}

func (s *Server) runZoneCommand(ctx context.Context, target ZoneCommandTarget, prepare ZoneCommandPrepare) error {
	if err := s.readOnly(); err != nil {
		return err
	}
	if s.opts.ZoneCommand == nil {
		return errUnavailable("current zone command unavailable")
	}
	return storeError(s.opts.ZoneCommand(ctx, target, prepare))
}

func currentZoneObject(ctx context.Context, tx *sqlite.CommandTx, name string) (*public.DNSZone, error) {
	if tx == nil {
		return nil, errUnavailable("current zone transaction unavailable")
	}
	var z public.DNSZone
	if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &z, nil
}

func currentZoneRecordSets(ctx context.Context, tx *sqlite.CommandTx, zoneName string) ([]*public.DNSRecordSet, error) {
	var sets []*public.DNSRecordSet
	err := commandScan(ctx, tx, "DNSRecordSet", func(obj store.Object) error {
		rs, ok := obj.(*public.DNSRecordSet)
		if !ok {
			return errUnavailable("invalid current record set")
		}
		if rs.Spec.ZoneRef.Name == zoneName {
			sets = append(sets, rs)
		}
		return nil
	})
	sort.Slice(sets, func(i, j int) bool {
		if sets[i].Namespace != sets[j].Namespace {
			return sets[i].Namespace < sets[j].Namespace
		}
		return sets[i].Name < sets[j].Name
	})
	return sets, err
}

// makeZonePlan seals only same-zone public operations. All application checks
// and every schema/owner/ETag decision happen after authorization, before Stage.
func (s *Server) makeZonePlan(tx *sqlite.CommandTx, target ZoneCommandTarget, attrs []rbac.Attributes, mutations []zoneCommandMutation, check func() error, after func([]writeResult)) (*ZoneCommandPlan, error) {
	return s.makeZonePlanWithDryRun(tx, target, attrs, mutations, check, after, false)
}

func (s *Server) makeZonePlanWithDryRun(tx *sqlite.CommandTx, target ZoneCommandTarget, attrs []rbac.Attributes, mutations []zoneCommandMutation, check func() error, after func([]writeResult), dryRun bool) (*ZoneCommandPlan, error) {
	if len(mutations) > maxZoneCommandObjects {
		return nil, errUnavailable("zone command exceeds object bounds")
	}
	total := 0
	seen := make(map[store.Key]bool, len(mutations))
	for _, m := range mutations {
		if seen[m.key] {
			return nil, errUnavailable("duplicate zone command target")
		}
		seen[m.key] = true
		for _, obj := range []store.Object{m.old, m.desired} {
			if obj == nil {
				continue
			}
			raw, err := json.Marshal(obj)
			if err != nil {
				return nil, err
			}
			if len(raw) > MaxObjectBytes || len(raw) > maxZoneCommandBytes-total {
				return nil, zoneCommandTooLarge()
			}
			total += len(raw)
		}
	}
	return newZoneCommandPlan(tx, target, attrs, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		prepared := make([]preparedCommandWrite, len(mutations))
		for i, m := range mutations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := checkZoneMutation(target, m); err != nil {
				return err
			}
			if m.desired == nil {
				if m.old == nil {
					return errUnavailable("missing current deletion target")
				}
				if owner := store.ManagedBy(m.old); !apiMayDelete(owner) {
					return errManagedBy(m.key, owner)
				}
				if m.ifMatch != "" && m.ifMatch != m.old.GetResourceVersion() {
					return errPrecondition(m.ifMatch, m.old.GetResourceVersion())
				}
				continue
			}
			body, err := typedObject(m.desired)
			if err != nil {
				return err
			}
			u := &unstructured.Unstructured{Object: body}
			u.SetAPIVersion(public.GroupVersion.String())
			u.SetKind(string(m.key.Kind))
			sanitizeDocument(u)
			k, ok := kindByKind(m.key.Kind)
			if !ok {
				return errUnavailable("unsupported zone command kind")
			}
			prepared[i], err = s.prepareCommandWrite(ctx, writeRequest{kind: k, createOnly: m.createOnly, ifMatch: m.ifMatch}, store.ManagerAPI, u, m.key, m.old)
			if err != nil {
				return err
			}
		}
		results := make([]writeResult, len(mutations))
		if !dryRun {
			for i, m := range mutations {
				if err := ctx.Err(); err != nil {
					return err
				}
				if m.desired == nil {
					if err := tx.Delete(ctx, m.key, apiDeleteOptions(store.ManagedBy(m.old))); err != nil {
						return err
					}
					continue
				}
				var err error
				results[i], err = prepared[i].apply(ctx, tx)
				if err != nil {
					return err
				}
			}
		}
		if after != nil {
			after(results)
		}
		return nil
	}), nil
}

func checkZoneMutation(target ZoneCommandTarget, m zoneCommandMutation) error {
	switch target.Action {
	case ZoneCommandUpsertRecord:
		if m.key.Kind != "DNSRecordSet" || m.desired == nil {
			return errUnavailable("invalid record upsert plan")
		}
	case ZoneCommandDeleteRecord:
		if m.key.Kind != "DNSRecordSet" || m.old == nil {
			return errUnavailable("invalid record deletion plan")
		}
		if m.desired != nil {
			old, ok := m.old.(*public.DNSRecordSet)
			desired, good := m.desired.(*public.DNSRecordSet)
			if !ok || !good || !zoneRemovalOnly(old, desired) {
				return errUnavailable("record deletion plan may only remove endpoints")
			}
		}
	case ZoneCommandDeleteZone:
		if m.desired != nil {
			return errUnavailable("cascade plan may only delete")
		}
	case ZoneCommandImport:
	default:
		return errUnavailable("invalid zone command action")
	}
	for _, obj := range []store.Object{m.old, m.desired} {
		if obj == nil {
			continue
		}
		switch o := obj.(type) {
		case *public.DNSZone:
			if m.key != (store.Key{Kind: "DNSZone", Name: target.Zone}) || o.Name != target.Zone || o.Namespace != "" {
				return errUnavailable("invalid zone command target")
			}
		case *public.DNSRecordSet:
			if m.key.Kind != "DNSRecordSet" || o.Name != m.key.Name || o.Namespace != m.key.Namespace || o.Spec.ZoneRef.Name != target.Zone {
				return newError(http.StatusUnprocessableEntity, KeyUnprocessable, fmt.Sprintf("record set %s belongs to another zone", m.key), "choose another recordSet")
			}
		default:
			return errUnavailable("unsupported zone command kind")
		}
	}
	return nil
}

// A delete-only grant can shrink a RecordSet, never alter retained endpoints,
// metadata, status or zone scope. Surviving duplicate endpoints retain order.
func zoneRemovalOnly(old, desired *public.DNSRecordSet) bool {
	if len(desired.Spec.Endpoints) >= len(old.Spec.Endpoints) {
		return false
	}
	expected := old.DeepCopy()
	expected.Spec.Endpoints = desired.Spec.Endpoints
	if !reflect.DeepEqual(expected, desired) {
		return false
	}
	i := 0
	for _, ep := range old.Spec.Endpoints {
		if i < len(desired.Spec.Endpoints) && reflect.DeepEqual(ep, desired.Spec.Endpoints[i]) {
			i++
		}
	}
	return i == len(desired.Spec.Endpoints)
}

func zoneCommandTooLarge() *Error {
	return newError(http.StatusRequestEntityTooLarge, KeyTooLarge, "zone command exceeds its object or aggregate byte bounds", "split the operation into smaller requests")
}

// Check all variable-width endpoint fields before deep copy/conversion. The
// later document cap also accounts for JSON escaping and structural overhead.
func zoneEndpointBytes(ep public.Endpoint) (int, error) {
	n := 0
	add := func(v string) bool {
		if len(v) > MaxObjectBytes-n {
			return false
		}
		n += len(v)
		return true
	}
	if len(ep.Targets) > MaxObjectBytes/3 || len(ep.Labels) > MaxObjectBytes/6 || len(ep.ProviderSpecific) > MaxObjectBytes/6 {
		return 0, zoneCommandTooLarge()
	}
	for _, v := range []string{ep.DNSName, ep.RecordType, ep.SetIdentifier} {
		if !add(v) {
			return 0, zoneCommandTooLarge()
		}
	}
	for _, v := range ep.Targets {
		if !add(v) {
			return 0, zoneCommandTooLarge()
		}
	}
	for k, v := range ep.Labels {
		if !add(k) || !add(v) {
			return 0, zoneCommandTooLarge()
		}
	}
	for _, v := range ep.ProviderSpecific {
		if !add(v.Name) || !add(v.Value) {
			return 0, zoneCommandTooLarge()
		}
	}
	if ep.Kudurru != nil && (!add(ep.Kudurru.Comment) || !add(ep.Kudurru.View)) {
		return 0, zoneCommandTooLarge()
	}
	return n, nil
}

func zoneMissing(name string) *Error { return errNotFound("zone " + name) }

func (s *Server) commandUpsertZoneRecord(ctx context.Context, in *upsertRecordInput) (*recordOutput, error) {
	if s.opts.ZoneCommand == nil {
		return nil, errUnavailable("current zone command unavailable")
	}
	if _, err := zoneEndpointBytes(in.Body); err != nil {
		return nil, err
	}
	if in.Body.DNSName == "" || in.Body.RecordType == "" {
		return nil, errInvalid("body: dnsName and recordType are required")
	}
	ep := *in.Body.DeepCopy()
	target := ZoneCommandTarget{Zone: in.Zone, Action: ZoneCommandUpsertRecord}
	var out *recordOutput
	var responseErr error
	err := s.runZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*ZoneCommandPlan, error) {
		attrs := []rbac.Attributes{zoneRequirement(target, "records", "write")}
		z, err := currentZoneObject(ctx, tx, in.Zone)
		if err != nil {
			return nil, err
		}
		if z == nil {
			responseErr = zoneMissing(in.Zone)
			return s.makeZonePlan(tx, target, attrs, nil, nil, nil)
		}
		ep.RecordType = strings.ToUpper(ep.RecordType)
		ep.DNSName = absName(ep.DNSName, z.Spec.ZoneName)
		if ep.DNSName == "" {
			ep.DNSName = "@"
		}
		ref := in.RecordSet
		if ref == "" {
			ref = "api-" + z.Name
		}
		ns, name := s.splitRef(ref, in.Namespace)
		key := store.Key{Kind: "DNSRecordSet", Namespace: ns, Name: name}
		var current public.DNSRecordSet
		var old store.Object
		err = tx.Get(ctx, key, &current)
		if err == nil {
			old = &current
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		var desired *public.DNSRecordSet
		if old != nil {
			desired = current.DeepCopy()
		} else {
			desired = &public.DNSRecordSet{}
			desired.Name, desired.Namespace = name, ns
			desired.Spec.ZoneRef.Name = z.Name
		}
		replaced := false
		for i, cur := range desired.Spec.Endpoints {
			if sameOwner(cur.DNSName, ep.DNSName, z.Spec.ZoneName) && strings.EqualFold(cur.RecordType, ep.RecordType) && cur.SetIdentifier == ep.SetIdentifier {
				desired.Spec.Endpoints[i], replaced = ep, true
				break
			}
		}
		if !replaced {
			desired.Spec.Endpoints = append(desired.Spec.Endpoints, ep)
		}
		return s.makeZonePlan(tx, target, attrs, []zoneCommandMutation{{key: key, old: old, desired: desired, createOnly: old == nil}}, func() error {
			if z.Spec.Type != public.ZoneTypePrimary {
				return errConflict("only primary zones accept record edits", "edit the records on the zone's primary")
			}
			return nil
		}, func(results []writeResult) {
			written := results[0].obj.(*public.DNSRecordSet)
			status := http.StatusCreated
			if replaced {
				status = http.StatusOK
			}
			record := recordOf(written, ep)
			for _, cur := range written.Spec.Endpoints {
				if sameOwner(cur.DNSName, ep.DNSName, z.Spec.ZoneName) && strings.EqualFold(cur.RecordType, ep.RecordType) && cur.SetIdentifier == ep.SetIdentifier {
					record = recordOf(written, cur)
					break
				}
			}
			out = &recordOutput{Status: status, Body: &recordBody{ZoneRecord: record}}
		})
	})
	if err != nil {
		return nil, err
	}
	return out, responseErr
}

func (s *Server) commandDeleteZoneRecord(ctx context.Context, in *deleteRecordInput) (*emptyOutput, error) {
	target := ZoneCommandTarget{Zone: in.Zone, Action: ZoneCommandDeleteRecord}
	var responseErr error
	err := s.runZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*ZoneCommandPlan, error) {
		attrs := []rbac.Attributes{zoneRequirement(target, "records", "delete")}
		z, err := currentZoneObject(ctx, tx, in.Zone)
		if err != nil {
			return nil, err
		}
		if z == nil {
			responseErr = zoneMissing(in.Zone)
			return s.makeZonePlan(tx, target, attrs, nil, nil, nil)
		}
		sets, err := currentZoneRecordSets(ctx, tx, z.Name)
		if err != nil {
			return nil, err
		}
		var onlyNS, onlyName string
		if in.RecordSet != "" {
			onlyNS, onlyName = s.splitRef(in.RecordSet, "")
		}
		var mutations []zoneCommandMutation
		for _, old := range sets {
			if onlyName != "" && (old.Namespace != onlyNS || old.Name != onlyName) {
				continue
			}
			desired := old.DeepCopy()
			keep := desired.Spec.Endpoints[:0]
			for _, ep := range desired.Spec.Endpoints {
				if sameOwner(ep.DNSName, in.Name, z.Spec.ZoneName) && strings.EqualFold(ep.RecordType, in.Type) && (in.SetIdentifier == "" || ep.SetIdentifier == in.SetIdentifier) {
					continue
				}
				keep = append(keep, ep)
			}
			if len(keep) == len(old.Spec.Endpoints) {
				continue
			}
			desired.Spec.Endpoints = keep
			m := zoneCommandMutation{key: store.Key{Kind: "DNSRecordSet", Namespace: old.Namespace, Name: old.Name}, old: old}
			if len(keep) != 0 {
				m.desired = desired
			}
			mutations = append(mutations, m)
		}
		if len(mutations) == 0 {
			responseErr = errNotFound(fmt.Sprintf("record %s %s in zone %s", in.Name, strings.ToUpper(in.Type), z.Name))
		}
		return s.makeZonePlan(tx, target, attrs, mutations, nil, nil)
	})
	if err != nil {
		return nil, err
	}
	if responseErr != nil {
		return nil, responseErr
	}
	return &emptyOutput{}, nil
}

func (s *Server) commandDeleteZone(ctx context.Context, in *deleteZoneInput) (*deleteZoneOutput, error) {
	input := *in
	in = &input
	target := ZoneCommandTarget{Zone: in.Zone, Action: ZoneCommandDeleteZone}
	var out *ZoneDeleteResult
	var responseErr error
	var before map[string]any
	err := s.runZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*ZoneCommandPlan, error) {
		attrs := []rbac.Attributes{zoneRequirement(target, "zones", "delete")}
		z, err := currentZoneObject(ctx, tx, in.Zone)
		if err != nil {
			return nil, err
		}
		if z == nil {
			responseErr = zoneMissing(in.Zone)
			return s.makeZonePlan(tx, target, attrs, nil, nil, nil)
		}
		sets, err := currentZoneRecordSets(ctx, tx, z.Name)
		if err != nil {
			return nil, err
		}
		if len(sets) > 0 {
			attrs = append(attrs, zoneRequirement(target, "records", "delete"))
		}
		res := &ZoneDeleteResult{Zone: z.Name, ZoneName: z.Spec.ZoneName, Type: string(z.Spec.Type), DryRun: in.DryRun, RecordSets: make([]string, 0, len(sets)), RecordSetCount: len(sets)}
		var mutations []zoneCommandMutation
		for _, rs := range sets {
			res.RecordSets = append(res.RecordSets, rs.Namespace+"/"+rs.Name)
			res.EndpointCount += len(rs.Spec.Endpoints)
			mutations = append(mutations, zoneCommandMutation{key: store.Key{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name}, old: rs})
		}
		mutations = append(mutations, zoneCommandMutation{key: store.Key{Kind: "DNSZone", Name: z.Name}, old: z, ifMatch: in.IfMatch})
		plan, err := s.makeZonePlanWithDryRun(tx, target, attrs, mutations, func() error {
			if in.IfMatch != "" && in.IfMatch != z.ResourceVersion {
				return errPrecondition(in.IfMatch, z.ResourceVersion)
			}
			if isRootZone(z) {
				return errDefaultZone(z)
			}
			var managed []ObjectOwner
			if owner := store.ManagedBy(z); !apiMayDelete(owner) {
				managed = append(managed, ObjectOwner{Kind: "DNSZone", Name: z.Name, ManagedBy: owner})
			}
			for _, rs := range managedRecordSets(sets) {
				managed = append(managed, recordSetOwner(rs))
			}
			if len(managed) != 0 {
				e := newError(http.StatusConflict, KeyManagedBy, fmt.Sprintf("zone %s cannot be deleted: managed by another writer: %s; change or remove them there", z.Name, listedObjects(managed, true)), managed[0].ManagedBy)
				e.Body.Objects = managed
				return e
			}
			if !in.DryRun {
				before = map[string]any{"zoneName": z.Spec.ZoneName, "type": z.Spec.Type, "recordSetCount": len(sets), "endpointCount": res.EndpointCount}
				if len(res.RecordSets) <= 50 {
					before["recordSets"] = res.RecordSets
				}
			}
			return nil
		}, func([]writeResult) { out = res }, in.DryRun)
		return plan, err
	})
	// Keep the legacy aggregate action summary outside the current command.
	// Per-object durable audit is staged atomically by the concrete transaction.
	defer func() {
		if before != nil {
			s.recordAction(ctx, "zones.delete", audit.Resource{Kind: "DNSZone", Name: in.Zone}, err, before, nil)
		}
	}()
	if err != nil {
		return nil, err
	}
	if responseErr != nil {
		return nil, responseErr
	}
	if out == nil {
		return nil, errUnavailable("current zone command returned no result")
	}
	if !out.DryRun {
		out.Flushed = s.flushZone(out.ZoneName)
	}
	return &deleteZoneOutput{Body: out}, nil
}
