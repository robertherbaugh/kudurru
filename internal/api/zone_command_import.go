package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// zoneCommandReader checks original request lifetime whenever the bounded
// importer reads more input. Its existing expanded-record ceiling also applies.
type zoneCommandReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r zoneCommandReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Server) commandImportZone(ctx context.Context, in *importInput) (*importOutput, error) {
	if s.opts.ZoneCommand == nil {
		return nil, errUnavailable("current zone command unavailable")
	}
	if len(in.RawBody) > maxBodyBytes {
		return nil, zoneCommandTooLarge()
	}
	target := ZoneCommandTarget{Zone: in.Zone, Action: ZoneCommandImport}
	var out *ImportResult
	var responseErr error
	err := s.runZoneCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*ZoneCommandPlan, error) {
		attrs := []rbac.Attributes{zoneRequirement(target, "records", "write")}
		z, err := currentZoneObject(ctx, tx, in.Zone)
		if err != nil {
			return nil, err
		}
		if z == nil && in.Origin == "" {
			e := zoneMissing(in.Zone)
			e.Body.Hint = "pass ?origin=<zone apex> to create the zone from the file"
			responseErr = e
			return s.makeZonePlan(tx, target, attrs, nil, nil, nil)
		}
		origin := in.Origin
		if z != nil {
			origin = z.Spec.ZoneName
		}
		if z == nil {
			attr := zoneRequirement(target, "zones", "write")
			attr.Scope = clusterScope
			attrs = append(attrs, attr)
		}
		im, err := zone.ImportBIND(zoneCommandReader{ctx: ctx, reader: bytes.NewReader(in.RawBody)}, origin)
		if err != nil {
			return nil, newError(http.StatusBadRequest, KeyInvalid, err.Error(), "the body must be an RFC 1035 master file for "+origin)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Bound all imported strings/counts before chunk copies/conversion. The
		// document and aggregate exact JSON caps are enforced by makeZonePlan.
		if len(im.Endpoints) > zone.MaxImportRecords || len(im.Warnings) > zone.MaxImportRecords {
			return nil, zoneCommandTooLarge()
		}
		total := 0
		for _, ep := range im.Endpoints {
			n, err := zoneEndpointBytes(ep)
			if err != nil || n > maxZoneCommandBytes-total {
				return nil, zoneCommandTooLarge()
			}
			total += n
		}
		for _, warning := range im.Warnings {
			if len(warning) > maxZoneCommandBytes-total {
				return nil, zoneCommandTooLarge()
			}
			total += len(warning)
		}
		ns := in.Namespace
		if ns == "" {
			ns = s.opts.DefaultNamespace
		}
		res := &ImportResult{Zone: in.Zone, ZoneName: im.Spec.ZoneName, Created: z == nil, Serial: im.Serial, Warnings: im.Warnings, RecordSets: []string{}}
		if res.Warnings == nil {
			res.Warnings = []string{}
		}
		var mutations []zoneCommandMutation
		if z == nil {
			desired := &public.DNSZone{Spec: im.Spec}
			desired.Name = in.Zone
			mutations = append(mutations, zoneCommandMutation{key: store.Key{Kind: "DNSZone", Name: in.Zone}, desired: desired, createOnly: true})
		}
		written := map[string]bool{}
		for start := 0; start < len(im.Endpoints); start += zone.MaxEndpointsPerRecordSet {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(mutations) >= maxZoneCommandObjects {
				return nil, zoneCommandTooLarge()
			}
			name := fmt.Sprintf("import-%s-%d", in.Zone, start/zone.MaxEndpointsPerRecordSet+1)
			key := store.Key{Kind: "DNSRecordSet", Namespace: ns, Name: name}
			var current public.DNSRecordSet
			var old store.Object
			err := tx.Get(ctx, key, &current)
			if err == nil {
				old = &current
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			desired := &public.DNSRecordSet{}
			desired.Name, desired.Namespace = name, ns
			desired.Labels = map[string]string{labelImport: in.Zone}
			desired.Spec.ZoneRef.Name = in.Zone
			end := min(start+zone.MaxEndpointsPerRecordSet, len(im.Endpoints))
			desired.Spec.Endpoints = im.Endpoints[start:end:end]
			mutations = append(mutations, zoneCommandMutation{key: key, old: old, desired: desired, createOnly: old == nil})
			written[name] = true
			res.RecordSets = append(res.RecordSets, ns+"/"+name)
			res.Endpoints += end - start
		}
		sets, err := currentZoneRecordSets(ctx, tx, in.Zone)
		if err != nil {
			return nil, err
		}
		cleanup := false
		for _, old := range sets {
			// A label by itself never grants authority over another zone or owner.
			if old.Namespace != ns || old.Labels[labelImport] != in.Zone || written[old.Name] || store.ManagedBy(old) != store.ManagerAPI {
				continue
			}
			cleanup = true
			mutations = append(mutations, zoneCommandMutation{key: store.Key{Kind: "DNSRecordSet", Namespace: old.Namespace, Name: old.Name}, old: old})
		}
		if cleanup {
			attrs = append(attrs, zoneRequirement(target, "records", "delete"))
		}
		return s.makeZonePlan(tx, target, attrs, mutations, func() error {
			if z != nil && z.Spec.Type != public.ZoneTypePrimary {
				return errConflict("only primary zones accept record imports", "edit the records on the zone's primary")
			}
			return nil
		}, func([]writeResult) { out = res })
	})
	if err != nil {
		return nil, err
	}
	if responseErr != nil {
		return nil, responseErr
	}
	if out == nil {
		return nil, errUnavailable("current zone command returned no result")
	}
	return &importOutput{Body: out}, nil
}
