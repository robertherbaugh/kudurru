package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// ZoneSummary is one zone in GET /zones.
type ZoneSummary struct {
	// Name is the DNSZone object name (the {zone} path segment).
	Name string `json:"name"`
	// ZoneName is the DNS name of the apex (spec.zoneName).
	ZoneName string `json:"zoneName"`
	Type     string `json:"type"`
	// Serial is the SOA serial the leader stamped (status.serial).
	Serial      uint32 `json:"serial"`
	RecordCount int    `json:"recordCount"`
	// RecordSets counts the DNSRecordSets that reference the zone.
	RecordSets int `json:"recordSets"`
	// Ready mirrors the Ready condition; Reason and Message explain it.
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// ManagedBy is the single writer (api, file:<path>, bootstrap,
	// integration:<name>).
	ManagedBy       string `json:"managedBy,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// Upstream is the upstream group of a forward zone.
	Upstream string `json:"upstreamGroup,omitempty"`
	// Fallback resolves missing names through normal routing or a chosen group.
	Fallback *v1alpha1.ZoneFallbackSpec `json:"fallback,omitempty"`
	// DeleteRefusal is the error key DELETE /zones/{zone} answers for this
	// zone whatever the caller's permissions: default_zone for the root
	// zone ".", managed_by when the zone or one of its record sets is
	// managed by a file, an integration, Kubernetes or the system; empty
	// when the zone can be deleted.
	DeleteRefusal string `json:"deleteRefusal,omitempty" enum:"default_zone,managed_by"`
	// SecondaryRuntime is this responding node's state, independent of shared status.
	SecondaryRuntime *SecondaryRuntime `json:"secondaryRuntime,omitempty"`
	// StubRuntime describes only this node's discovered routes.
	StubRuntime *StubRuntime `json:"stubRuntime,omitempty"`
}

// SecondaryRuntime describes the last published copy on one node. Serial and
// RecordCount are absent until a complete copy exists; serial zero is valid.
type SecondaryRuntime struct {
	Node            string     `json:"node"`
	State           string     `json:"state" enum:"pending,serving,expired,unavailable"`
	Serial          *uint32    `json:"serial,omitempty" format:"int64" minimum:"0" maximum:"4294967295"`
	RecordCount     *int       `json:"recordCount,omitempty"`
	Primary         string     `json:"primary,omitempty"`
	LastConfirmedAt *time.Time `json:"lastConfirmedAt,omitempty"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	NextAttemptAt   *time.Time `json:"nextAttemptAt,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	Message         string     `json:"message,omitempty"`
}

func secondaryRuntime(node string, st secondary.Status, found bool) *SecondaryRuntime {
	out := &SecondaryRuntime{Node: node, State: "pending", Reason: "TransferPending", Message: "Waiting for this node's first complete transfer."}
	if !found {
		return out
	}
	out.Primary = st.Primary
	if !st.NextRefresh.IsZero() {
		out.NextAttemptAt = &st.NextRefresh
	}
	if !st.ConfirmedAt.IsZero() {
		out.Serial, out.RecordCount = &st.Serial, &st.RecordCount
		out.LastConfirmedAt, out.ExpiresAt = &st.ConfirmedAt, &st.ExpiresAt
		out.State, out.Reason, out.Message = "expired", "SecondaryExpired", "The last copy has expired; this node answers SERVFAIL for this zone."
		if st.Available {
			out.State, out.Reason, out.Message = "serving", "", ""
		}
	}
	if st.Error != "" {
		if out.State == "pending" {
			out.State = "unavailable"
		}
		out.Reason = "SecondaryTransfer"
		out.Message = st.Error
	}
	if st.ConfigurationError != "" {
		out.State, out.Reason, out.Message = "unavailable", "ConfigurationUnavailable", st.ConfigurationError
	}
	return out
}

func (s *Server) localZoneSummary(z *v1alpha1.DNSZone, recordSets, managedSets int) ZoneSummary {
	out := zoneSummary(z, recordSets, managedSets)
	if z.Spec.Type == v1alpha1.ZoneTypeSecondary {
		var st secondary.Status
		var found bool
		if s.opts.Secondaries != nil {
			st, found = s.opts.Secondaries.StatusFor(z)
		}
		out.SecondaryRuntime = secondaryRuntime(s.opts.NodeName, st, found)
		// Legacy leader-computed serials must not imply a replica is serving.
		out.Serial, out.RecordCount, out.Ready = 0, 0, false
		out.Reason, out.Message = "PerNodeTransfer", "Transfer availability is reported per node in secondaryRuntime."
	}
	if z.Spec.Type == v1alpha1.ZoneTypeStub {
		var st stub.Status
		var found bool
		if s.opts.Stubs != nil {
			st, found = s.opts.Stubs.StatusFor(z)
		}
		out.StubRuntime = stubRuntime(s.opts.NodeName, st, found)
		out.Serial, out.RecordCount, out.Ready = 0, 0, false
		out.Reason, out.Message = "PerNodeDiscovery", "Routing availability is reported per node in stubRuntime."
	}
	return out
}

func capturedZoneError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("zone")
	}
	return errUnavailable("committed zone read: %v", err)
}

func (s *Server) capturedZoneSummary(ctx context.Context, z *v1alpha1.DNSZone, sets []*v1alpha1.DNSRecordSet) (ZoneSummary, error) {
	out := zoneSummary(z, len(sets), len(managedRecordSets(sets)))
	// Desired management/status fields are independent from this node's actual
	// serving readiness and committed scalars, which may advance after UPDATE.
	out.Serial, out.RecordCount, out.Ready = 0, 0, false
	out.Reason, out.Message = "ServingUnavailable", "This node has no available committed copy for this zone."
	if o, found, err := s.opts.ZoneReads.PrimaryStatus(ctx, z.Name); err != nil {
		return ZoneSummary{}, capturedZoneError(err)
	} else if found {
		out.Serial, out.RecordCount = o.Serial, o.RecordCount
	}
	model, err := s.opts.ZoneReads.ReadZone(ctx, z.Name)
	if err != nil && !errors.Is(err, serving.ErrPending) {
		return ZoneSummary{}, capturedZoneError(err)
	}
	available := err == nil && model != nil && model.RoutingAvailableAt(s.opts.Now())
	if available {
		out.Serial, out.RecordCount, out.Ready = model.Serial(), model.RecordCount(), true
		out.Reason, out.Message = "", ""
	}
	switch z.Spec.Type {
	case v1alpha1.ZoneTypeSecondary:
		out.SecondaryRuntime = &SecondaryRuntime{Node: s.opts.NodeName, State: "unavailable", Reason: out.Reason, Message: out.Message}
		if available {
			serial, count, expiry := model.Serial(), model.RecordCount(), model.NextExpiry()
			out.SecondaryRuntime = &SecondaryRuntime{Node: s.opts.NodeName, State: "serving", Serial: &serial, RecordCount: &count, ExpiresAt: &expiry}
		}
	case v1alpha1.ZoneTypeStub:
		out.StubRuntime = &StubRuntime{Node: s.opts.NodeName, State: "unavailable", Authorities: []StubAuthority{}, Reason: out.Reason, Message: out.Message}
		if available {
			out.StubRuntime.State, out.StubRuntime.Reason, out.StubRuntime.Message = "routing", "", ""
			serial, expiry := model.Serial(), model.NextExpiry()
			out.StubRuntime.Serial, out.StubRuntime.ExpiresAt = &serial, &expiry
			for _, server := range model.StubServers() {
				out.StubRuntime.Authorities = append(out.StubRuntime.Authorities, StubAuthority{Name: server.Name, Address: server.Address.String(), Protocol: server.Protocol})
			}
		}
	}
	return out, nil
}

func (s *Server) listCapturedZones(ctx context.Context) (*zoneListOutput, error) {
	objects, err := s.opts.ZoneReads.ZoneObjects(ctx)
	if err != nil {
		return nil, capturedZoneError(err)
	}
	st := stateFrom(ctx)
	items := make([]ZoneSummary, 0, len(objects.Zones()))
	for _, z := range objects.Zones() {
		if !st.clusterAllowed && !s.allowed(ctx, "zones", "read", rbac.Scope{Kind: rbac.ScopeZone, Name: z.Name}) {
			continue
		}
		summary, err := s.capturedZoneSummary(ctx, z, objects.RecordSetsForZone(z.Name))
		if err != nil {
			return nil, err
		}
		items = append(items, summary)
	}
	return &zoneListOutput{Body: newList(items, "")}, nil
}

// ZoneDeleteResult is DELETE /zones/{zone}: what was deleted with the
// zone, or with dryRun what would be.
type ZoneDeleteResult struct {
	// Zone is the DNSZone object name.
	Zone     string `json:"zone"`
	ZoneName string `json:"zoneName"`
	Type     string `json:"type"`
	// DryRun is set when nothing was deleted.
	DryRun bool `json:"dryRun"`
	// RecordSets are the DNSRecordSets that reference the zone
	// ("<namespace>/<name>", any namespace), deleted with it.
	RecordSets     []string `json:"recordSets" nullable:"false"`
	RecordSetCount int      `json:"recordSetCount"`
	// EndpointCount counts the endpoints (records) of those record sets.
	EndpointCount int `json:"endpointCount"`
	// Flushed is how many response-cache entries at or below the apex
	// this node dropped (0 on a dry run; other nodes expire theirs).
	Flushed int `json:"flushed"`
	Took
}

type deleteZoneInput struct {
	Zone    string `path:"zone" doc:"DNSZone object name."`
	DryRun  bool   `query:"dryRun" doc:"Run every check and report what would be deleted, without deleting."`
	IfMatch string `header:"If-Match" doc:"Delete only when the zone's resourceVersion equals this value (412 otherwise)."`
}

type deleteZoneOutput struct {
	Body *ZoneDeleteResult
}

// ZoneDetail is GET /zones/{zone}: the summary plus the object.
type ZoneDetail struct {
	ZoneSummary
	Object Object `json:"object"`
	Took
}

// ZoneRecord is a desired endpoint, or one served RRset in complete mode.
type ZoneRecord struct {
	// RecordSet is "<namespace>/<name>" of the DNSRecordSet holding it, empty
	// when the actual served RRset has no proven mutable desired owner.
	RecordSet     string                `json:"recordSet"`
	ManagedBy     string                `json:"managedBy,omitempty"`
	DNSName       string                `json:"dnsName"`
	RecordType    string                `json:"recordType"`
	Targets       []string              `json:"targets"`
	RecordTTL     int64                 `json:"recordTTL,omitempty"`
	SetIdentifier string                `json:"setIdentifier,omitempty"`
	Labels        map[string]string     `json:"labels,omitempty"`
	Kudurru       *v1alpha1.EndpointExt `json:"kudurru,omitempty"`
}

type zonePathInput struct {
	Zone string `path:"zone" doc:"DNSZone object name."`
}

type zoneListOutput struct {
	Body *List[ZoneSummary]
}

type zoneDetailOutput struct {
	ETag string `header:"ETag"`
	Body *ZoneDetail
}

type zoneRecordsOutput struct {
	Body *List[ZoneRecord]
}

type upsertRecordInput struct {
	Zone string `path:"zone"`
	// RecordSet names the DNSRecordSet to write (default api-<zone>).
	RecordSet string `query:"recordSet" doc:"DNSRecordSet to write; default api-<zone>."`
	Namespace string `query:"namespace" doc:"Namespace of the record set; defaults to the server's default namespace (the release namespace in Kubernetes, or \"default\" for legacy installs and other modes)."`
	Body      v1alpha1.Endpoint
}

type recordOutput struct {
	Status int
	Body   *recordBody
}

type recordBody struct {
	ZoneRecord
	Took
}

type deleteRecordInput struct {
	Zone          string `path:"zone"`
	Name          string `path:"name" doc:"Owner name, relative to the zone or absolute."`
	Type          string `path:"type" doc:"Record type, e.g. A."`
	RecordSet     string `query:"recordSet" doc:"Only this DNSRecordSet (namespace/name or name)."`
	SetIdentifier string `query:"setIdentifier"`
}

type importInput struct {
	Zone   string `path:"zone"`
	Origin string `query:"origin" doc:"Zone apex when the DNSZone does not exist yet (it is created)."`
	// Namespace of the record sets the import writes.
	Namespace string `query:"namespace"`
	RawBody   []byte `contentType:"text/dns"`
}

// ImportResult is the outcome of POST /zones/{zone}/import.
type ImportResult struct {
	Zone       string   `json:"zone"`
	ZoneName   string   `json:"zoneName"`
	Created    bool     `json:"created" doc:"The DNSZone was created by the import."`
	RecordSets []string `json:"recordSets"`
	Endpoints  int      `json:"endpoints"`
	// Serial is the SOA serial found in the file.
	Serial   uint32   `json:"serial"`
	Warnings []string `json:"warnings" nullable:"false"`
	Took
}

type importOutput struct {
	Body *ImportResult
}

type exportOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	Body               []byte
}

// NotifyResult is the outcome of POST /zones/{zone}/notify.
type NotifyResult struct {
	Zone    string   `json:"zone"`
	Targets []string `json:"targets" nullable:"false"`
	Took
}

type notifyOutput struct {
	Status int
	Body   *NotifyResult
}

// Import labels mark the record sets an import wrote so a re-import
// replaces them.
const labelImport = "kudurru.io/import"

func (s *Server) registerZones() {
	zoneRead := perm{resource: "zones", verb: "read", scope: zoneScope("zone")}
	register(s, operation(http.MethodGet, "/zones", "listZones", "List zones with serial, record count and readiness", "zones"),
		perm{resource: "zones", verb: "read", scopable: true}, s.listZones)
	register(s, operation(http.MethodGet, "/zones/{zone}", "getZone", "Get one zone", "zones"), zoneRead, s.getZone)
	deleteOp := operation(http.MethodDelete, "/zones/{zone}", "deleteZone", "Delete a zone with every record set that references it", "zones")
	deleteOp.Description = "Deletes the DNSZone and every DNSRecordSet whose zoneRef names it, in any namespace (D148): in one " +
		"transaction on the sqlite driver; on the kube driver record sets first and the zone last, so a failure keeps the zone " +
		"and the delete can be retried. Needs zones.delete on the zone, and records.delete on it when record sets exist. " +
		"409 default_zone for the root zone \".\"; 409 managed_by (error.objects lists them) when the zone or a record set is " +
		"managed by a file, an integration, Kubernetes or the system. ?dryRun=true runs every check and answers the same " +
		"summary without deleting. Each object delete is audited by the store; the API adds one zones.delete event."
	register(s, deleteOp, perm{resource: "zones", verb: "delete", scope: zoneScope("zone")}, s.deleteZone)
	register(s, operation(http.MethodGet, "/zones/{zone}/records", "listZoneRecords", "List the endpoints of a zone across record sets", "zones"),
		perm{resource: "records", verb: "read", scope: zoneScope("zone")}, s.listZoneRecords)
	register(s, operation(http.MethodPost, "/zones/{zone}/records", "upsertZoneRecord", "Create or replace one endpoint in a record set", "zones"),
		perm{resource: "records", verb: "write", scope: zoneScope("zone")}, s.upsertZoneRecord)
	register(s, withStatus(operation(http.MethodDelete, "/zones/{zone}/records/{name}/{type}", "deleteZoneRecord", "Delete the endpoints of one name and type", "zones"), http.StatusNoContent),
		perm{resource: "records", verb: "delete", scope: zoneScope("zone")}, s.deleteZoneRecord)
	importOp := operation(http.MethodPost, "/zones/{zone}/import", "importZone", "Import a BIND master file (text/dns)", "zones")
	register(s, importOp, perm{resource: "records", verb: "write", scope: zoneScope("zone")}, s.importZone)
	register(s, operation(http.MethodGet, "/zones/{zone}/export", "exportZone", "Export the served zone as a BIND master file (text/dns)", "zones"),
		perm{resource: "zones", verb: "read", scope: zoneScope("zone")}, s.exportZone)
	register(s, withStatus(operation(http.MethodPost, "/zones/{zone}/notify", "notifyZone", "Send NOTIFY for the zone now", "zones"), http.StatusAccepted),
		perm{resource: "zones", verb: "transfer", scope: zoneScope("zone")}, s.notifyZone)
}

// zoneSummary derives the GET /zones row of a zone; managedSets counts
// its record sets the API may not delete.
func zoneSummary(z *v1alpha1.DNSZone, recordSets, managedSets int) ZoneSummary {
	out := ZoneSummary{
		Name: z.Name, ZoneName: z.Spec.ZoneName, Type: string(z.Spec.Type), Serial: z.Status.Serial,
		RecordCount: z.Status.RecordCount, RecordSets: recordSets, ManagedBy: store.ManagedBy(z), ResourceVersion: z.ResourceVersion,
		Fallback: z.Spec.Fallback.DeepCopy(),
	}
	switch {
	case isRootZone(z):
		out.DeleteRefusal = KeyDefaultZone
	case !apiMayDelete(out.ManagedBy) || managedSets > 0:
		out.DeleteRefusal = KeyManagedBy
	}
	if z.Spec.Forward != nil {
		out.Upstream = z.Spec.Forward.UpstreamGroup
	}
	for _, c := range z.Status.Conditions {
		if c.Type == v1alpha1.ConditionReady {
			out.Ready = c.Status == metav1.ConditionTrue
			out.Reason, out.Message = c.Reason, c.Message
		}
	}
	return out
}

// allZones lists every DNSZone from the store.
func (s *Server) allZones(ctx context.Context) ([]*v1alpha1.DNSZone, error) {
	var out []*v1alpha1.DNSZone
	err := s.listAll(ctx, "DNSZone", "", func(o store.Object) {
		if z, ok := o.(*v1alpha1.DNSZone); ok {
			out = append(out, z)
		}
	})
	return out, err
}

// allRecordSets lists every DNSRecordSet from the store.
func (s *Server) allRecordSets(ctx context.Context, namespace string) ([]*v1alpha1.DNSRecordSet, error) {
	var out []*v1alpha1.DNSRecordSet
	err := s.listAll(ctx, "DNSRecordSet", namespace, func(o store.Object) {
		if rs, ok := o.(*v1alpha1.DNSRecordSet); ok {
			out = append(out, rs)
		}
	})
	return out, err
}

// listAll pages through every object of a kind.
func (s *Server) listAll(ctx context.Context, kind store.Kind, namespace string, fn func(store.Object)) error {
	cont := ""
	for {
		page, err := s.opts.Store.List(ctx, kind, store.ListOptions{Namespace: namespace, Limit: 1000, Continue: cont})
		if err != nil {
			return storeError(err)
		}
		for _, o := range page.Items {
			fn(o)
		}
		if page.Continue == "" {
			return nil
		}
		cont = page.Continue
	}
}

func (s *Server) listZones(ctx context.Context, _ *struct{}) (*zoneListOutput, error) {
	if s.opts.ZoneReads != nil {
		return s.listCapturedZones(ctx)
	}
	zones, err := s.allZones(ctx)
	if err != nil {
		return nil, err
	}
	sets, err := s.allRecordSets(ctx, "")
	if err != nil {
		return nil, err
	}
	counts, managed := map[string]int{}, map[string]int{}
	for _, rs := range sets {
		counts[rs.Spec.ZoneRef.Name]++
		if !apiMayDelete(store.ManagedBy(rs)) {
			managed[rs.Spec.ZoneRef.Name]++
		}
	}
	st := stateFrom(ctx)
	items := make([]ZoneSummary, 0, len(zones))
	for _, z := range zones {
		if !st.clusterAllowed && !s.allowed(ctx, "zones", "read", rbac.Scope{Kind: rbac.ScopeZone, Name: z.Name}) {
			continue
		}
		items = append(items, s.localZoneSummary(z, counts[z.Name], managed[z.Name]))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return &zoneListOutput{Body: newList(items, "")}, nil
}

// zoneObject reads the DNSZone named by the path.
func (s *Server) zoneObject(ctx context.Context, name string) (*v1alpha1.DNSZone, error) {
	z := &v1alpha1.DNSZone{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, z); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errNotFound("zone " + name)
		}
		return nil, storeError(err)
	}
	return z, nil
}

// zoneRecordSets returns the record sets that reference zone, sorted.
func (s *Server) zoneRecordSets(ctx context.Context, zoneName string) ([]*v1alpha1.DNSRecordSet, error) {
	all, err := s.allRecordSets(ctx, "")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, rs := range all {
		if rs.Spec.ZoneRef.Name == zoneName {
			out = append(out, rs)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Server) getZone(ctx context.Context, in *zonePathInput) (*zoneDetailOutput, error) {
	if s.opts.ZoneReads != nil {
		objects, err := s.opts.ZoneReads.ZoneObjects(ctx)
		if err != nil {
			return nil, capturedZoneError(err)
		}
		z, found := objects.Zone(in.Zone)
		if !found {
			return nil, errNotFound("zone " + in.Zone)
		}
		summary, err := s.capturedZoneSummary(ctx, z, objects.RecordSetsForZone(z.Name))
		if err != nil {
			return nil, err
		}
		k, _ := kindByPlural("dnszones")
		m, err := objectMap(k, z)
		if err != nil {
			return nil, err
		}
		return &zoneDetailOutput{ETag: z.ResourceVersion, Body: &ZoneDetail{ZoneSummary: summary, Object: m}}, nil
	}
	z, err := s.zoneObject(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil {
		return nil, err
	}
	k, _ := kindByPlural("dnszones")
	m, err := objectMap(k, z)
	if err != nil {
		return nil, err
	}
	return &zoneDetailOutput{ETag: z.ResourceVersion, Body: &ZoneDetail{ZoneSummary: s.localZoneSummary(z, len(sets), len(managedRecordSets(sets))), Object: m}}, nil
}

// isRootZone reports whether z is the root zone ".": the bootstrap
// forwarder every query outside the other zones is routed by.
func isRootZone(z *v1alpha1.DNSZone) bool { return normName(z.Spec.ZoneName) == "" }

// errDefaultZone is the 409 of deleting the root zone (D148).
func errDefaultZone(z *v1alpha1.DNSZone) *Error {
	return newError(http.StatusConflict, KeyDefaultZone,
		fmt.Sprintf("zone %s is the root zone \".\": every query outside the other zones is routed by it (its upstream group and query access apply to every such name), so it cannot be deleted", z.Name),
		"edit it instead: change spec.forward.upstreamGroup, or set spec.policy.queryAccess to deny to stop recursion")
}

// managedRecordSets returns the record sets the API may not delete.
func managedRecordSets(sets []*v1alpha1.DNSRecordSet) []*v1alpha1.DNSRecordSet {
	var out []*v1alpha1.DNSRecordSet
	for _, rs := range sets {
		if !apiMayDelete(store.ManagedBy(rs)) {
			out = append(out, rs)
		}
	}
	return out
}

// maxListedObjects bounds the objects an error message names (all of them
// are in error.objects).
const maxListedObjects = 5

// listedObjects renders the first few objects for a message.
func listedObjects(objs []ObjectOwner, withOwner bool) string {
	parts := make([]string, 0, maxListedObjects)
	for i, o := range objs {
		if i == maxListedObjects {
			parts = append(parts, fmt.Sprintf("and %d more", len(objs)-i))
			break
		}
		key := store.Key{Kind: store.Kind(o.Kind), Namespace: o.Namespace, Name: o.Name}.String()
		if withOwner {
			key += " (" + o.ManagedBy + ")"
		}
		parts = append(parts, key)
	}
	return strings.Join(parts, ", ")
}

func recordSetOwner(rs *v1alpha1.DNSRecordSet) ObjectOwner {
	return ObjectOwner{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name, ManagedBy: store.ManagedBy(rs)}
}

// zoneAloneDeletable guards the object routes' delete of a DNSZone: the
// root zone is refused, and so is a zone that record sets still reference
// (deleting it alone would orphan them; D148).
func (s *Server) zoneAloneDeletable(ctx context.Context, z *v1alpha1.DNSZone) error {
	if isRootZone(z) {
		return errDefaultZone(z)
	}
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil || len(sets) == 0 {
		return err
	}
	objs := make([]ObjectOwner, len(sets))
	for i, rs := range sets {
		objs[i] = recordSetOwner(rs)
	}
	e := newError(http.StatusConflict, KeyZoneNotEmpty,
		fmt.Sprintf("zone %s is referenced by %d record sets (%s); deleting the DNSZone alone would orphan them", z.Name, len(sets), listedObjects(objs, false)),
		"DELETE "+BasePath+"/zones/"+z.Name+" deletes the zone with its record sets (?dryRun=true lists them first)")
	e.Body.Objects = objs
	return e
}

// deleteZone is DELETE /zones/{zone} (D148): the zone and every record
// set that references it, all or nothing where the store has
// transactions, record sets first and the zone last where it does not.
func (s *Server) deleteZone(ctx context.Context, in *deleteZoneInput) (*deleteZoneOutput, error) {
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	z, err := s.zoneObject(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	if in.IfMatch != "" && in.IfMatch != z.ResourceVersion {
		return nil, errPrecondition(in.IfMatch, z.ResourceVersion)
	}
	if isRootZone(z) {
		return nil, errDefaultZone(z)
	}
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil {
		return nil, err
	}
	sc := rbac.Scope{Kind: rbac.ScopeZone, Name: z.Name}
	if len(sets) > 0 {
		if err := s.require(ctx, "records", "delete", sc, z.Name); err != nil {
			return nil, err
		}
	}
	var managed []ObjectOwner
	if owner := store.ManagedBy(z); !apiMayDelete(owner) {
		managed = append(managed, ObjectOwner{Kind: "DNSZone", Name: z.Name, ManagedBy: owner})
	}
	for _, rs := range managedRecordSets(sets) {
		managed = append(managed, recordSetOwner(rs))
	}
	if len(managed) > 0 {
		e := newError(http.StatusConflict, KeyManagedBy,
			fmt.Sprintf("zone %s cannot be deleted: managed by another writer: %s; change or remove them there",
				z.Name, listedObjects(managed, true)),
			managed[0].ManagedBy)
		e.Body.Objects = managed
		return nil, e
	}

	res := &ZoneDeleteResult{Zone: z.Name, ZoneName: z.Spec.ZoneName, Type: string(z.Spec.Type), DryRun: in.DryRun,
		RecordSets: make([]string, 0, len(sets)), RecordSetCount: len(sets)}
	reqs := make([]store.DeleteRequest, 0, len(sets)+1)
	for _, rs := range sets {
		res.RecordSets = append(res.RecordSets, rs.Namespace+"/"+rs.Name)
		res.EndpointCount += len(rs.Spec.Endpoints)
		reqs = append(reqs, store.DeleteRequest{Key: store.Key{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name}, Options: apiDeleteOptions(store.ManagedBy(rs))})
	}
	if in.DryRun {
		return &deleteZoneOutput{Body: res}, nil
	}
	reqs = append(reqs, store.DeleteRequest{Key: store.Key{Kind: "DNSZone", Name: z.Name}, Options: apiDeleteOptions(store.ManagedBy(z))})

	before := map[string]any{"zoneName": z.Spec.ZoneName, "type": z.Spec.Type, "recordSetCount": len(sets), "endpointCount": res.EndpointCount}
	if len(res.RecordSets) <= 50 {
		before["recordSets"] = res.RecordSets
	}
	deleted, atomic, err := store.DeleteAll(ctx, s.opts.Store, reqs)
	if err != nil {
		out := storeError(err)
		var e *Error
		if !atomic && deleted > 0 && errors.As(out, &e) {
			e.Body.Message += fmt.Sprintf(" (%d of %d record sets were deleted before the failure; the zone and the rest were kept: retry the delete)", deleted, len(sets))
			before["deleted"] = deleted
		}
		s.recordAction(ctx, "zones.delete", audit.Resource{Kind: "DNSZone", Name: z.Name}, out, before, nil)
		return nil, out
	}
	res.Flushed = s.flushZone(z.Spec.ZoneName)
	s.recordAction(ctx, "zones.delete", audit.Resource{Kind: "DNSZone", Name: z.Name}, nil, before, nil)
	return &deleteZoneOutput{Body: res}, nil
}

// flushZone drops this node's cached answers at or below apex.
func (s *Server) flushZone(apex string) int {
	if s.opts.ResponseCache == nil {
		return 0
	}
	return s.opts.ResponseCache.FlushZone(apex)
}

func recordOf(rs *v1alpha1.DNSRecordSet, ep v1alpha1.Endpoint) ZoneRecord {
	targets := ep.Targets
	if targets == nil {
		targets = []string{}
	}
	return ZoneRecord{
		RecordSet: rs.Namespace + "/" + rs.Name, ManagedBy: store.ManagedBy(rs),
		DNSName: ep.DNSName, RecordType: ep.RecordType, Targets: targets, RecordTTL: ep.RecordTTL,
		SetIdentifier: ep.SetIdentifier, Labels: ep.Labels, Kudurru: ep.Kudurru,
	}
}

func (s *Server) listZoneRecords(ctx context.Context, in *zonePathInput) (*zoneRecordsOutput, error) {
	if s.opts.ZoneReads != nil {
		model, err := s.opts.ZoneReads.ReadZone(ctx, in.Zone)
		if err != nil {
			return nil, capturedZoneError(err)
		}
		if model == nil || !model.RoutingAvailableAt(s.opts.Now()) {
			return nil, errUnavailable("zone contents are unavailable")
		}
		if model.SOA() == nil {
			return nil, errConflict("zone has no complete record image", "inspect its routing configuration")
		}
		var items []ZoneRecord
		var invalid bool
		model.RRsets(func(set *zone.RRset) bool {
			row := ZoneRecord{DNSName: strings.TrimSuffix(set.Name, "."), RecordType: set.TypeString(), RecordTTL: int64(set.TTL), Targets: make([]string, 0, len(set.RRs))}
			if set.Name == "." {
				row.DNSName = "@"
			}
			for _, rr := range set.RRs {
				parts := strings.SplitN(rr.String(), "\t", 5)
				if len(parts) != 5 {
					invalid = true
					return false
				}
				row.Targets = append(row.Targets, parts[4])
			}
			// An actual RR is not proof of its desired mutable RecordSet owner.
			// Keep unproven references empty; desired objects retain ownership.
			items = append(items, row)
			return ctx.Err() == nil
		})
		if invalid || ctx.Err() != nil {
			return nil, errUnavailable("zone contents could not be read")
		}
		return &zoneRecordsOutput{Body: newList(items, "")}, nil
	}
	if _, err := s.zoneObject(ctx, in.Zone); err != nil {
		return nil, err
	}
	sets, err := s.zoneRecordSets(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	var items []ZoneRecord
	for _, rs := range sets {
		for _, ep := range rs.Spec.Endpoints {
			items = append(items, recordOf(rs, ep))
		}
	}
	return &zoneRecordsOutput{Body: newList(items, "")}, nil
}

// normName lower-cases a DNS name and drops the trailing dot.
func normName(n string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".") }

// sameOwner reports whether endpoint name a denotes name b in zone apex:
// "www", "www.example.com" and "www.example.com." are the same owner, and
// "@" is the apex.
func sameOwner(a, b, apex string) bool {
	return absName(a, apex) == absName(b, apex)
}

func absName(n, apex string) string {
	n, apex = normName(n), normName(apex)
	switch {
	case n == "@" || n == "":
		return apex
	case apex == "" || n == apex || strings.HasSuffix(n, "."+apex):
		return n
	}
	return n + "." + apex
}

// splitRef splits "namespace/name" (or "name" with the default namespace).
func (s *Server) splitRef(ref, defNS string) (string, string) {
	if ns, name, ok := strings.Cut(ref, "/"); ok {
		return ns, name
	}
	if defNS == "" {
		defNS = s.opts.DefaultNamespace
	}
	return defNS, ref
}

func (s *Server) upsertZoneRecord(ctx context.Context, in *upsertRecordInput) (*recordOutput, error) {
	z, err := s.zoneObject(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	if z.Spec.Type != v1alpha1.ZoneTypePrimary {
		return nil, errConflict("only primary zones accept record edits", "edit the records on the zone's primary")
	}
	ep := in.Body
	if ep.DNSName == "" || ep.RecordType == "" {
		return nil, errInvalid("body: dnsName and recordType are required")
	}
	ep.RecordType = strings.ToUpper(ep.RecordType)
	// Endpoint names are absolute (external-dns style, no trailing dot);
	// the API also accepts names relative to the zone and "@".
	ep.DNSName = absName(ep.DNSName, z.Spec.ZoneName)
	if ep.DNSName == "" {
		ep.DNSName = "@"
	}
	name := in.RecordSet
	if name == "" {
		name = "api-" + z.Name
	}
	ns, name := s.splitRef(name, in.Namespace)
	k, _ := kindByPlural("dnsrecordsets")
	// The client sends one endpoint, not a resourceVersion: a write that
	// loses a race (the controller updating the record set's status, a
	// second client) is re-read and re-applied.
	for attempt := 1; ; attempt++ {
		out, err := s.upsertRecordOnce(ctx, z, k, ns, name, ep)
		if lostRace(err) {
			if attempt < patchAttempts {
				continue
			}
			return nil, errRecordRace(ns, name)
		}
		return out, err
	}
}

// errRecordRace is the answer when every attempt lost the race: the client
// sent no If-Match, so a 412 naming one would mislead.
func errRecordRace(ns, name string) *Error {
	what := "the record set"
	if name != "" {
		what = "record set " + ns + "/" + name
	}
	return errConflict(what+" kept changing while the record was written", "try again")
}

// lostRace reports a write refused because the object changed since it was
// read.
func lostRace(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && (apiErr.Status == http.StatusPreconditionFailed || apiErr.Body.Key == KeyConflict)
}

func (s *Server) upsertRecordOnce(ctx context.Context, z *v1alpha1.DNSZone, k kindInfo, ns, name string, ep v1alpha1.Endpoint) (*recordOutput, error) {
	rs := &v1alpha1.DNSRecordSet{}
	created := false
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "DNSRecordSet", Namespace: ns, Name: name}, rs); errors.Is(err, store.ErrNotFound) {
		created = true
		rs = &v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: z.Name}}}
	} else if err != nil {
		return nil, storeError(err)
	} else if rs.Spec.ZoneRef.Name != z.Name {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable,
			fmt.Sprintf("record set %s/%s belongs to zone %s", ns, name, rs.Spec.ZoneRef.Name), "choose another recordSet")
	}
	replaced := false
	for i, cur := range rs.Spec.Endpoints {
		if sameOwner(cur.DNSName, ep.DNSName, z.Spec.ZoneName) && strings.EqualFold(cur.RecordType, ep.RecordType) && cur.SetIdentifier == ep.SetIdentifier {
			rs.Spec.Endpoints[i] = ep
			replaced = true
			break
		}
	}
	if !replaced {
		rs.Spec.Endpoints = append(rs.Spec.Endpoints, ep)
	}
	body, err := typedObject(rs)
	if err != nil {
		return nil, storeError(err)
	}
	req := writeRequest{kind: k, body: body, namespace: ns, name: name}
	if !created {
		req.ifMatch = rs.ResourceVersion
	}
	wr, err := s.writeObject(ctx, req)
	if err != nil {
		return nil, err
	}
	written, _ := wr.obj.(*v1alpha1.DNSRecordSet)
	if written == nil {
		written = rs
	}
	status := http.StatusOK
	if !replaced {
		status = http.StatusCreated
	}
	out := recordOf(written, ep)
	for _, cur := range written.Spec.Endpoints {
		if sameOwner(cur.DNSName, ep.DNSName, z.Spec.ZoneName) && strings.EqualFold(cur.RecordType, ep.RecordType) && cur.SetIdentifier == ep.SetIdentifier {
			out = recordOf(written, cur)
		}
	}
	return &recordOutput{Status: status, Body: &recordBody{ZoneRecord: out}}, nil
}

func (s *Server) deleteZoneRecord(ctx context.Context, in *deleteRecordInput) (*emptyOutput, error) {
	z, err := s.zoneObject(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	// As upsertZoneRecord: a lost race is re-read and re-applied; a record
	// set an earlier attempt already wrote counts as found.
	found := false
	for attempt := 1; ; attempt++ {
		f, err := s.deleteRecordOnce(ctx, z, in)
		found = found || f
		if lostRace(err) {
			if attempt < patchAttempts {
				continue
			}
			return nil, errRecordRace("", "")
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if !found {
		return nil, errNotFound(fmt.Sprintf("record %s %s in zone %s", in.Name, strings.ToUpper(in.Type), z.Name))
	}
	return &emptyOutput{}, nil
}

// deleteRecordOnce removes the endpoints of in from every record set of z
// and reports whether any record set had one.
func (s *Server) deleteRecordOnce(ctx context.Context, z *v1alpha1.DNSZone, in *deleteRecordInput) (bool, error) {
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil {
		return false, err
	}
	var onlyNS, onlyName string
	if in.RecordSet != "" {
		onlyNS, onlyName = s.splitRef(in.RecordSet, "")
	}
	k, _ := kindByPlural("dnsrecordsets")
	found := false
	for _, rs := range sets {
		if onlyName != "" && (rs.Namespace != onlyNS || rs.Name != onlyName) {
			continue
		}
		keep := rs.Spec.Endpoints[:0:0]
		for _, ep := range rs.Spec.Endpoints {
			if sameOwner(ep.DNSName, in.Name, z.Spec.ZoneName) && strings.EqualFold(ep.RecordType, in.Type) &&
				(in.SetIdentifier == "" || ep.SetIdentifier == in.SetIdentifier) {
				continue
			}
			keep = append(keep, ep)
		}
		if len(keep) == len(rs.Spec.Endpoints) {
			continue
		}
		found = true
		if owner := store.ManagedBy(rs); !apiMayDelete(owner) {
			return found, errManagedBy(store.Key{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name}, owner)
		}
		if len(keep) == 0 {
			if err := s.deleteObject(ctx, k, rs, rs.ResourceVersion); err != nil {
				return found, err
			}
			continue
		}
		rs.Spec.Endpoints = keep
		body, err := typedObject(rs)
		if err != nil {
			return found, storeError(err)
		}
		if _, err := s.writeObject(ctx, writeRequest{kind: k, body: body, namespace: rs.Namespace, name: rs.Name, ifMatch: rs.ResourceVersion}); err != nil {
			return found, err
		}
	}
	return found, nil
}

func (s *Server) importZone(ctx context.Context, in *importInput) (*importOutput, error) {
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	z, err := s.zoneObject(ctx, in.Zone)
	var nf *Error
	created := false
	switch {
	case err == nil:
	case errors.As(err, &nf) && nf.Status == http.StatusNotFound && in.Origin != "":
		// Creating a zone needs zones.write at cluster scope (§8.5).
		if err := s.require(ctx, "zones", "write", clusterScope, in.Zone); err != nil {
			return nil, err
		}
		created = true
	default:
		if errors.As(err, &nf) && nf.Status == http.StatusNotFound {
			nf.Body.Hint = "pass ?origin=<zone apex> to create the zone from the file"
		}
		return nil, err
	}
	origin := in.Origin
	if z != nil {
		if z.Spec.Type != v1alpha1.ZoneTypePrimary {
			return nil, errConflict("only primary zones accept record imports", "edit the records on the zone's primary")
		}
		origin = z.Spec.ZoneName
	}
	im, err := zone.ImportBIND(bytes.NewReader(in.RawBody), origin)
	if err != nil {
		return nil, newError(http.StatusBadRequest, KeyInvalid, err.Error(), "the body must be an RFC 1035 master file for "+origin)
	}
	res := &ImportResult{Zone: in.Zone, ZoneName: im.Spec.ZoneName, Created: created, Serial: im.Serial, Warnings: im.Warnings}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	if created {
		dz := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: in.Zone}, Spec: im.Spec}
		body, err := typedObject(dz)
		if err != nil {
			return nil, storeError(err)
		}
		k, _ := kindByPlural("dnszones")
		if _, err := s.writeObject(ctx, writeRequest{kind: k, body: body, name: in.Zone, createOnly: true}); err != nil {
			return nil, err
		}
	}
	ns := in.Namespace
	if ns == "" {
		ns = s.opts.DefaultNamespace
	}
	k, _ := kindByPlural("dnsrecordsets")
	chunks := chunkEndpoints(im.Endpoints, zone.MaxEndpointsPerRecordSet)
	written := map[string]bool{}
	for i, eps := range chunks {
		name := fmt.Sprintf("import-%s-%d", in.Zone, i+1)
		rs := &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{labelImport: in.Zone}},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: in.Zone}, Endpoints: eps},
		}
		body, err := typedObject(rs)
		if err != nil {
			return nil, storeError(err)
		}
		if _, err := s.writeObject(ctx, writeRequest{kind: k, body: body, namespace: ns, name: name}); err != nil {
			return nil, err
		}
		written[name] = true
		res.RecordSets = append(res.RecordSets, ns+"/"+name)
		res.Endpoints += len(eps)
	}
	// A re-import replaces the previous import: drop its leftover chunks.
	old, err := s.opts.Store.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: ns, LabelSelector: map[string]string{labelImport: in.Zone}})
	if err == nil {
		for _, o := range old.Items {
			// Only this zone's own chunks: the label alone is no proof (any
			// writer of another zone can set it).
			if rs, ok := o.(*v1alpha1.DNSRecordSet); !ok || rs.Spec.ZoneRef.Name != in.Zone {
				continue
			}
			if !written[o.GetName()] && store.ManagedBy(o) == store.ManagerAPI {
				_ = s.deleteObject(ctx, k, o, "")
			}
		}
	}
	if res.RecordSets == nil {
		res.RecordSets = []string{}
	}
	return &importOutput{Body: res}, nil
}

func chunkEndpoints(eps []v1alpha1.Endpoint, n int) [][]v1alpha1.Endpoint {
	var out [][]v1alpha1.Endpoint
	for len(eps) > 0 {
		m := min(n, len(eps))
		out = append(out, slices.Clone(eps[:m]))
		eps = eps[m:]
	}
	return out
}

// buildZone compiles the zone the way replicas serve it (the same
// selection the controller hashes) from the store.
func (s *Server) buildZone(ctx context.Context, z *v1alpha1.DNSZone) (*zone.Zone, error) {
	if z.Spec.Type == v1alpha1.ZoneTypeSecondary {
		return nil, errConflict("secondary contents are transferred from their primary", "local secondary export is not available yet")
	}
	if z.Spec.Type == v1alpha1.ZoneTypeStub {
		return nil, errConflict("stub zones hold discovered routes, not a complete zone", "inspect stubRuntime for this node's authorities and expiry")
	}
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil {
		return nil, err
	}
	built, errs := zone.Build(z, zone.ServedRecordSets(z, sets), zone.BuildOptions{Now: s.opts.Now()})
	if built == nil {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, "the zone does not build: "+errs.Zone.ToAggregate().Error(), "")
	}
	return built, nil
}

func (s *Server) exportZone(ctx context.Context, in *zonePathInput) (*exportOutput, error) {
	if s.opts.ZoneReads != nil {
		built, err := s.opts.ZoneReads.ReadZone(ctx, in.Zone)
		if err != nil {
			return nil, capturedZoneError(err)
		}
		if built == nil || !built.RoutingAvailableAt(s.opts.Now()) {
			return nil, errUnavailable("zone contents are unavailable")
		}
		if built.Type() != v1alpha1.ZoneTypePrimary || built.SOA() == nil {
			return nil, errConflict("only complete primary zones have records to export", "inspect the zone's runtime state")
		}
		var buf bytes.Buffer
		if err := zone.ExportBIND(&buf, built); err != nil {
			return nil, errUnavailable("zone export: %v", err)
		}
		if ctx.Err() != nil {
			return nil, errUnavailable("zone export interrupted")
		}
		return &exportOutput{ContentType: "text/dns; charset=utf-8",
			ContentDisposition: `attachment; filename="` + strings.TrimSuffix(built.Name(), ".") + `.zone"`, Body: buf.Bytes()}, nil
	}
	z, err := s.zoneObject(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	built, err := s.buildZone(ctx, z)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := zone.ExportBIND(&buf, built); err != nil {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, err.Error(), "only primary zones have records to export")
	}
	return &exportOutput{
		ContentType:        "text/dns; charset=utf-8",
		ContentDisposition: `attachment; filename="` + strings.TrimSuffix(z.Spec.ZoneName, ".") + `.zone"`,
		Body:               buf.Bytes(),
	}, nil
}

func (s *Server) notifyZone(ctx context.Context, in *zonePathInput) (*notifyOutput, error) {
	z, err := s.zoneObject(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	if z.Spec.Fallback != nil {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable,
			"NOTIFY is unavailable for zones with missing-name fallback",
			"ordinary secondaries cannot reproduce this zone's fallback behavior")
	}
	if z.Spec.Type != v1alpha1.ZoneTypePrimary {
		return nil, errConflict("manual NOTIFY is available for primary zones", "secondary zones refresh independently on every node")
	}
	if s.opts.Notifier == nil {
		return nil, errNotImplemented("NOTIFY")
	}
	targets, err := s.opts.Notifier.NotifyZone(ctx, z.Spec.ZoneName)
	if targets == nil && err == nil {
		if built, berr := s.buildZone(ctx, z); berr == nil {
			for _, t := range built.NotifyTargets() {
				targets = append(targets, t.String())
			}
		}
	}
	s.recordAction(ctx, "zones.transfer", audit.Resource{Kind: "DNSZone", Name: z.Name}, err, nil, map[string]any{"notify": targets})
	if err != nil {
		return nil, errUnavailable("notify: %v", err)
	}
	if targets == nil {
		targets = []string{}
	}
	return &notifyOutput{Status: http.StatusAccepted, Body: &NotifyResult{Zone: z.Name, Targets: targets}}, nil
}

// NotifyTrigger adapts a trigger such as notifier.Notifier.Trigger to
// ZoneNotifier: the leader's notifier compares serials now; the targets
// reported are those of the zone's NOTIFY policy.
type NotifyTrigger func()

// NotifyZone implements ZoneNotifier.
func (t NotifyTrigger) NotifyZone(context.Context, string) ([]string, error) {
	t()
	return nil, nil
}
