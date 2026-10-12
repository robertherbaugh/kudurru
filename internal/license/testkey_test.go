//go:build licensetest

package license

import (
	"os"
	"testing"
	"time"
)

// The key the dashboard suite hands to its server verifies with the test key
// and grants everything for the foreseeable future.
func TestE2EKeyVerifies(t *testing.T) {
	m, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/e2e-license.key")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set(string(raw), "test"); err != nil {
		t.Fatal(err)
	}
	s := m.Status()
	if s.State != StateActive || s.Nodes != 10 || s.PolicyLimit != 0 || len(s.Features) != len(Features()) {
		t.Fatalf("status = %+v", s)
	}
	if s.ExpiresAt.Before(time.Now().AddDate(50, 0, 0)) {
		t.Fatalf("the e2e key expires at %v", s.ExpiresAt)
	}
}
