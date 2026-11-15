package kudurrutest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Variables that turn the skip of a missing external program into a failure,
// like KUDURRU_REQUIRE_POSTGRES for the central database
// (internal/central/centraltest, D158). The hosted e2e job sets them, so a
// comparator that lost its program fails there instead of passing as a
// skipped test.
const (
	// EnvRequireBIND covers named and the BIND 9 tools (dnssec-keygen,
	// dnssec-signzone, ...).
	EnvRequireBIND = "KUDURRU_REQUIRE_BIND"
	// EnvRequireEnvtest covers the envtest control-plane binaries
	// (kube-apiserver and etcd, `make envtest`).
	EnvRequireEnvtest = "KUDURRU_REQUIRE_ENVTEST"
)

// EnvNamed names the BIND 9 named executable to use instead of searching
// for one.
const EnvNamed = "KUDURRU_NAMED"

// Where the distributions install named and the BIND 9 tools, searched after
// PATH (not every user's PATH has the sbin directories).
var (
	namedDirs    = []string{"/usr/sbin", "/usr/local/sbin"}
	bindToolDirs = []string{"/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin"}
)

// Unavailable ends a test that cannot run because an external program is
// missing. It skips the test with the given reason, unless the variable
// named by require is set: then the test fails.
func Unavailable(t testing.TB, require, format string, args ...any) {
	t.Helper()
	reason := fmt.Sprintf(format, args...)
	if os.Getenv(require) != "" {
		t.Fatalf("%s is set: %s", require, reason)
	}
	t.Skip(reason)
}

// Named returns the BIND 9 named executable: $KUDURRU_NAMED when it is set
// (the test fails when that is not an executable), otherwise named on PATH,
// in /usr/sbin or in /usr/local/sbin. Without one the test skips, or fails
// when KUDURRU_REQUIRE_BIND is set.
func Named(t testing.TB) string {
	t.Helper()
	if p := os.Getenv(EnvNamed); p != "" {
		path, err := exec.LookPath(p)
		if err != nil {
			t.Fatalf("%s=%s: %v", EnvNamed, p, err)
		}
		return path
	}
	if path := lookProgram("named", namedDirs...); path != "" {
		return path
	}
	Unavailable(t, EnvRequireBIND, "named (BIND 9) is not installed: dnf install bind / apt-get install bind9")
	return ""
}

// BINDTool returns an installed BIND 9 tool (dnssec-keygen, dnssec-signzone,
// dnssec-verify, dnssec-dsfromkey, ...): on PATH or in the directories the
// packages install them to. Without it the test skips, or fails when
// KUDURRU_REQUIRE_BIND is set.
func BINDTool(t testing.TB, name string) string {
	t.Helper()
	if path := lookProgram(name, bindToolDirs...); path != "" {
		return path
	}
	Unavailable(t, EnvRequireBIND, "%s (BIND 9 tools) is not installed: dnf install bind-dnssec-utils / apt-get install bind9-utils", name)
	return ""
}

// lookProgram finds name on PATH, then in dirs; "" when it is nowhere.
func lookProgram(name string, dirs ...string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range dirs {
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path
		}
	}
	return ""
}
