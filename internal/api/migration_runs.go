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

// MigrationRun is the administrator-facing view of the internal journal.
type MigrationRun struct {
	ID       string                    `json:"id"`
	Revision string                    `json:"revision"`
	Data     internal.MigrationRunSpec `json:"data"`
}

func migrationView(r *internal.MigrationRun) MigrationRun {
	return MigrationRun{ID: r.Name, Revision: r.ResourceVersion, Data: r.Spec}
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
		list, err := s.opts.Store.List(ctx, internal.KindMigrationRun, store.ListOptions{Limit: limit, Continue: in.Continue})
		if err != nil {
			return nil, storeError(err)
		}
		items := make([]MigrationRun, 0, len(list.Items))
		for _, obj := range list.Items {
			items = append(items, migrationView(obj.(*internal.MigrationRun)))
		}
		return &migrationRunsOutput{Body: newList(items, list.Continue)}, nil
	})
	register(s, operation(http.MethodGet, "/imports/runs/{id}", "getMigrationRun", "Read a migration run and its cutovers", "imports"), perm{resource: "settings", verb: "write"}, func(ctx context.Context, in *migrationRunInput) (*migrationRunOutput, error) {
		run, err := s.getMigrationRun(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &migrationRunOutput{Body: migrationView(run)}, nil
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
	r := &internal.MigrationRun{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: internal.KindMigrationRun, Name: id}, r); err != nil {
		return nil, storeError(err)
	}
	return r, nil
}
func (s *Server) saveMigrationRun(ctx context.Context, r *internal.MigrationRun) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > store.MaxObjectBytes {
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
	// Reverse timestamp order makes paginated store listings newest first.
	if source == "" {
		source = "manual"
	}
	r := &internal.MigrationRun{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("migration-%016x-%s", 1<<63-1-s.opts.Now().UnixMilli(), uuid.NewString())}, Spec: internal.MigrationRunSpec{
		Source: source, Actor: s.actor(ctx), StartedAt: metav1.NewTime(s.opts.Now()), Status: "running", Items: []internal.MigrationItem{}, Zones: []internal.MigrationZone{}, Warnings: append([]string{}, plan.Warnings...), Cutovers: []internal.MigrationCutover{},
	}}
	if plan.Source != nil {
		if plan.Source.Type != "pihole" && plan.Source.Type != "technitium" {
			return nil, errInvalid("unknown migration source")
		}
		r.Spec.Source = plan.Source.Type
		// Persist only the origin. Paths and queries may contain credentials.
		u, err := url.Parse(plan.Source.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, errInvalid("invalid source URL")
		}
		r.Spec.SourceURL = (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
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
		r.Spec.Items = append(r.Spec.Items, internal.MigrationItem{Kind: kind, Namespace: ns, Name: name, Result: "pending"})
		if kind != "DNSZone" {
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
	if err := s.saveMigrationRun(ctx, r); err != nil {
		return nil, storeError(err)
	}
	return r, nil
}
func migrationItem(r ApplyResult) internal.MigrationItem {
	return internal.MigrationItem{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Result: r.Result, Error: truncateMigrationError(r.Error)}
}
func truncateMigrationError(s string) string {
	if len(s) > 1024 {
		return s[:1024] + "…"
	}
	return s
}

// persistMigrationProgress stops further writes if the journal cannot keep up.
func (s *Server) persistMigrationProgress(ctx context.Context, r *internal.MigrationRun, result *MigrationResult) bool {
	if err := s.saveMigrationRun(ctx, r); err != nil {
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
	for _, item := range r.Spec.Items {
		if item.Result == ResultError || item.Result == "pending" {
			r.Spec.Status = "partial"
		}
	}
	if result.SettingsError != "" || result.JournalError != "" {
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
	current := &v1alpha1.DNSZone{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "DNSZone", Namespace: z.Namespace, Name: z.Name}, current); err != nil {
		return nil, storeError(err)
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
	if r.Spec.Status == "running" && s.opts.Now().Sub(r.Spec.StartedAt.Time) < 10*time.Minute {
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
	if len(ips) > 0 {
		sets, err := s.allRecordSets(ctx, "")
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
		z, err := s.migrationZone(ctx, r, tracked)
		if err == nil {
			k, _ := kindByKind("DNSZone")
			body, _ := objectMap(k, z)
			err = s.authorizeWrite(ctx, k, writeRequest{kind: k, body: body})
		}
		if err == nil {
			c.TargetNameservers, _, err = importers.Authority(z.Spec.ZoneName, ns, ips)
		}
		if err == nil && len(ips) > 0 {
			c.AddressRecordSet, err = s.migrationAddressRecordSet(ctx, r, z, ips, p.recordSets[z.Name])
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

func zonePrimary(z *v1alpha1.DNSZone) string {
	if z.Spec.SOA == nil {
		return ""
	}
	return z.Spec.SOA.MName
}
