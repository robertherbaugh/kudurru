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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
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
}

// ZoneDetail is GET /zones/{zone}: the summary plus the object.
type ZoneDetail struct {
	ZoneSummary
	Object Object `json:"object"`
	Took
}

// ZoneRecord is one endpoint of a zone, flattened across record sets.
type ZoneRecord struct {
	// RecordSet is "<namespace>/<name>" of the DNSRecordSet holding it.
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
	Namespace string `query:"namespace" doc:"Namespace of the record set; default \"default\"."`
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

// zoneSummary derives the GET /zones row of a zone.
func zoneSummary(z *v1alpha1.DNSZone, recordSets int) ZoneSummary {
	out := ZoneSummary{
		Name: z.Name, ZoneName: z.Spec.ZoneName, Type: string(z.Spec.Type), Serial: z.Status.Serial,
		RecordCount: z.Status.RecordCount, RecordSets: recordSets, ManagedBy: store.ManagedBy(z), ResourceVersion: z.ResourceVersion,
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
	zones, err := s.allZones(ctx)
	if err != nil {
		return nil, err
	}
	sets, err := s.allRecordSets(ctx, "")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, rs := range sets {
		counts[rs.Spec.ZoneRef.Name]++
	}
	st := stateFrom(ctx)
	items := make([]ZoneSummary, 0, len(zones))
	for _, z := range zones {
		if !st.clusterAllowed && !s.allowed(ctx, "zones", "read", rbac.Scope{Kind: rbac.ScopeZone, Name: z.Name}) {
			continue
		}
		items = append(items, zoneSummary(z, counts[z.Name]))
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
	return &zoneDetailOutput{ETag: z.ResourceVersion, Body: &ZoneDetail{ZoneSummary: zoneSummary(z, len(sets)), Object: m}}, nil
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
	sets, err := s.zoneRecordSets(ctx, z.Name)
	if err != nil {
		return nil, err
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
		if owner := store.ManagedBy(rs); owner != "" && owner != store.ManagerAPI && owner != store.ManagerBootstrap {
			return nil, errManagedBy(store.Key{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name}, owner)
		}
		if len(keep) == 0 {
			if err := s.deleteObject(ctx, k, rs, rs.ResourceVersion); err != nil {
				return nil, err
			}
			continue
		}
		rs.Spec.Endpoints = keep
		body, err := typedObject(rs)
		if err != nil {
			return nil, storeError(err)
		}
		if _, err := s.writeObject(ctx, writeRequest{kind: k, body: body, namespace: rs.Namespace, name: rs.Name, ifMatch: rs.ResourceVersion}); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, errNotFound(fmt.Sprintf("record %s %s in zone %s", in.Name, strings.ToUpper(in.Type), z.Name))
	}
	return &emptyOutput{}, nil
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
