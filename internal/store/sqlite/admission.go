package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

// This driver publishes one complete serving image or nothing: a stored
// object that the image cannot carry stops DNS, readiness and the whole API,
// and the API is then not there to correct the object. The store therefore
// never holds such an object (D373):
//
//   - a write of a DNSZone is refused when the image could not build the
//     zone (zone.ServingErrors) or another DNSZone already has its zone name;
//   - a schema upgrade is refused, with the database unchanged, when an
//     object written by the earlier release is not valid in this one
//     (admitUpgrade).

// sqlZoneWithApex finds another DNSZone with a zone name. Zone names match
// the schema pattern (letters, digits, '-', '_' and dots, with a trailing
// dot), so the ASCII lower case of the stored name is its canonical form.
// The expression and the kind literal are those of sqlZoneNameIndex, which
// answers the lookup without reading the zones.
const sqlZoneWithApex = `SELECT name FROM objects WHERE kind = 'DNSZone' AND lower(json_extract(data, '$.spec.zoneName')) = ? AND name <> ? LIMIT 1`

// sqlZoneNameIndex keeps the zone-name lookup of a new DNSZone logarithmic:
// without it every creation reads every stored zone, and loading ten thousand
// zones reads fifty million. It is not a numbered migration step. No release
// wrote schema 9 without it, an index holds no data of its own, and creating
// it again does nothing, so migrate ensures it on every database at schema 9
// or later, in the transaction of the open.
const sqlZoneNameIndex = `CREATE INDEX IF NOT EXISTS objects_zone_name ON objects(lower(json_extract(data, '$.spec.zoneName'))) WHERE kind = 'DNSZone'`

// zoneNameIndexSchema is the first schema migrate keeps sqlZoneNameIndex on.
const zoneNameIndexSchema = 9

// admitZone is the part of the DNSZone rule that needs only the object: the
// image must be able to build the zone. It returns the canonical zone name
// for the uniqueness check that writeMutation makes inside the transaction.
func (s *Store) admitZone(ki *kindInfo, key store.Key, dz *v1alpha1.DNSZone) (string, error) {
	if errs := zonemodel.ServingErrors(dz); len(errs) > 0 {
		return "", validator.NewInvalid(schema.GroupKind{Group: ki.gvk.Group, Kind: ki.gvk.Kind}, key.Namespace, key.Name, errs)
	}
	return dynamicupdate.CanonicalName(dz.Spec.ZoneName)
}

// admitZoneName refuses a new DNSZone whose zone name another DNSZone has.
// It runs in the write transaction, under the write lock, so two concurrent
// creations cannot both pass.
func (s *Store) admitZoneName(ctx context.Context, q txQueryer, m *mutation) error {
	var other string
	switch err := q.QueryRowContext(ctx, sqlZoneWithApex, m.apex, m.key.Name).Scan(&other); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("store/sqlite: check the zone name of %s: %w", m.key, err)
	}
	dz, _ := m.obj.(*v1alpha1.DNSZone)
	name := m.apex
	if dz != nil {
		name = dz.Spec.ZoneName
	}
	return validator.NewInvalid(schema.GroupKind{Group: m.ki.gvk.Group, Kind: m.ki.gvk.Kind}, m.key.Namespace, m.key.Name,
		field.ErrorList{duplicateZoneName(name, other)})
}

func duplicateZoneName(zoneName string, others ...string) *field.Error {
	quoted := make([]string, len(others))
	for i, o := range others {
		quoted[i] = fmt.Sprintf("%q", o)
	}
	return &field.Error{Type: field.ErrorTypeDuplicate, Field: "spec.zoneName", BadValue: zoneName,
		Detail: "the zone is also configured by DNSZone " + strings.Join(quoted, ", ")}
}

// ErrUpgradeRefused means a schema upgrade found stored objects that are not
// valid in this release and changed nothing (*UpgradeError has the objects).
var ErrUpgradeRefused = errors.New("store/sqlite: stored objects are not valid in this release")

// UpgradeIssue is one stored object that stops a schema upgrade.
type UpgradeIssue struct {
	Key    store.Key
	Reason string
}

// UpgradeError is the refusal of a schema upgrade. The transaction of the
// upgrade was rolled back: the database is exactly as the earlier release
// left it, and that release still opens it.
type UpgradeError struct {
	From, To int
	Issues   []UpgradeIssue
}

// maxUpgradeIssuesShown bounds the message; Issues always has every object.
const maxUpgradeIssuesShown = 50

func (e *UpgradeError) Error() string {
	var b strings.Builder
	noun := "objects are"
	if len(e.Issues) == 1 {
		noun = "object is"
	}
	fmt.Fprintf(&b, "store/sqlite: state.db (schema %d) cannot be upgraded to schema %d: %d stored %s not valid in this release:", e.From, e.To, len(e.Issues), noun)
	for i, issue := range e.Issues {
		if i == maxUpgradeIssuesShown {
			fmt.Fprintf(&b, "\n  ... and %d more", len(e.Issues)-i)
			break
		}
		fmt.Fprintf(&b, "\n  %s %s: %s", issue.Key.Kind, namespacedName(issue.Key), issue.Reason)
	}
	b.WriteString("\nstate.db was not changed")
	return b.String()
}

// Unwrap makes errors.Is(err, ErrUpgradeRefused) true.
func (e *UpgradeError) Unwrap() error { return ErrUpgradeRefused }

func namespacedName(k store.Key) string {
	if k.Namespace == "" {
		return k.Name
	}
	return k.Namespace + "/" + k.Name
}

// admitUpgrade is the last step of a schema upgrade, inside its transaction
// and after every migration step: each stored object must pass what the
// complete serving image of this release checks for it (strict decoding,
// source metadata, the current CRD schema or the kind's own validation), and
// the DNSZones must pass the rules admitZone and admitZoneName apply to
// writes. A release may tighten its schemas, and the earlier release may
// have stored what this one refuses to write (0.1.0 stored secondary and stub
// zones without their configuration, zones of types it did not serve, several
// DNSZones for one zone name and free-form policy rewrites). Upgrading such
// a database would leave a node that serves nothing and cannot be corrected
// through its API, on a schema the earlier release no longer opens. The
// upgrade is refused instead, naming every object.
func (s *Store) admitUpgrade(ctx context.Context, tx *sql.Tx, from int) error {
	validate := s.replicationValidation(ctx).ValidateObject
	rows, err := tx.QueryContext(ctx, `SELECT kind, namespace, name, data FROM objects ORDER BY kind, namespace, name`)
	if err != nil {
		return fmt.Errorf("store/sqlite: read the objects to upgrade: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var issues []UpgradeIssue
	zones := map[string][]string{} // canonical zone name -> DNSZone names
	spelled := map[string]string{} // DNSZone name -> spec.zoneName
	for rows.Next() {
		var key store.Key
		var kind, data string
		if err := rows.Scan(&kind, &key.Namespace, &key.Name, &data); err != nil {
			return fmt.Errorf("store/sqlite: read the objects to upgrade: %w", err)
		}
		key.Kind = store.Kind(kind)
		ki, err := s.kinds.get(key.Kind)
		if err != nil {
			issues = append(issues, UpgradeIssue{Key: key, Reason: "this release has no kind " + kind})
			continue
		}
		obj := ki.newObject()
		d := json.NewDecoder(bytes.NewReader([]byte(data)))
		d.DisallowUnknownFields()
		if err := d.Decode(obj); err != nil {
			issues = append(issues, UpgradeIssue{Key: key, Reason: strings.TrimPrefix(err.Error(), "json: ")})
			continue
		}
		if err := validate(json.RawMessage(data)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			issues = append(issues, UpgradeIssue{Key: key, Reason: upgradeReason(err)})
			continue
		}
		dz, ok := obj.(*v1alpha1.DNSZone)
		if !ok {
			continue
		}
		if errs := zonemodel.ServingErrors(dz); len(errs) > 0 {
			issues = append(issues, UpgradeIssue{Key: key, Reason: fieldReasons(errs)})
			continue
		}
		if apex, err := dynamicupdate.CanonicalName(dz.Spec.ZoneName); err == nil {
			zones[apex] = append(zones[apex], key.Name)
			spelled[key.Name] = dz.Spec.ZoneName
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store/sqlite: read the objects to upgrade: %w", err)
	}
	for _, names := range zones {
		if len(names) < 2 {
			continue
		}
		for i, name := range names {
			others := append(append([]string{}, names[:i]...), names[i+1:]...)
			issues = append(issues, UpgradeIssue{Key: store.Key{Kind: "DNSZone", Name: name},
				Reason: fieldReasons(field.ErrorList{duplicateZoneName(spelled[name], others...)})})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Key.String() < issues[j].Key.String() })
	return &UpgradeError{From: from, To: store.SchemaVersion, Issues: issues}
}

// upgradeReason is the reason of one refused object without the object's
// own name, which the message already has.
func upgradeReason(err error) string {
	var invalid *validator.Error
	if !errors.As(err, &invalid) || len(invalid.Causes) == 0 {
		return err.Error()
	}
	var reasons []string
	unchecked := false
	for _, c := range invalid.Causes {
		// The schema validator adds this cause, on no field, when it skipped
		// the rules that need a structurally valid object.
		if strings.Contains(c.Message, "some validation rules were not checked") {
			unchecked = true
			continue
		}
		reasons = append(reasons, c.String())
	}
	if unchecked {
		reasons = append(reasons, "further rules are checked once this is corrected")
	}
	return strings.Join(reasons, "; ")
}

func fieldReasons(errs field.ErrorList) string {
	reasons := make([]string, len(errs))
	for i, e := range errs {
		reasons[i] = e.Error()
	}
	return strings.Join(reasons, "; ")
}
