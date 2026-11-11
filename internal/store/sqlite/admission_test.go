package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// refusedField asserts that err is the validation failure of one object
// naming field, and returns its message.
func refusedField(t *testing.T, err error, field string) string {
	t.Helper()
	var invalid *validator.Error
	if !errors.As(err, &invalid) {
		t.Fatalf("got %v, want a validation failure naming %s", err, field)
	}
	if !slices.Contains(invalid.Fields(), field) {
		t.Fatalf("the refusal names %v, want %s: %v", invalid.Fields(), field, err)
	}
	return invalid.Error()
}

// The store never holds a DNSZone that its complete serving image cannot
// carry: such a write is refused like any invalid object, nothing is stored,
// and the image of the store stays valid.
func TestApplyRefusesZonesTheImageCannotCarry(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	forward := zone("corp", "corp.example.")
	forward.Spec.Type, forward.Spec.Forward = v1alpha1.ZoneTypeForward, &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
	mustApply(t, s, forward, store.ManagerAPI)
	// A record set that names a forward zone is not a zone configuration
	// error: the store admits it and the image carries it (servingzones).
	rs := recordSet("default", "www", nil)
	rs.Spec.ZoneRef.Name = "corp"
	mustApply(t, s, rs, store.ManagerAPI)
	head := s.head.Load()

	for name, tc := range map[string]struct {
		zone  *v1alpha1.DNSZone
		field string
		say   string
	}{
		"external type": {zone: func() *v1alpha1.DNSZone {
			z := zone("ext", "ext.example.")
			z.Spec.Type = v1alpha1.ZoneTypeExternal
			return z
		}(), field: "spec.type", say: `supported values: "primary", "forward", "secondary", "stub"`},
		"catalog type": {zone: func() *v1alpha1.DNSZone {
			z := zone("cat", "cat.example.")
			z.Spec.Type = v1alpha1.ZoneTypeCatalog
			return z
		}(), field: "spec.type"},
		"query ACL": {zone: func() *v1alpha1.DNSZone {
			z := zone("acl", "acl.example.")
			z.Spec.Policy.QueryAccess, z.Spec.Policy.QueryACL = v1alpha1.AccessUseNetworkACL, []string{"10.0.0.0/33"}
			return z
		}(), field: "spec.policy.queryACL[0]", say: "must be a CIDR or an IP address"},
		"transfer ACL of a forward zone": {zone: func() *v1alpha1.DNSZone {
			z := zone("xfr", "xfr.example.")
			z.Spec.Type, z.Spec.Forward = v1alpha1.ZoneTypeForward, &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
			z.Spec.Policy.ZoneTransfer, z.Spec.Policy.TransferACL = v1alpha1.AccessUseNetworkACL, []string{"192.168.1"}
			return z
		}(), field: "spec.policy.transferACL[0]"},
		"notify target of a secondary zone": {zone: func() *v1alpha1.DNSZone {
			z := zone("sec", "sec.example.")
			z.Spec.Type, z.Spec.Secondary = v1alpha1.ZoneTypeSecondary, &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}}
			z.Spec.Policy.Notify, z.Spec.Policy.NotifyTargets = v1alpha1.NotifySpecified, []string{"ns2.example.net"}
			return z
		}(), field: "spec.policy.notifyTargets[0]"},
		"zone name of another DNSZone": {zone: zone("home-again", "HOME.example."), field: "spec.zoneName",
			say: `Duplicate value: "HOME.example.": the zone is also configured by DNSZone "home"`},
		"zone name of a forward zone": {zone: zone("corp-again", "corp.example."), field: "spec.zoneName", say: `DNSZone "corp"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Apply(ctx, tc.zone, store.ApplyOptions{Manager: store.ManagerAPI})
			if msg := refusedField(t, err, tc.field); !strings.Contains(msg, tc.say) || !strings.HasPrefix(msg, `DNSZone.kudurru.io "`+tc.zone.Name+`" is invalid: `) {
				t.Fatalf("refusal: %s", msg)
			}
			var got v1alpha1.DNSZone
			if err := s.Get(ctx, store.KeyOf(tc.zone), &got); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the refused zone was stored: %v", err)
			}
		})
	}
	t.Run("update of a stored zone", func(t *testing.T) {
		broken := zone("home", "home.example.")
		broken.Spec.Policy.ZoneTransfer, broken.Spec.Policy.TransferACL = v1alpha1.AccessUseNetworkACL, []string{"nope"}
		_, err := s.Apply(ctx, broken, store.ApplyOptions{Manager: store.ManagerAPI})
		refusedField(t, err, "spec.policy.transferACL[0]")
		var got v1alpha1.DNSZone
		if err := s.Get(ctx, store.KeyOf(broken), &got); err != nil || len(got.Spec.Policy.TransferACL) != 0 || got.Generation != 1 {
			t.Fatalf("the refused update changed the zone: %+v %v", got.Spec.Policy, err)
		}
		// A valid update of the same zone, to its own zone name, passes.
		fixed := zone("home", "home.example.")
		fixed.Spec.Policy.ZoneTransfer, fixed.Spec.Policy.TransferACL = v1alpha1.AccessUseNetworkACL, []string{"192.0.2.0/24"}
		if _, err := s.Apply(ctx, fixed, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("command transaction", func(t *testing.T) {
		// Two zones for one name in one transaction: the second sees the first.
		err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
			if _, err := tx.Apply(ctx, zone("lab", "lab.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				return err
			}
			_, err := tx.Apply(ctx, zone("lab-again", "lab.example."), store.ApplyOptions{Manager: store.ManagerAPI})
			return err
		})
		refusedField(t, err, "spec.zoneName")
		var got v1alpha1.DNSZone
		if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "lab"}, &got); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the refused transaction stored its first zone: %v", err)
		}
	})
	t.Run("the name is free again after the zone is deleted", func(t *testing.T) {
		mustApply(t, s, zone("temp", "temp.example."), store.ManagerAPI)
		if _, err := s.Apply(ctx, zone("temp-next", "temp.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
			t.Fatal("two zones for temp.example.")
		}
		if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "temp"}, store.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, s, zone("temp-next", "temp.example."), store.ManagerAPI)
	})
	t.Run("concurrent creations of one zone name", func(t *testing.T) {
		const writers = 8
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := range writers {
			wg.Go(func() {
				_, errs[i] = s.Apply(ctx, zone(fmt.Sprintf("race-%d", i), "race.example."), store.ApplyOptions{Manager: store.ManagerAPI})
			})
		}
		wg.Wait()
		stored := 0
		for _, err := range errs {
			if err == nil {
				stored++
				continue
			}
			refusedField(t, err, "spec.zoneName")
		}
		list, err := s.List(ctx, "DNSZone", store.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		named := 0
		for _, item := range list.Items {
			if item.(*v1alpha1.DNSZone).Spec.ZoneName == "race.example." {
				named++
			}
		}
		if stored != 1 || named != 1 {
			t.Fatalf("%d of %d concurrent creations succeeded and %d zones are stored, want 1 and 1", stored, writers, named)
		}
	})
	t.Run("a zone that is being removed is not judged", func(t *testing.T) {
		held := zone("held", "held.example.")
		held.Finalizers = []string{"kudurru.io/test"}
		mustApply(t, s, held, store.ManagerAPI)
		if err := s.Delete(ctx, store.KeyOf(held), store.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		var deleting v1alpha1.DNSZone
		if err := s.Get(ctx, store.KeyOf(held), &deleting); err != nil || deleting.DeletionTimestamp == nil {
			t.Fatalf("the zone is not being deleted: %v", err)
		}
		// The last write of the zone clears its finalizer, whatever its policy.
		deleting.Finalizers = nil
		deleting.Spec.Policy.ZoneTransfer, deleting.Spec.Policy.TransferACL = v1alpha1.AccessUseNetworkACL, []string{"nope"}
		if _, err := s.Apply(ctx, &deleting, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatalf("the finalizer of a zone that is being removed could not be cleared: %v", err)
		}
		if err := s.Get(ctx, store.KeyOf(held), &deleting); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the zone was not removed: %v", err)
		}
	})
	// Every refusal left the store as it was, and what it holds is an image
	// the replication validator accepts.
	if _, err := s.ValidateImage(ctx, *imageOf(t, s)); err != nil {
		t.Fatal(err)
	}
	if s.head.Load() == head {
		t.Fatal("the valid writes of the subtests were not stored")
	}
}

// The zone-name check of a new DNSZone is an index lookup. Without the index
// every creation reads every stored zone, which made the conformance case
// that writes 1,500 zones run into its deadline.
func TestZoneNameCheckUsesItsIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path)
	plan := func(s *Store) string {
		t.Helper()
		query := strings.Replace(strings.Replace(sqlZoneWithApex, "?", "'home.example.'", 1), "?", "'home'", 1)
		return strings.Join(rowsOf(t, s.w, "EXPLAIN QUERY PLAN "+query), "\n")
	}
	if got := plan(s); !strings.Contains(got, "objects_zone_name") {
		t.Fatalf("the zone-name check does not use its index:\n%s", got)
	}
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	// A database of this schema from before the index gets it at the next
	// open, and the check works either way.
	if _, err := s.w.Exec(`DROP INDEX objects_zone_name`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), zone("home-again", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("two zones for one name without the index")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again := openAt(t, path)
	if got := plan(again); !strings.Contains(got, "objects_zone_name") {
		t.Fatalf("the index was not restored by the open:\n%s", got)
	}
	if _, err := again.Apply(context.Background(), zone("home-again", "HOME.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("two zones for one name")
	}
}

// legacyUpgradeDB is a schema-1 database (the 0.1.0 release) with objects of
// that release: valid ones, and the shapes 0.1.0 stored that this release
// cannot serve. It returns the keys of the latter, sorted.
func legacyUpgradeDB(t *testing.T) (*sql.DB, string, []string) {
	t.Helper()
	db, path := legacyCredentialDB(t)
	created := metav1.NewTime(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	stamp := func(obj store.Object) store.Object {
		obj.SetCreationTimestamp(created)
		return obj
	}
	typed := func(name, zoneName string, typ v1alpha1.ZoneType) *v1alpha1.DNSZone {
		z := zone(name, zoneName)
		z.Spec.Type = typ
		return z
	}
	emptySecondary := typed("secondary-empty", "secondary-empty.example.", v1alpha1.ZoneTypeSecondary)
	emptySecondary.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{}}
	policy := &v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1alpha1.PolicySpec{
		ClientGroupRefs: []string{"lan"}, Rewrites: []v1alpha1.Rewrite{{Name: "placeholder.example."}},
	}}
	badACL := typed("acl", "acl.example.", v1alpha1.ZoneTypeForward)
	badACL.Spec.Policy.QueryAccess, badACL.Spec.Policy.QueryACL = v1alpha1.AccessUseNetworkACL, []string{"not-a-network"}
	for i, obj := range []store.Object{
		zone("home", "home.example."),
		clientGroup("default", "lan"),
		typed("corp", "corp.example.", v1alpha1.ZoneTypeForward),
		typed("secondary", "secondary.example.", v1alpha1.ZoneTypeSecondary),
		emptySecondary,
		typed("stub", "stub.example.", v1alpha1.ZoneTypeStub),
		typed("external", "external.example.", v1alpha1.ZoneTypeExternal),
		typed("catalog", "catalog.example.", v1alpha1.ZoneTypeCatalog),
		zone("home-again", "HOME.example."),
		badACL,
		policy,
	} {
		insertLegacyCredential(t, db, stamp(obj), i+1)
	}
	refused := []string{"DNSZone/acl", "DNSZone/catalog", "DNSZone/external", "DNSZone/home", "DNSZone/home-again", "DNSZone/secondary",
		"DNSZone/secondary-empty", "DNSZone/stub", "Policy/default/default"}
	return db, path, refused
}

// rowsOf renders every row of a query, its columns separated by a space, for
// before and after comparisons.
func rowsOf(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		refs := make([]any, len(cols))
		for i := range values {
			refs[i] = &values[i]
		}
		if err := rows.Scan(refs...); err != nil {
			t.Fatal(err)
		}
		columns := make([]string, len(values))
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			columns[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(columns, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// 0.1.0 stored zones of types it did not serve, with or without their
// configuration, several DNSZones for one zone name, zone policies that do
// not compile and free-form policy rewrites. Opening such a database is not
// an upgrade that leaves a node serving nothing on a schema 0.1.0 no longer
// opens: it is refused, names every object, and changes nothing.
func TestUpgradeLegacyPlaceholderObjectsAreRefused(t *testing.T) {
	db, path, refused := legacyUpgradeDB(t)
	objects := rowsOf(t, db, `SELECT kind, namespace, name, uid, generation, resource_version, data FROM objects ORDER BY kind, namespace, name`)
	meta := rowsOf(t, db, `SELECT key, value FROM meta ORDER BY key`)
	tables := rowsOf(t, db, `SELECT name FROM sqlite_master ORDER BY name`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Open(path, testOptions(internalOptions))
	var upgrade *UpgradeError
	if !errors.Is(err, ErrUpgradeRefused) || !errors.As(err, &upgrade) {
		t.Fatalf("Open = %v, want ErrUpgradeRefused", err)
	}
	if upgrade.From != 1 || upgrade.To != store.SchemaVersion {
		t.Fatalf("refusal of an upgrade from %d to %d", upgrade.From, upgrade.To)
	}
	var keys []string
	reasons := map[string]string{}
	for _, issue := range upgrade.Issues {
		keys = append(keys, issue.Key.String())
		reasons[issue.Key.String()] = issue.Reason
	}
	if !slices.Equal(keys, refused) {
		t.Fatalf("refused objects %v, want %v", keys, refused)
	}
	for key, want := range map[string]string{
		"DNSZone/secondary":       "secondary zones require secondary configuration",
		"DNSZone/secondary-empty": "spec.secondary.primaries",
		"DNSZone/stub":            "stub zones require nameserver configuration",
		"DNSZone/external":        `spec.type: Unsupported value: "external": supported values: "primary", "forward", "secondary", "stub"`,
		"DNSZone/catalog":         `spec.type: Unsupported value: "catalog"`,
		"DNSZone/home":            `spec.zoneName: Duplicate value: "home.example.": the zone is also configured by DNSZone "home-again"`,
		"DNSZone/home-again":      `spec.zoneName: Duplicate value: "HOME.example.": the zone is also configured by DNSZone "home"`,
		"DNSZone/acl":             `spec.policy.queryACL[0]: Invalid value: "not-a-network"`,
		"Policy/default/default":  "spec.rewrites[0]: Invalid value: provide either a CNAME or targets, not both",
	} {
		if !strings.Contains(reasons[key], want) {
			t.Errorf("%s: %q, want it to contain %q", key, reasons[key], want)
		}
	}
	msg := err.Error()
	for _, want := range []string{
		fmt.Sprintf("state.db (schema 1) cannot be upgraded to schema %d: 9 stored objects are not valid in this release:", store.SchemaVersion),
		"\n  DNSZone stub: spec: Invalid value: stub zones require nameserver configuration",
		"\n  Policy default/default: spec.rewrites[0]:",
		"\nstate.db was not changed",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}

	// The refusal is the whole effect: same rows, same schema, no table of
	// a later schema, and the lock released for the earlier release.
	raw, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if got := rowsOf(t, raw, `SELECT kind, namespace, name, uid, generation, resource_version, data FROM objects ORDER BY kind, namespace, name`); !slices.Equal(got, objects) {
		t.Fatal("the refused upgrade changed stored objects")
	}
	if got := rowsOf(t, raw, `SELECT key, value FROM meta ORDER BY key`); !slices.Equal(got, meta) {
		t.Fatalf("the refused upgrade changed meta: %v, was %v", got, meta)
	}
	if got := rowsOf(t, raw, `SELECT name FROM sqlite_master ORDER BY name`); !slices.Equal(got, tables) {
		t.Fatalf("the refused upgrade left tables of a later schema: %v, was %v", got, tables)
	}
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		t.Fatalf("the refused upgrade kept the lock: %v", err)
	}
	_ = lock.release()

	// What the operator does with the earlier release: delete the zones it
	// never served and the duplicate, correct the others. Then the upgrade
	// goes through and the image of the store is valid.
	for _, stmt := range []string{
		`DELETE FROM objects WHERE kind = 'DNSZone' AND name IN ('secondary', 'secondary-empty', 'stub', 'external', 'catalog', 'home-again')`,
		`UPDATE objects SET data = json_remove(data, '$.spec.policy.queryACL', '$.spec.policy.queryAccess') WHERE kind = 'DNSZone' AND name = 'acl'`,
		`UPDATE objects SET data = json_remove(data, '$.spec.rewrites') WHERE kind = 'Policy'`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path, internalOptions)
	if s.Info().SchemaVersion != store.SchemaVersion {
		t.Fatalf("schema %d after the corrected upgrade", s.Info().SchemaVersion)
	}
	var home v1alpha1.DNSZone
	if err := s.Get(context.Background(), store.Key{Kind: "DNSZone", Name: "home"}, &home); err != nil || home.UID != "legacy-home" || home.Generation != 4 {
		t.Fatalf("the upgrade changed a valid zone: %s generation %d, %v", home.UID, home.Generation, err)
	}
	if _, err := s.ValidateImage(context.Background(), *imageOf(t, s)); err != nil {
		t.Fatalf("the upgraded store holds an image that is not valid: %v", err)
	}
}

// Without the refusal the same database is upgraded and then never serves:
// the complete image of the upgraded store is not valid. This is what the
// binary did before, and why the refusal exists.
func TestUpgradeLegacyPlaceholderObjectsWouldFailTheImage(t *testing.T) {
	db, path, _ := legacyUpgradeDB(t)
	// The upgrade as it was: every migration step, no last word.
	if err := migrate(t.Context(), db, migrations, store.SchemaVersion, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path, internalOptions)
	_, err := s.ValidateImage(context.Background(), *imageOf(t, s))
	var invalid *validator.Error
	if !errors.As(err, &invalid) {
		t.Fatalf("ValidateImage = %v, want the validation failure of a stored object", err)
	}
}

// The refusal is the last word of an upgrade, while the earlier release can
// still run on the file. A new database has nothing to judge, and a database
// that is already on the current schema is opened as it is: no earlier
// release could open it, and the store can still remove the object.
func TestUpgradeAdmissionOnlyJudgesAnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path)
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	if _, err := s.w.Exec(`UPDATE objects SET data = json_set(data, '$.spec.type', 'external') WHERE kind = 'DNSZone'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, testOptions())
	if err != nil {
		t.Fatalf("a database on the current schema was refused: %v", err)
	}
	defer func() { _ = again.Close() }()
	if err := again.Delete(context.Background(), store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}
