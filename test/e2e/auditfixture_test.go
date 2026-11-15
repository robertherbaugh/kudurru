package e2e

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// licensedBuild is the binary of the licensed fixtures: built once per run
// of this package, or the prebuilt binary KUDURRU_E2E_LICENSED_BINARY names.
// TestMain removes dir.
var licensedBuild struct {
	once          sync.Once
	dir, bin, log string
	err           error
}

// licensedNodeFixture returns a binary that trusts the existing synthetic
// ten-node license, and the environment of its nodes. Audit, static-cluster
// and enrolled-cluster fixtures use it: MainBuild and KUDURRU_E2E_BINARY
// still supply the ordinary binary for other E2Es. A release binary cannot
// trust this key, so the binary is always a -tags licensetest build: the
// harness's own, or the prebuilt one of KUDURRU_E2E_LICENSED_BINARY. Each
// test logs which binary it ran (kudurrutest.Describe).
func licensedNodeFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, err := kudurrutest.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	b := &licensedBuild
	b.once.Do(func() {
		if b.dir, b.err = os.MkdirTemp("", "kudurru-e2e-licensed-"); b.err != nil {
			return
		}
		if b.bin, b.err = kudurrutest.BuildLicensed(b.dir); b.err != nil {
			return
		}
		b.log, b.err = kudurrutest.Describe(b.bin)
	})
	if b.err != nil {
		t.Fatalf("licensed fixture binary: %v", b.err)
	}
	t.Logf("licensed fixture binary %s", b.log)
	return b.bin, []string{
		"KUDURRU_LICENSE_KEY_FILE=" + filepath.Join(root, "internal", "license", "testdata", "e2e-license.key"),
		"KUDURRU_ADMIN_PASSWORD=audit-e2e-password-1", // synthetic fixture; avoid generated credentials in failure logs
	}
}

// removeLicensedBuild removes what licensedNodeFixture built.
func removeLicensedBuild() {
	if licensedBuild.dir != "" {
		_ = os.RemoveAll(licensedBuild.dir)
	}
}
