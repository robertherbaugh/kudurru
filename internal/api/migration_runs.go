package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const migrationRunAnnotation = "kudurru.io/migration-run"

// Journals are Kubernetes Secret data on the kube driver, whose decoded data
// limit is smaller than the general store/etcd object cap. Use it on all drivers
// so previews and complete outcomes fit either deployment.
const migrationJournalMaxBytes = 1 << 20

// MigrationRun is the administrator-facing view of the internal journal.
type MigrationRun struct {
	ID       string                    `json:"id"`
	Revision string                    `json:"revision"`
	Data     internal.MigrationRunSpec `json:"data"`
}

func migrationView(r *internal.MigrationRun) MigrationRun {
	return MigrationRun{ID: r.Name, Revision: r.ResourceVersion, Data: r.DeepCopy().Spec}
}

func (s *Server) migrationRunView(r *internal.MigrationRun) MigrationRun {
	view := migrationView(r)
	if r.Spec.Status == "running" && r.Spec.Deadline != nil && s.opts.Now().After(r.Spec.Deadline.Add(15*time.Second)) {
		view.Data.Status = "interrupted"
		for i := range view.Data.Items {
			if view.Data.Items[i].Result == "pending" || view.Data.Items[i].Result == "running" {
				view.Data.Items[i].Result = "unconfirmed"
				view.Data.Items[i].Hint = "The worker stopped reporting. Check the destination before retrying."
			}
		}
		view.Data.Failure = "The import worker stopped reporting before completion. Successful writes remain; pending results are unconfirmed. Review the destination before starting another import."
	}
	return view
}

type migrationRunInput struct {
	ID string `path:"id"`
}
type migrationRunOutput struct{ Body MigrationRun }
type migrationRunsInput struct {
	Limit    int    `query:"limit" minimum:"0" maximum:"100"`
	Continue string `query:"continue"`
}
type migrationRunsOutput struct{ Body *List[MigrationRun] }

// MigrationAuthorityChange is one reviewed change, fenced by object version.
type MigrationAuthorityChange struct {
	// UID pins a reviewed previous import; cutover callers may omit it.
	UID               string                 `json:"uid,omitempty"`
	Name              string                 `json:"name"`
	Namespace         string                 `json:"namespace,omitempty"`
	ZoneName          string                 `json:"zoneName"`
	ResourceVersion   string                 `json:"resourceVersion"`
	Nameservers       []string               `json:"nameservers" nullable:"false"`
	Primary           string                 `json:"primary"`
	Error             string                 `json:"error,omitempty"`
	TargetNameservers []string               `json:"targetNameservers,omitempty"`
	AddressRecordSet  *v1alpha1.DNSRecordSet `json:"addressRecordSet,omitempty"`
}

// MigrationCutoverPlan can only change authority of zones recorded in a run.
type MigrationCutoverPlan struct {
	Revision    string                     `json:"revision" minLength:"1"`
	Nameservers []string                   `json:"nameservers" nullable:"false" maxItems:"16"`
	Addresses   []string                   `json:"addresses,omitempty" maxItems:"16"`
	Zones       []MigrationAuthorityChange `json:"zones" nullable:"false"`
	Warnings    []string                   `json:"warnings" nullable:"false"`
	// Request-local snapshot, reloaded at apply. Never accept it from the client.
	recordSets map[string][]*v1alpha1.DNSRecordSet
}
type migrationCutoverPreviewInput struct {
	ID   string `path:"id"`
	Body struct {
		Nameservers []string `json:"nameservers" nullable:"false" maxItems:"16"`
		Addresses   []string `json:"addresses,omitempty" maxItems:"16"`
	}
}
type migrationCutoverInput struct {
	ID   string `path:"id"`
	Body MigrationCutoverPlan
}
type migrationCutoverOutput struct{ Body *MigrationCutoverPlan }
type migrationCutoverResultOutput struct{ Body *MigrationResult }

func (s *Server) registerMigrationRuns() {
	s.registerMigrationAdoption()
	register(s, operation(http.MethodGet, "/imports/runs", "listMigrationRuns", "List migration runs", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *migrationRunsInput) (*migrationRunsOutput, error) {
		limit := in.Limit
		if limit == 0 {
			limit = 25
		}
		reader, err := s.migrationReader(ctx)
		if err != nil {
			return nil, err
		}
		list, err := reader.List(ctx, internal.KindMigrationRun, store.ListOptions{Limit: limit, Continue: in.Continue})
		if err != nil {
			return nil, storeError(err)
		}
		items := make([]MigrationRun, 0, len(list.Items))
		for _, obj := range list.Items {
			items = append(items, s.migrationRunView(obj.(*internal.MigrationRun)))
		}
		return &migrationRunsOutput{Body: newList(items, list.Continue)}, nil
	})
	register(s, operation(http.MethodGet, "/imports/runs/{id}", "getMigrationRun", "Read a migration run and its cutovers", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *migrationRunInput) (*migrationRunOutput, error) {
		run, err := s.getMigrationRun(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &migrationRunOutput{Body: s.migrationRunView(run)}, nil
	})
	register(s, operation(http.MethodPost, "/imports/runs/{id}/decommission/preview", "previewMigrationDecommission", "Preview replacing an imported zone's NS and SOA primary", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *migrationCutoverPreviewInput) (*migrationCutoverOutput, error) {
		run, err := s.getMigrationRun(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		plan, err := s.previewMigrationCutover(ctx, run, in.Body.Nameservers, in.Body.Addresses)
		if err != nil {
			return nil, err
		}
		return &migrationCutoverOutput{Body: plan}, nil
	})
	register(s, operation(http.MethodPost, "/imports/runs/{id}/decommission", "decommissionMigration", "Replace imported zone authority and record the results", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *migrationCutoverInput) (*migrationCutoverResultOutput, error) {
		result, err := s.applyMigrationCutover(ctx, in.ID, &in.Body)
		if err != nil {
			return nil, err
		}
		return &migrationCutoverResultOutput{Body: result}, nil
	})
}

func (s *Server) getMigrationRun(ctx context.Context, id string) (*internal.MigrationRun, error) {
	reader, err := s.migrationReader(ctx)
	if err != nil {
		return nil, err
	}
	r := &internal.MigrationRun{}
	if err := reader.Get(ctx, store.Key{Kind: internal.KindMigrationRun, Name: id}, r); err != nil {
		return nil, storeError(err)
	}
	return r, nil
}
func (s *Server) saveMigrationRun(ctx context.Context, r *internal.MigrationRun) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > migrationJournalMaxBytes {
		return store.ErrTooLarge
	}
	out, err := s.opts.Store.Apply(ctx, r, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: r.ResourceVersion})
	if err != nil {
		return err
	}
	*r = *out.(*internal.MigrationRun)
	return nil
}
func (s *Server) newMigrationRun(ctx context.Context, plan *importers.Plan, source string) (*internal.MigrationRun, error) {
	r, err := s.buildMigrationRun(ctx, plan, source)
	if err != nil {
		return nil, err
	}
	if err := s.saveMigrationRun(ctx, r); err != nil {
		return nil, storeError(err)
	}
	return r, nil
}

func (s *Server) buildMigrationRun(ctx context.Context, plan *importers.Plan, source string) (*internal.MigrationRun, error) {
	selected, settings, err := plan.Selected()
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	// Reverse timestamp order makes paginated store listings newest first.
	if source == "" {
		source = "manual"
	}
	r := &internal.MigrationRun{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("migration-%016x-%s", 1<<63-1-s.opts.Now().UnixMilli(), uuid.NewString())}, Spec: internal.MigrationRunSpec{
		Source: source, Actor: s.actor(ctx), StartedAt: metav1.NewTime(s.opts.Now()), Status: "running", Items: []internal.MigrationItem{}, Zones: []internal.MigrationZone{}, Warnings: append([]string{}, plan.Warnings...), Cutovers: []internal.MigrationCutover{},
	}}
	deadline := metav1.NewTime(s.opts.Now().Add(migrationTimeout))
	r.Spec.Deadline = &deadline
	if plan.Source != nil {
		r.Spec.Source = plan.Source.Type
		switch plan.Source.Type {
		case "adguard", "blocky":
			if plan.Source.URL != "" {
				return nil, errInvalid("offline migration source must not include a URL or file path")
			}
		case "pihole", "technitium":
			// Persist only the origin. Paths and queries may contain credentials.
			u, err := url.Parse(plan.Source.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, errInvalid("invalid source URL")
			}
			r.Spec.SourceURL = (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		default:
			return nil, errInvalid("unknown migration source")
		}
	}
	seen := map[string]bool{}
	for _, doc := range plan.Objects {
		kind, _ := doc["kind"].(string)
		name, _ := nestedString(doc, "metadata", "name")
		ns, _ := nestedString(doc, "metadata", "namespace")
		if k, ok := kindByKind(store.Kind(kind)); ok && k.namespaced && ns == "" {
			ns = s.opts.DefaultNamespace
		}
		key := kind + "/" + ns + "/" + name
		if seen[key] {
			return nil, errInvalid("duplicate migration object %s", key)
		}
		seen[key] = true
		result := "pending"
		if !selected[importers.ObjectID(doc)] {
			result = "skipped"
		}
		r.Spec.Items = append(r.Spec.Items, internal.MigrationItem{Kind: kind, Namespace: ns, Name: name, Result: result})
		if kind != "DNSZone" || result == "skipped" {
			continue
		}
		var z v1alpha1.DNSZone
		b, _ := json.Marshal(doc)
		if err := json.Unmarshal(b, &z); err != nil {
			return nil, errInvalid("invalid migration zone")
		}
		if z.Spec.Type != v1alpha1.ZoneTypePrimary {
			continue
		}
		tracked := internal.MigrationZone{Name: name, Namespace: ns, ZoneName: z.Spec.ZoneName, Nameservers: append([]string{}, z.Spec.Nameservers...), Primary: zonePrimary(&z), OriginalNameservers: append([]string{}, z.Spec.Nameservers...), OriginalPrimary: zonePrimary(&z)}
		for _, original := range plan.Zones {
			if original.Name == name {
				tracked.OriginalNameservers = append([]string{}, original.Nameservers...)
				tracked.OriginalPrimary = original.Primary
				break
			}
		}
		r.Spec.Zones = append(r.Spec.Zones, tracked)
	}
	keys := make([]string, 0, len(plan.Settings))
	for key := range plan.Settings {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		result := "pending"
		if _, ok := settings[key]; !ok {
			result = "skipped"
		}
		r.Spec.Items = append(r.Spec.Items, internal.MigrationItem{Kind: "Settings", Name: key, Result: result})
	}
	if err := migrationImportCapacity(r); err != nil {
		return nil, err
	}
	return r, nil
}
func migrationItem(r ApplyResult) internal.MigrationItem {
	return internal.MigrationItem{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Result: r.Result, Error: truncateMigrationError(r.Error), ErrorKey: r.ErrorKey, Hint: truncateMigrationError(r.Hint), ResourceVersion: r.ResourceVersion}
}
func truncateMigrationError(s string) string {
	if len(s) <= 1024 {
		if b, _ := json.Marshal(s); len(b) <= 1024 {
			return s
		}
	}
	// Bound encoded bytes too, preserving valid Unicode and room for the suffix.
	runes := []rune(s[:min(len(s), 1024)])
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		b, _ := json.Marshal(string(runes[:mid]) + "…")
		if len(b) <= 1024 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + "…"
}

func reserveMigrationItem(item *internal.MigrationItem) {
	item.Result = "not_applied"
	item.Error = strings.Repeat("x", 1024)
	item.Hint = strings.Repeat("x", 1024)
	item.ErrorKey = strings.Repeat("x", 64)
	item.ResourceVersion = strings.Repeat("x", 64)
}

// Every selected item must fit even when every write returns an error.
func migrationImportCapacity(r *internal.MigrationRun) error {
	projected := r.DeepCopy()
	for i := range projected.Spec.Items {
		if projected.Spec.Items[i].Result != "skipped" {
			reserveMigrationItem(&projected.Spec.Items[i])
		}
	}
	for i := range projected.Spec.Zones {
		if projected.Spec.Zones[i].UID == "" {
			projected.Spec.Zones[i].UID = strings.Repeat("x", 64)
		}
	}
	b, err := json.Marshal(projected)
	if err != nil {
		return err
	}
	if len(b)+8192 > migrationJournalMaxBytes {
		return errInvalid("This selection cannot fit a complete migration report; select fewer objects per import")
	}
	return nil
}

// persistMigrationProgress stops further writes if the journal cannot keep up.
func (s *Server) persistMigrationProgress(ctx context.Context, r *internal.MigrationRun, result *MigrationResult) bool {
	if err := s.saveMigrationRun(ctx, r); err != nil {
		s.log.Error("migration progress could not be saved", "run", r.Name, "error", err)
		result.JournalError = "Migration journal could not be saved; further writes stopped. Read run " + r.Name + " before retrying."
		return false
	}
	return true
}
func (s *Server) finishMigrationRun(ctx context.Context, r *internal.MigrationRun, result *MigrationResult) {
	// A disconnected browser must not prevent recording completed destination writes.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := metav1.NewTime(s.opts.Now())
	r.Spec.FinishedAt = &now
	r.Spec.Status = "completed"
	r.Spec.SettingsError = truncateMigrationError(result.SettingsError)
	r.Spec.JournalError = result.JournalError
	for i := range r.Spec.Items {
		item := &r.Spec.Items[i]
		if item.Result == "pending" || item.Result == "running" {
			item.Result = "not_applied"
			item.Hint = "The import stopped before this item was attempted."
		}
		if item.Result == ResultError || item.Result == "pending" || item.Result == "running" || item.Result == "not_applied" || item.Result == "unconfirmed" {
			r.Spec.Status = "partial"
		}
	}
	if result.SettingsError != "" || result.JournalError != "" || r.Spec.Failure != "" {
		r.Spec.Status = "partial"
	}
	s.persistMigrationProgress(finishCtx, r, result)
	result.RunID = r.Name
}

func equalAuthority(a []string, ap string, b []string, bp string) bool {
	normalize := func(names []string) []string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = strings.ToLower(strings.TrimSuffix(n, "."))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return strings.EqualFold(strings.TrimSuffix(ap, "."), strings.TrimSuffix(bp, ".")) && slices.Equal(normalize(a), normalize(b))
}
func (s *Server) migrationZone(ctx context.Context, r *internal.MigrationRun, z internal.MigrationZone) (*v1alpha1.DNSZone, error) {
	reader, err := s.migrationReader(ctx)
	if err != nil {
		return nil, err
	}
	return migrationZoneFrom(ctx, reader, r, z)
}
func migrationZoneFrom(ctx context.Context, reader migrationRead, r *internal.MigrationRun, z internal.MigrationZone) (*v1alpha1.DNSZone, error) {
	current := &v1alpha1.DNSZone{}
	if err := reader.Get(ctx, store.Key{Kind: "DNSZone", Namespace: z.Namespace, Name: z.Name}, current); err != nil {
		return nil, storeError(err)
	}
	if store.ManagedBy(current) != store.ManagerAPI {
		return nil, errManagedBy(store.KeyOf(current), store.ManagedBy(current))
	}
	if current.Annotations[migrationRunAnnotation] != r.Name || (z.UID != "" && string(current.UID) != z.UID) || current.Spec.ZoneName != z.ZoneName || current.Spec.Type != v1alpha1.ZoneTypePrimary {
		return nil, errConflict("Zone was replaced or belongs to another migration", "Import it again to create a new run")
	}
	expected := equalAuthority(current.Spec.Nameservers, zonePrimary(current), z.Nameservers, z.Primary)
	// Recover a completed write whose following journal update was interrupted.
	if !expected && len(r.Spec.Cutovers) > 0 {
		last := r.Spec.Cutovers[len(r.Spec.Cutovers)-1]
		if ns, _, err := importers.Authority(z.ZoneName, last.Nameservers, last.Addresses); err == nil && len(ns) > 0 {
			expected = equalAuthority(current.Spec.Nameservers, zonePrimary(current), ns, ns[0])
		}
	}
	if !expected {
		return nil, errConflict("Zone authority changed since the migration", "Review the zone before importing again")
	}
	return current, nil
}
func (s *Server) previewMigrationCutover(ctx context.Context, r *internal.MigrationRun, names, addresses []string) (*MigrationCutoverPlan, error) {
	reader, err := s.migrationReader(ctx)
	if err != nil {
		return nil, err
	}
	return s.previewMigrationCutoverFrom(ctx, reader, r, names, addresses, true)
}
func (s *Server) previewMigrationCutoverFrom(ctx context.Context, reader migrationRead, r *internal.MigrationRun, names, addresses []string, authorize bool) (*MigrationCutoverPlan, error) {
	if len(r.Spec.Zones) == 0 {
		return nil, errInvalid("This migration has no authoritative zones to decommission")
	}
	ns, err := importers.Nameservers(names)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	ips, err := importers.NameserverAddresses(addresses)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	if (len(ns) == 0) == (len(ips) == 0) {
		return nil, errInvalid("Choose kudurru DNS names or serving IP addresses")
	}
	if r.Spec.Status == "running" && (r.Spec.Deadline != nil && !s.opts.Now().After(r.Spec.Deadline.Add(15*time.Second)) || r.Spec.Deadline == nil && s.opts.Now().Sub(r.Spec.StartedAt.Time) < 10*time.Minute) {
		return nil, errConflict("Migration is still running", "Wait for the import to finish")
	}
	if len(r.Spec.Cutovers) > 0 {
		last := r.Spec.Cutovers[len(r.Spec.Cutovers)-1]
		if last.Status == "running" && s.opts.Now().Sub(last.StartedAt.Time) < 10*time.Minute {
			return nil, errConflict("A cutover is still running", "Wait for it to finish; interrupted attempts can be retried after ten minutes")
		}
	}
	p := &MigrationCutoverPlan{Revision: r.ResourceVersion, Nameservers: ns, Addresses: ips, Zones: []MigrationAuthorityChange{}, Warnings: []string{
		"Nameserver DNS names must resolve to kudurru's serving addresses. Check DNS answers before retiring the source server.",
		"This updates apex NS and SOA primary names in the imported zones, plus generated A/AAAA records when serving IPs are selected. Update registrar/parent delegations, glue, and delegations in other zones separately. The source server is not shut down.",
		"Writes are not transactional. Successful changes remain if another zone fails; review errors and preview again to retry.",
	}}
	if s.currentMigrations() {
		p.Warnings[2] = "Each zone and its generated nameserver records commit together. Successful earlier zones remain if a later command fails; review errors and preview again to retry."
	}
	if len(ips) > 0 {
		sets, err := migrationRecordSets(ctx, reader)
		if err != nil {
			return nil, err
		}
		p.recordSets = make(map[string][]*v1alpha1.DNSRecordSet)
		for _, set := range sets {
			p.recordSets[set.Spec.ZoneRef.Name] = append(p.recordSets[set.Spec.ZoneRef.Name], set)
		}
	}
	for _, tracked := range r.Spec.Zones {
		c := MigrationAuthorityChange{Name: tracked.Name, Namespace: tracked.Namespace, ZoneName: tracked.ZoneName, Nameservers: []string{}}
		z, err := migrationZoneFrom(ctx, reader, r, tracked)
		if err == nil && authorize {
			k, _ := kindByKind("DNSZone")
			body, _ := objectMap(k, z)
			err = s.authorizeWrite(ctx, k, writeRequest{kind: k, body: body})
		}
		if err == nil {
			c.TargetNameservers, _, err = importers.Authority(z.Spec.ZoneName, ns, ips)
		}
		if err == nil && len(ips) > 0 {
			c.AddressRecordSet, err = s.migrationAddressesFrom(ctx, reader, r, z, ips, p.recordSets[z.Name], authorize)
		}
		if err != nil {
			c.Error = err.Error()
		} else {
			c.ResourceVersion = z.ResourceVersion
			c.Nameservers = append([]string{}, z.Spec.Nameservers...)
			c.Primary = zonePrimary(z)
		}
		p.Zones = append(p.Zones, c)
	}
	return p, nil
}
func (s *Server) applyMigrationCutover(ctx context.Context, id string, p *MigrationCutoverPlan) (*MigrationResult, error) {
	if s.currentMigrations() {
		return s.applyCurrentCutover(ctx, id, p)
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	r, err := s.getMigrationRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.ResourceVersion != p.Revision {
		return nil, errPrecondition(p.Revision, r.ResourceVersion)
	}
	fresh, err := s.previewMigrationCutover(ctx, r, p.Nameservers, p.Addresses)
	if err != nil {
		return nil, err
	}
	if len(fresh.Zones) != len(p.Zones) {
		return nil, errInvalid("Preview every zone in the run before applying")
	}
	for i, z := range fresh.Zones {
		reviewed := p.Zones[i]
		if reviewed.Name != z.Name || reviewed.Namespace != z.Namespace || reviewed.ResourceVersion != z.ResourceVersion || reviewed.Error != z.Error || !reflect.DeepEqual(reviewed.AddressRecordSet, z.AddressRecordSet) {
			return nil, errConflict("A zone changed after the preview", "Preview the cutover again")
		}
	}
	if len(r.Spec.Cutovers) >= 100 {
		return nil, errInvalid("This migration already has 100 cutover attempts; import again to start a new run")
	}
	// Close an abandoned attempt when its successor takes over.
	now := metav1.NewTime(s.opts.Now())
	if r.Spec.Status == "running" {
		r.Spec.Status = "partial"
		r.Spec.FinishedAt = &now
	}
	if n := len(r.Spec.Cutovers); n > 0 && r.Spec.Cutovers[n-1].Status == "running" {
		r.Spec.Cutovers[n-1].Status = "partial"
		r.Spec.Cutovers[n-1].FinishedAt = &now
	}
	// Keep the reviewed snapshot as the expected authority, including recovery
	// from an interrupted earlier cutover.
	for i, c := range fresh.Zones {
		if c.Error == "" {
			r.Spec.Zones[i].Nameservers = c.Nameservers
			r.Spec.Zones[i].Primary = c.Primary
		}
	}
	attempt := internal.MigrationCutover{StartedAt: metav1.NewTime(s.opts.Now()), Actor: s.actor(ctx), Status: "running", Nameservers: fresh.Nameservers, Addresses: fresh.Addresses, Items: []internal.MigrationItem{}}
	r.Spec.Cutovers = append(r.Spec.Cutovers, attempt)
	if err := migrationCutoverCapacity(r, fresh); err != nil {
		return nil, err
	}
	if err := s.saveMigrationRun(ctx, r); err != nil {
		return nil, storeError(err)
	}
	result := &MigrationResult{RunID: r.Name, Items: []ApplyResult{}}
	// Bound an attempt so a crashed worker's lease can eventually be recovered.
	workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	visited := 0
	for i, c := range fresh.Zones {
		visited++
		item := ApplyResult{Kind: "DNSZone", Namespace: c.Namespace, Name: c.Name, Result: ResultError, Error: c.Error}
		if c.Error == "" {
			z, e := s.migrationZone(workCtx, r, r.Spec.Zones[i])
			if e == nil && z.ResourceVersion != c.ResourceVersion {
				e = errPrecondition(c.ResourceVersion, z.ResourceVersion)
			}
			if e == nil && c.AddressRecordSet != nil {
				var addressItem ApplyResult
				addressItem, e = s.writeMigrationAddresses(workCtx, r, z, fresh.Addresses, c.AddressRecordSet, fresh.recordSets[z.Name])
				result.Items = append(result.Items, addressItem)
				last := &r.Spec.Cutovers[len(r.Spec.Cutovers)-1]
				last.Items = append(last.Items, migrationItem(addressItem))
				if !s.persistMigrationProgress(workCtx, r, result) {
					break
				}
			}
			if e == nil {
				z.Spec.Nameservers = append([]string{}, c.TargetNameservers...)
				if z.Spec.SOA == nil {
					z.Spec.SOA = &v1alpha1.SOASpec{}
				}
				z.Spec.SOA.MName = c.TargetNameservers[0]
				k, _ := kindByKind("DNSZone")
				body, mapErr := objectMap(k, z)
				e = mapErr
				req := writeRequest{kind: k, body: body, ifMatch: c.ResourceVersion}
				if e == nil {
					e = s.authorizeWrite(workCtx, k, req)
				}
				if e == nil {
					wr, writeErr := s.writeObject(workCtx, req)
					e = writeErr
					if e == nil {
						item.Result = wr.result
						item.ResourceVersion = wr.obj.GetResourceVersion()
						tracked := &r.Spec.Zones[i]
						tracked.UID = string(wr.obj.GetUID())
						tracked.Nameservers = c.TargetNameservers
						tracked.Primary = c.TargetNameservers[0]
					}
				}
			}
			if e != nil {
				item.Error = e.Error()
			}
		}
		result.Items = append(result.Items, item)
		last := &r.Spec.Cutovers[len(r.Spec.Cutovers)-1]
		last.Items = append(last.Items, migrationItem(item))
		if !s.persistMigrationProgress(workCtx, r, result) {
			break
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	last := &r.Spec.Cutovers[len(r.Spec.Cutovers)-1]
	now = metav1.NewTime(s.opts.Now())
	last.FinishedAt = &now
	last.Status = "completed"
	if visited != len(fresh.Zones) || result.JournalError != "" {
		last.Status = "partial"
	}
	for _, item := range last.Items {
		if item.Result == ResultError {
			last.Status = "partial"
		}
	}
	s.persistMigrationProgress(finishCtx, r, result)
	return result, nil
}

// Reserve enough room for every outcome before claiming an attempt or writing
// DNS data. Error details are bounded by their encoded JSON size.
func migrationCutoverCapacity(r *internal.MigrationRun, p *MigrationCutoverPlan) error {
	projected := r.DeepCopy()
	last := &projected.Spec.Cutovers[len(projected.Spec.Cutovers)-1]
	for i, change := range p.Zones {
		item := internal.MigrationItem{Kind: "DNSZone", Namespace: change.Namespace, Name: change.Name}
		reserveMigrationItem(&item)
		last.Items = append(last.Items, item)
		if change.AddressRecordSet != nil {
			rs := change.AddressRecordSet
			item := internal.MigrationItem{Kind: "DNSRecordSet", Namespace: rs.Namespace, Name: rs.Name}
			reserveMigrationItem(&item)
			last.Items = append(last.Items, item)
		}
		// Include both old and target authority to bound every partial outcome.
		tracked := &projected.Spec.Zones[i]
		tracked.Nameservers = append(tracked.Nameservers, change.TargetNameservers...)
		if len(change.TargetNameservers) > 0 {
			tracked.Primary += change.TargetNameservers[0]
		}
		if tracked.UID == "" {
			tracked.UID = strings.Repeat("x", 64)
		}
	}
	b, err := json.Marshal(projected)
	if err != nil {
		return err
	}
	// Allow for finish timestamps and store-assigned metadata growth as well.
	if len(b)+4096 > migrationJournalMaxBytes {
		return errConflict("Migration journal has insufficient space for another complete cutover attempt", "Start a new migration run with fewer zones before retrying; no DNS changes were made")
	}
	return nil
}

func zonePrimary(z *v1alpha1.DNSZone) string {
	if z.Spec.SOA == nil {
		return ""
	}
	return z.Spec.SOA.MName
}
