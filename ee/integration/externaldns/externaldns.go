// Package externaldns serves the external-dns webhook provider contract
// (docs/architecture.md §5.6, §9.6) over the kudurru object store, so
// `external-dns --provider=webhook` turns Ingress, Service and Gateway
// hostnames into kudurru records with no kudurru-side Kubernetes code.
//
// The contract, re-implemented from the external-dns webhook provider
// documentation and its Apache-2.0 sources (sigs.k8s.io/external-dns
// provider/webhook):
//
//   - GET / negotiates: it answers the domain filter
//     ({"include":[...]}, the apexes of the selected primary zones) with
//     Content-Type application/external.dns.webhook+json;version=1;
//   - GET /records lists the current endpoints;
//   - POST /records applies plan.Changes ({Create, UpdateOld, UpdateNew,
//     Delete}) and answers 204 No Content;
//   - POST /adjustendpoints echoes the endpoints (kudurru stores them as
//     given);
//
// with request bodies capped at 32 MiB. external-dns retries 5xx answers
// only, so permanent problems (a name outside every zone, an endpoint the
// validator refuses) are 4xx.
//
// Applied changes become DNSRecordSets in one namespace, one record set
// per DNS name, managed by "integration:external-dns"; the TXT ownership
// records of the external-dns registry are ordinary TXT endpoints. Reads
// come from the zone cache snapshot when it is live (any replica can
// answer) and writes go through Store.Apply, which every replica can
// perform, so the listener runs on every replica.
package externaldns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Wire constants of the external-dns webhook contract.
const (
	// MediaType is the versioned media type of every JSON body.
	MediaType = "application/external.dns.webhook+json;version=1"
	// MaxBodyBytes caps request bodies (32 MiB, as external-dns does).
	MaxBodyBytes = 32 << 20
)

// IntegrationName is the integration the record sets are managed by.
const IntegrationName = "external-dns"

// Manager is the store manager of every record set the webhook writes.
var Manager = store.ManagerIntegration(IntegrationName)

// LabelDNSName records the DNS name a record set holds (sanitised for a
// label value when needed); the record set name may be a hash.
const LabelDNSName = "externaldns.kudurru.io/name"

// ProviderSpecificProperty is one provider-specific endpoint property.
type ProviderSpecificProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Endpoint is the external-dns endpoint wire shape
// (sigs.k8s.io/external-dns/endpoint.Endpoint).
type Endpoint struct {
	DNSName          string                     `json:"dnsName,omitempty"`
	Targets          []string                   `json:"targets,omitempty"`
	RecordType       string                     `json:"recordType,omitempty"`
	SetIdentifier    string                     `json:"setIdentifier,omitempty"`
	RecordTTL        int64                      `json:"recordTTL,omitempty"`
	Labels           map[string]string          `json:"labels,omitempty"`
	ProviderSpecific []ProviderSpecificProperty `json:"providerSpecific,omitempty"`
}

// Changes is the plan.Changes body of POST /records. Releases before
// v0.15 sent the Go field names (Create, UpdateOld, ...), later ones the
// tagged names (create, updateOld, ...); encoding/json matches both.
type Changes struct {
	Create    []*Endpoint `json:"create,omitempty"`
	UpdateOld []*Endpoint `json:"updateOld,omitempty"`
	UpdateNew []*Endpoint `json:"updateNew,omitempty"`
	Delete    []*Endpoint `json:"delete,omitempty"`
}

// DomainFilter is the GET / body (endpoint.DomainFilter's JSON form).
type DomainFilter struct {
	Include      []string `json:"include,omitempty"`
	Exclude      []string `json:"exclude,omitempty"`
	RegexInclude string   `json:"regexInclude,omitempty"`
	RegexExclude string   `json:"regexExclude,omitempty"`
}

// Options configures a Server.
type Options struct {
	// Store receives the record sets; Validator checks them before every
	// write. Both are required.
	Store     store.Store
	Validator *validator.Validator
	// Cache serves reads while it is live; nil reads the store.
	Cache *zonecache.Cache
	// Namespace receives the DNSRecordSets (externalDNS.webhook.namespace).
	Namespace string
	// ZoneSelector chooses the primary DNSZones external-dns may write to
	// by label (externalDNS.webhook.zoneSelector); empty selects all.
	ZoneSelector map[string]string
	// Address is the listener address of Run (externalDNS.webhook.listen).
	Address string
	// Registerer receives the kudurru_externaldns_* metrics; nil skips.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
}

// OptionsFrom maps the externalDNS.webhook settings onto Options; the
// caller adds Store, Validator, Cache, Registerer and Logger.
func OptionsFrom(c config.ExternalDNSWebhook) Options {
	return Options{Namespace: c.Namespace, ZoneSelector: c.ZoneSelector, Address: c.Listen}
}

// Server is the webhook provider. It is an http.Handler.
type Server struct {
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux

	requests *prometheus.CounterVec
	changes  *prometheus.CounterVec

	// mu serialises ApplyChanges in this process; replicas are kept
	// consistent by the store's optimistic concurrency.
	mu sync.Mutex
}

// New builds the webhook server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil || opts.Validator == nil {
		return nil, errors.New("externaldns: Store and Validator are required")
	}
	if opts.Namespace == "" {
		return nil, errors.New("externaldns: Namespace is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{
		opts: opts,
		log:  opts.Logger.With("component", "externaldns"),
		mux:  http.NewServeMux(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_externaldns_requests_total", Help: "external-dns webhook requests by route and status code.",
		}, []string{"route", "code"}),
		changes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_externaldns_changes_total", Help: "external-dns endpoint changes applied, by action.",
		}, []string{"action"}),
	}
	if opts.Registerer != nil {
		opts.Registerer.MustRegister(s.requests, s.changes)
	}
	s.mux.HandleFunc("GET /{$}", s.instrument("negotiate", s.negotiate))
	s.mux.HandleFunc("GET /records", s.instrument("records", s.getRecords))
	s.mux.HandleFunc("POST /records", s.instrument("apply", s.postRecords))
	s.mux.HandleFunc("POST /adjustendpoints", s.instrument("adjustendpoints", s.adjustEndpoints))
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Run serves on Options.Address until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Address)
	if err != nil {
		return fmt.Errorf("externaldns: listen %s: %w", s.opts.Address, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.log.Info("external-dns webhook listening", "address", ln.Addr().String(), "namespace", s.opts.Namespace)
	if ap, err := netip.ParseAddrPort(ln.Addr().String()); err == nil && !ap.Addr().Unmap().IsLoopback() {
		s.log.Warn("the external-dns webhook has no authentication and listens beyond loopback: restrict who reaches it (NetworkPolicy, firewall)",
			"address", ln.Addr().String())
	}
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(sctx)
		<-errCh
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *Server) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusRecorder{ResponseWriter: w}
		h(sw, r)
		code := sw.code
		if code == 0 {
			code = http.StatusOK
		}
		s.requests.WithLabelValues(route, strconv.Itoa(code)).Inc()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", MediaType)
	w.Header().Set("Vary", "Content-Type")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// httpError answers a plain-text error.
func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}

// statusError carries the HTTP status of an apply failure.
type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func badRequest(format string, args ...any) error {
	return &statusError{code: http.StatusBadRequest, err: fmt.Errorf(format, args...)}
}

func statusOf(err error) int {
	var se *statusError
	switch {
	case errors.As(err, &se):
		return se.code
	case store.IsManagedBy(err), validator.IsInvalid(err):
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func (s *Server) negotiate(w http.ResponseWriter, r *http.Request) {
	df, err := s.DomainFilter(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, df)
}

func (s *Server) getRecords(w http.ResponseWriter, r *http.Request) {
	eps, err := s.Records(r.Context())
	if err != nil {
		httpError(w, statusOf(err), err.Error())
		return
	}
	writeJSON(w, eps)
}

func (s *Server) postRecords(w http.ResponseWriter, r *http.Request) {
	var ch Changes
	if err := decodeBody(w, r, &ch); err != nil {
		httpError(w, statusOf(err), err.Error())
		return
	}
	if err := s.ApplyChanges(r.Context(), &ch); err != nil {
		s.log.Warn("external-dns changes refused", "err", err)
		httpError(w, statusOf(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adjustEndpoints(w http.ResponseWriter, r *http.Request) {
	var eps []*Endpoint
	if err := decodeBody(w, r, &eps); err != nil {
		httpError(w, statusOf(err), err.Error())
		return
	}
	if eps == nil {
		eps = []*Endpoint{}
	}
	writeJSON(w, eps)
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	body := http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &statusError{code: http.StatusRequestEntityTooLarge, err: fmt.Errorf("request body exceeds %d bytes", MaxBodyBytes)}
		}
		return badRequest("decode body: %v", err)
	}
	return nil
}

// normName lower-cases a DNS name and drops the trailing dot.
func normName(n string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".") }

// snapshot returns the live object set of the zone cache, if any.
func (s *Server) snapshot() (zonecache.ObjectSet, bool) {
	if s.opts.Cache == nil {
		return zonecache.ObjectSet{}, false
	}
	snap := s.opts.Cache.Snapshot()
	if snap == nil || snap.State != zonecache.Live {
		return zonecache.ObjectSet{}, false
	}
	return snap.Objects, true
}

// zones returns the primary zones external-dns may write to.
func (s *Server) zones(ctx context.Context) ([]*v1alpha1.DNSZone, error) {
	var all []*v1alpha1.DNSZone
	if objs, ok := s.snapshot(); ok {
		all = objs.Zones()
	} else {
		cont := ""
		for {
			page, err := s.opts.Store.List(ctx, "DNSZone", store.ListOptions{Limit: 1000, Continue: cont})
			if err != nil {
				return nil, err
			}
			for _, o := range page.Items {
				if z, ok := o.(*v1alpha1.DNSZone); ok {
					all = append(all, z)
				}
			}
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	out := all[:0:0]
	for _, z := range all {
		if z.Spec.Type != v1alpha1.ZoneTypePrimary {
			continue
		}
		if !matchLabels(z.Labels, s.opts.ZoneSelector) {
			continue
		}
		out = append(out, z)
	}
	return out, nil
}

func matchLabels(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// DomainFilter returns the apexes of the selected zones.
func (s *Server) DomainFilter(ctx context.Context) (DomainFilter, error) {
	zones, err := s.zones(ctx)
	if err != nil {
		return DomainFilter{}, err
	}
	seen := map[string]bool{}
	df := DomainFilter{Include: []string{}}
	for _, z := range zones {
		n := normName(z.Spec.ZoneName)
		if n != "" && !seen[n] {
			seen[n] = true
			df.Include = append(df.Include, n)
		}
	}
	sort.Strings(df.Include)
	return df, nil
}

// managedSets lists the record sets the webhook owns. fromStore forces a
// store read (writes must see the latest resourceVersions).
func (s *Server) managedSets(ctx context.Context, fromStore bool) ([]*v1alpha1.DNSRecordSet, error) {
	var items []store.Object
	if objs, ok := s.snapshot(); ok && !fromStore {
		for _, o := range objs.List("DNSRecordSet") {
			if o.GetNamespace() == s.opts.Namespace {
				items = append(items, o)
			}
		}
	} else {
		cont := ""
		for {
			page, err := s.opts.Store.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: s.opts.Namespace, Limit: 1000, Continue: cont})
			if err != nil {
				return nil, err
			}
			items = append(items, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	var out []*v1alpha1.DNSRecordSet
	for _, o := range items {
		rs, ok := o.(*v1alpha1.DNSRecordSet)
		if ok && store.ManagedBy(rs) == Manager {
			out = append(out, rs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Records lists the endpoints of the managed record sets, ownership TXT
// records included.
func (s *Server) Records(ctx context.Context) ([]*Endpoint, error) {
	sets, err := s.managedSets(ctx, false)
	if err != nil {
		return nil, err
	}
	out := []*Endpoint{}
	for _, rs := range sets {
		for _, e := range rs.Spec.Endpoints {
			out = append(out, fromEndpoint(e))
		}
	}
	return out, nil
}

func fromEndpoint(e v1alpha1.Endpoint) *Endpoint {
	out := &Endpoint{
		DNSName: e.DNSName, Targets: slices.Clone(e.Targets), RecordType: e.RecordType, SetIdentifier: e.SetIdentifier,
		RecordTTL: e.RecordTTL, Labels: maps.Clone(e.Labels),
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	for _, p := range e.ProviderSpecific {
		out.ProviderSpecific = append(out.ProviderSpecific, ProviderSpecificProperty{Name: p.Name, Value: p.Value})
	}
	return out
}

func toEndpoint(e *Endpoint) v1alpha1.Endpoint {
	out := v1alpha1.Endpoint{
		DNSName: normName(e.DNSName), Targets: slices.Clone(e.Targets), RecordType: strings.ToUpper(e.RecordType),
		SetIdentifier: e.SetIdentifier, RecordTTL: e.RecordTTL,
	}
	if len(e.Labels) > 0 {
		out.Labels = maps.Clone(e.Labels)
	}
	for _, p := range e.ProviderSpecific {
		out.ProviderSpecific = append(out.ProviderSpecific, v1alpha1.ProviderSpecific{Name: p.Name, Value: p.Value})
	}
	return out
}

// sameIdentity is the external-dns endpoint key: name, type, set id.
func sameIdentity(a v1alpha1.Endpoint, name, typ, setID string) bool {
	return normName(a.DNSName) == name && strings.EqualFold(a.RecordType, typ) && a.SetIdentifier == setID
}

// ApplyChanges applies a plan: deletions and the old side of updates are
// removed first, then creations and the new side of updates are added
// (replacing an endpoint with the same name, type and set identifier).
// Each touched DNS name is written as one record set; a record set left
// without endpoints is deleted. A concurrent writer (another replica)
// makes the store refuse the stale write; the plan is then re-applied on
// fresh state, up to three times.
func (s *Server) ApplyChanges(ctx context.Context, ch *Changes) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for range 3 {
		if err = s.apply(ctx, ch); !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return err
}

func (s *Server) apply(ctx context.Context, ch *Changes) error {
	for _, list := range [][]*Endpoint{ch.Create, ch.UpdateOld, ch.UpdateNew, ch.Delete} {
		for _, e := range list {
			if e == nil || normName(e.DNSName) == "" || e.RecordType == "" {
				return badRequest("every endpoint needs dnsName and recordType")
			}
		}
	}
	sets, err := s.managedSets(ctx, true)
	if err != nil {
		return err
	}
	byName := map[string]*v1alpha1.DNSRecordSet{}
	for _, rs := range sets {
		for _, e := range rs.Spec.Endpoints {
			byName[normName(e.DNSName)] = rs
		}
	}
	desired := map[string][]v1alpha1.Endpoint{}
	touch := func(name string) {
		if _, ok := desired[name]; ok {
			return
		}
		var eps []v1alpha1.Endpoint
		if rs := byName[name]; rs != nil {
			for _, e := range rs.Spec.Endpoints {
				if normName(e.DNSName) == name {
					eps = append(eps, e)
				}
			}
		}
		desired[name] = eps
	}
	remove := func(e *Endpoint) {
		name := normName(e.DNSName)
		touch(name)
		desired[name] = slices.DeleteFunc(desired[name], func(x v1alpha1.Endpoint) bool {
			return sameIdentity(x, name, e.RecordType, e.SetIdentifier)
		})
	}
	upsert := func(e *Endpoint) {
		name := normName(e.DNSName)
		touch(name)
		n := toEndpoint(e)
		eps := desired[name]
		if i := slices.IndexFunc(eps, func(x v1alpha1.Endpoint) bool { return sameIdentity(x, name, e.RecordType, e.SetIdentifier) }); i >= 0 {
			eps[i] = n
		} else {
			eps = append(eps, n)
		}
		desired[name] = eps
	}
	for _, e := range ch.Delete {
		remove(e)
	}
	for _, e := range ch.UpdateOld {
		remove(e)
	}
	for _, e := range ch.Create {
		upsert(e)
	}
	for _, e := range ch.UpdateNew {
		upsert(e)
	}

	zones, err := s.zones(ctx)
	if err != nil {
		return err
	}
	// Names already stored are written back into their record set (once
	// per set, keeping names of that set the plan did not touch); new
	// names get a record set each.
	perSet := map[*v1alpha1.DNSRecordSet][]string{}
	var fresh []string
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		if rs := byName[name]; rs != nil {
			perSet[rs] = append(perSet[rs], name)
		} else {
			fresh = append(fresh, name)
		}
	}
	for _, rs := range sets {
		names, ok := perSet[rs]
		if !ok {
			continue
		}
		var eps []v1alpha1.Endpoint
		for _, e := range rs.Spec.Endpoints {
			if !slices.Contains(names, normName(e.DNSName)) {
				eps = append(eps, e)
			}
		}
		for _, name := range names {
			eps = append(eps, desired[name]...)
		}
		if err := s.writeSet(ctx, rs, "", eps, zones); err != nil {
			return err
		}
	}
	for _, name := range fresh {
		if err := s.writeSet(ctx, nil, name, desired[name], zones); err != nil {
			return err
		}
	}
	s.changes.WithLabelValues("create").Add(float64(len(ch.Create)))
	s.changes.WithLabelValues("update").Add(float64(len(ch.UpdateNew)))
	s.changes.WithLabelValues("delete").Add(float64(len(ch.Delete)))
	return nil
}

// zoneFor returns the zone with the longest apex that contains name.
func zoneFor(name string, zones []*v1alpha1.DNSZone) *v1alpha1.DNSZone {
	var best *v1alpha1.DNSZone
	bestLen := -1
	for _, z := range zones {
		apex := normName(z.Spec.ZoneName)
		if (name == apex || strings.HasSuffix(name, "."+apex) || apex == "") && len(apex) > bestLen {
			best, bestLen = z, len(apex)
		}
	}
	return best
}

// writeSet stores eps in the record set cur, or for a new DNS name in a
// new record set; an empty eps deletes cur.
func (s *Server) writeSet(ctx context.Context, cur *v1alpha1.DNSRecordSet, name string, eps []v1alpha1.Endpoint, zones []*v1alpha1.DNSZone) error {
	if len(eps) == 0 {
		if cur == nil {
			return nil
		}
		err := s.opts.Store.Delete(ctx, store.KeyOf(cur), store.DeleteOptions{Manager: Manager})
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrConflict
		}
		return err
	}
	var rs *v1alpha1.DNSRecordSet
	if cur != nil {
		rs = cur.DeepCopy()
		rs.Spec.Endpoints = eps
	} else {
		z := zoneFor(name, zones)
		if z == nil {
			return badRequest("%s is not inside a primary zone external-dns may write to", name)
		}
		rs = &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: RecordSetName(name), Namespace: s.opts.Namespace, Labels: map[string]string{LabelDNSName: labelValue(name)}},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: z.Name}, Endpoints: eps},
		}
	}
	gvk, _, _ := s.opts.Validator.GroupVersionKind("DNSRecordSet")
	rs.GetObjectKind().SetGroupVersionKind(gvk)
	var old store.Object
	if cur != nil {
		old = cur
	} else {
		// A record set of that name may exist (another replica, or one
		// external-dns no longer owns): never overwrite it blindly.
		existing := &v1alpha1.DNSRecordSet{}
		if err := s.opts.Store.Get(ctx, store.KeyOf(rs), existing); err == nil {
			if owner := store.ManagedBy(existing); owner != Manager {
				return &store.ErrManagedBy{Key: store.KeyOf(rs), Owner: owner}
			}
			return store.ErrConflict
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if err := s.opts.Validator.Validate(ctx, rs, old); err != nil {
		return &statusError{code: http.StatusUnprocessableEntity, err: err}
	}
	opts := store.ApplyOptions{Manager: Manager}
	if cur != nil {
		opts.ExpectedResourceVersion = cur.ResourceVersion
	}
	_, err := s.opts.Store.Apply(ctx, rs, opts)
	return err
}

// RecordSetName is the DNSRecordSet name for a DNS name: "edns-" plus the
// name when it is a valid object name, otherwise a sanitised prefix plus
// a hash ("edns-wildcard.apps.example.com-3f2a9c1d" for *.apps...).
func RecordSetName(dnsName string) string {
	n := normName(dnsName)
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		case r == '*':
			b.WriteString("wildcard")
		default:
			b.WriteByte('-')
		}
	}
	clean := strings.Trim(b.String(), "-.")
	name := "edns-" + clean
	if clean != n || len(name) > 253 || strings.Contains(clean, "..") {
		sum := sha256.Sum256([]byte(n))
		suffix := "-" + hex.EncodeToString(sum[:4])
		if len(name) > 253-len(suffix) {
			name = strings.TrimRight(name[:253-len(suffix)], "-.")
		}
		name += suffix
	}
	return name
}

// labelValue fits a DNS name into a label value (63 characters, alnum
// ends); longer names keep a hashed tail.
func labelValue(n string) string {
	v := strings.NewReplacer("*", "wildcard").Replace(n)
	var b strings.Builder
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	v = b.String()
	if len(v) > 63 {
		sum := sha256.Sum256([]byte(n))
		v = v[:54] + "-" + hex.EncodeToString(sum[:4])
	}
	return strings.Trim(v, "-._")
}
