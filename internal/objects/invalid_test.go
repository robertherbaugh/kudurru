package objects

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// badYAML mixes every kind of broken document with good ones; the good ones
// must be applied and every bad one reported.
const badYAML = `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: good }
spec: { zoneName: good.example., type: primary }
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: typo }
spec: { zoneName: typo.example., type: primary, ttl: 300 }
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: nodot }
spec: { zoneName: nodot.example, type: primary }
---
apiVersion: kudurru.io/v1alpha1
kind: Zone
metadata: { name: wrongkind }
spec: {}
---
just a string
---
kind: DNSZone
metadata: { name: noversion }
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: { name: oneof }
spec: { sources: [{ url: "https://a.example/", configMapRef: { name: lists } }] }
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: { name: dupkey }
spec: { priority: 1, priority: 2 }
`

const syntaxErrorYAML = `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: broken
spec: [
`

func TestLoadInvalidDocumentsDoNotAbort(t *testing.T) {
	f := newFixture(t, []map[string]any{
		{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": map[string]any{"name": "inline-bad"},
			"spec": map[string]any{"clients": []any{map[string]any{"ip": "10.0.0.1", "cidr": "10.0.0.0/8"}}}},
	})
	f.write(t, "a-good.yaml", listsYAML)
	f.write(t, "b-bad.yaml", badYAML)
	f.write(t, "c-syntax.yaml", syntaxErrorYAML)
	f.write(t, "d-good.yaml", zonesYAML)

	rep := f.l.Load(ctx(t))
	for _, k := range []store.Key{zoneKey("good"), zoneKey("home"), nsKey("BlockList", "default", "ads"), nsKey("ClientGroup", "dns", "kids")} {
		if owner(t, f.st, k) == "" {
			t.Errorf("%s must be applied despite the broken documents", k)
		}
	}
	for _, k := range []store.Key{zoneKey("typo"), zoneKey("nodot"), nsKey("BlockList", "default", "oneof"), nsKey("ClientGroup", "default", "inline-bad")} {
		if owner(t, f.st, k) != "" {
			t.Errorf("%s is invalid and must not be applied", k)
		}
	}
	if rep.Created != 4 {
		t.Errorf("created = %d", rep.Created)
	}

	typo := findDiagFor(t, rep.Diagnoses, "DNSZone/typo")
	if typo.Reason != ReasonInvalid || len(typo.Fields) != 1 || typo.Fields[0] != "spec.ttl" {
		t.Errorf("unknown field diagnosis = %+v", typo)
	}
	nodot := findDiagFor(t, rep.Diagnoses, "DNSZone/nodot")
	if nodot.Reason != ReasonInvalid || len(nodot.Fields) != 1 || nodot.Fields[0] != "spec.zoneName" {
		t.Errorf("invalid object diagnosis = %+v", nodot)
	}

	golden(t, "invalid.golden", rep.Diagnoses)
}

func TestLoadBrokenSourceKeepsLastGood(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "zones.yaml", zonesYAML)
	f.l.Load(ctx(t))

	// A syntax error anywhere in the file keeps everything it owned, even
	// documents that now parse but no longer mention the ClientGroup.
	f.write(t, "zones.yaml", strings.Replace(zonesYAML, "kind: ClientGroup", "kind: ClientGroup\n  bad: [", 1))
	rep := f.l.Load(ctx(t))
	findDiag(t, rep.Diagnoses, ReasonInvalidYAML)
	if rep.Deleted != 0 || owner(t, f.st, nsKey("ClientGroup", "dns", "kids")) == "" {
		t.Fatalf("objects of a file that does not parse must be kept: %+v", rep)
	}

	// An invalid version of an existing object keeps the stored one.
	f.write(t, "zones.yaml", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home }
spec: { zoneName: home.example., type: primary, defaultTTL: "soon" }
`)
	rep = f.l.Load(ctx(t))
	if d := findDiag(t, rep.Diagnoses, ReasonInvalid); d.Object != "DNSZone/home" {
		t.Fatalf("diagnosis = %+v", d)
	}
	if z := zone(t, f.st, "home"); z == nil || z.Spec.DefaultTTL != 300 {
		t.Fatalf("last good zone must be kept: %+v", z)
	}
	// The ClientGroup is no longer declared and the file parses: deleted.
	if rep.Deleted != 1 || owner(t, f.st, nsKey("ClientGroup", "dns", "kids")) != "" {
		t.Fatalf("undeclared object of a parsing file must be deleted: %+v", rep)
	}

	// Transition rules see the stored object: zoneName is immutable.
	f.write(t, "zones.yaml", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home }
spec: { zoneName: other.example., type: primary }
`)
	rep = f.l.Load(ctx(t))
	if d := findDiag(t, rep.Diagnoses, ReasonInvalid); len(d.Fields) != 1 || d.Fields[0] != "spec.zoneName" {
		t.Fatalf("immutability diagnosis = %+v", d)
	}
	if z := zone(t, f.st, "home"); z.Spec.ZoneName != "home.example." {
		t.Fatalf("immutable field changed: %q", z.Spec.ZoneName)
	}
}

func TestLoadOwnerUnreadable(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "a.yaml", listsYAML)
	f.l.Load(ctx(t))
	// a.yaml breaks while b.yaml starts declaring the same object: the
	// loader cannot tell whether a.yaml still declares it.
	f.write(t, "a.yaml", "kind: [\n")
	f.write(t, "b.yaml", listsYAML)
	rep := f.l.Load(ctx(t))
	d := findDiag(t, rep.Diagnoses, ReasonOwnerUnreadable)
	if d.Severity != SeverityWarning || d.Owner != "file:objects.d/a.yaml" {
		t.Fatalf("diagnosis = %+v", d)
	}
	if got := owner(t, f.st, nsKey("BlockList", "default", "ads")); got != "file:objects.d/a.yaml" {
		t.Fatalf("owner = %q", got)
	}
}

func findDiagFor(t *testing.T, diags []Diagnosis, object string) Diagnosis {
	t.Helper()
	for _, d := range diags {
		if d.Object == object {
			return d
		}
	}
	t.Fatalf("no diagnosis for %s in %v", object, diags)
	return Diagnosis{}
}

// golden compares diags (as indented JSON) with testdata/name.
func golden(t *testing.T, name string, diags []Diagnosis) {
	t.Helper()
	got, err := json.MarshalIndent(diags, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test path
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("diagnoses differ from %s (run go test -update to accept):\n%s", path, got)
	}
}

var _ = v1alpha1.GroupVersion
