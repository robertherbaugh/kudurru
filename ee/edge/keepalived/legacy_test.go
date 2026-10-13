package keepalived

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnauthenticatedInstances(t *testing.T) {
	for file, want := range map[string][]string{
		"legacy.conf":          {"VI_dns", "VI_dns2"},
		"two-vips-node-a.conf": nil,
	} {
		b, err := os.ReadFile(filepath.Join("testdata", file)) //nolint:gosec // test data
		if err != nil {
			t.Fatal(err)
		}
		got, total := UnauthenticatedInstances(b)
		if total != 2 || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v of %d, want %v of 2", file, got, total, want)
		}
	}
	if got, total := UnauthenticatedInstances([]byte("global_defs {\n}\n")); got != nil || total != 0 {
		t.Errorf("no instances: %v %d", got, total)
	}
}

func TestLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	if err := writeLegacyMarker(dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LegacyMarkerPath(dir)); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if err := writeLegacyMarker(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LegacyMarkerPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("marker not removed: %v", err)
	}
	if err := writeLegacyMarker(dir, false); err != nil {
		t.Errorf("removing a missing marker: %v", err)
	}
}
