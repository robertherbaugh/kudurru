package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// licensedAuditFixture builds a binary that trusts the existing synthetic
// ten-node license. Only the audit fixtures use it: MainBuild and
// KUDURRU_E2E_BINARY still supply the ordinary binary for other E2Es.
// A release binary cannot trust this key, so this build is always required.
func licensedAuditFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, err := kudurrutest.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "kudurru")
	build := exec.Command("go", "build", "-tags=licensetest", "-trimpath", "-o", bin, "./cmd/kudurru") //nolint:gosec // fixed test-only build arguments
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build licensed audit fixture: %v\n%s", err, out)
	}
	return bin, []string{
		"KUDURRU_LICENSE_KEY_FILE=" + filepath.Join(root, "internal", "license", "testdata", "e2e-license.key"),
		"KUDURRU_ADMIN_PASSWORD=audit-e2e-password-1", // synthetic fixture; avoid generated credentials in failure logs
	}
}
