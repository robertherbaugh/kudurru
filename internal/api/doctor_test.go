package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/health"
)

func TestDoctor(t *testing.T) {
	checker := health.New(nil)
	h := newHarness(t, func(o *Options) { o.Health = checker })
	var rep DoctorReport
	h.do(http.MethodGet, "/api/v1/doctor", "viewer", nil).want(http.StatusOK).json(&rep)
	byName := map[string]DoctorCheck{}
	for _, c := range rep.Checks {
		byName[c.Check] = c
	}
	for _, name := range []string{"config", "api", "token", "diagnoses", "dataDir", "ready"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("check %s missing: %+v", name, rep.Checks)
		}
		if c.Status != DoctorOK {
			t.Errorf("%s: %s %s", name, c.Status, c.Message)
		}
	}
	if rep.Failures != 0 || rep.Node != "node-a" {
		t.Fatalf("report %+v", rep)
	}

	checker.Set("dns", false, "listener not bound")
	h.do(http.MethodGet, "/api/v1/doctor", "viewer", nil).want(http.StatusOK).json(&rep)
	failed := map[string]bool{}
	for _, c := range rep.Checks {
		failed[c.Check] = c.Status == DoctorFail
	}
	// Readiness fails, and the unready component is also a diagnosis.
	if !failed["ready"] || !failed["diagnoses"] || rep.Failures != 2 {
		t.Fatalf("unready: %+v", rep.Checks)
	}
	h.do(http.MethodGet, "/api/v1/doctor", "", nil).want(http.StatusUnauthorized)
}

func TestCheckDataDir(t *testing.T) {
	dir := t.TempDir()
	if st, msg := checkDataDir(dir); st != DoctorOK {
		t.Fatalf("%s %s", st, msg)
	}
	if st, _ := checkDataDir(filepath.Join(dir, "missing")); st != DoctorWarn {
		t.Fatalf("missing: %s", st)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.db"), nil, 0o644); err != nil { //nolint:gosec // world-readable on purpose
		t.Fatal(err)
	}
	if st, _ := checkDataDir(dir); st != DoctorWarn {
		t.Fatalf("world-readable state.db: %s", st)
	}
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // the test makes it world-writable on purpose
		t.Fatal(err)
	}
	if st, _ := checkDataDir(dir); st != DoctorWarn {
		t.Fatalf("world-writable: %s", st)
	}
}
