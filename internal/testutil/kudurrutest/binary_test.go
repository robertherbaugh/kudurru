package kudurrutest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKudurru compiles a small program that answers `version` like kudurru
// does, with the given build flags, and returns its path: a real Go binary
// with real build information, without the cost of building kudurru.
func fakeKudurru(t *testing.T, flags ...string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod": "module fake.test/kudurru\n\ngo 1.24\n",
		"main.go": `package main

import "fmt"

func main() { fmt.Println("kudurru 9.9.9 (abc1234, go, test/test)") }
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "kudurru")
	cmd := exec.Command("go", append(append([]string{"build"}, flags...), "-o", out, ".")...) //nolint:gosec // fixed test build
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// The log line of a binary names the artifact: its version output, the
// checksum of the file and the build tags recorded by the toolchain.
func TestDescribeNamesTheArtifact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		tags  string
	}{
		{"release build", []string{"-trimpath"}, "tags none"},
		{"license test build", []string{"-trimpath", "-tags=licensetest"}, "tags licensetest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeKudurru(t, tc.flags...)
			got, err := Describe(bin)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(bin) //nolint:gosec // the binary this test built
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			for _, want := range []string{bin, "kudurru 9.9.9 (abc1234, go, test/test)", "sha256 " + hex.EncodeToString(sum[:]), tc.tags, "CGO_ENABLED=0"} {
				if !strings.Contains(got, want) {
					t.Errorf("Describe = %q, want it to contain %q", got, want)
				}
			}
		})
	}
	if _, err := Describe(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Describe of a missing binary succeeded")
	}
}

// A prebuilt binary for the licensed fixtures is used as is when it carries
// the licensetest tag and refused before any node starts when it does not
// (a release binary, a typo in the path).
func TestBuildLicensedPrebuilt(t *testing.T) {
	tagged := fakeKudurru(t, "-trimpath", "-tags=other,licensetest")
	t.Setenv(EnvLicensedBinary, tagged)
	got, err := BuildLicensed(t.TempDir())
	if err != nil || got != tagged {
		t.Fatalf("BuildLicensed with a tagged prebuilt binary = %q, %v", got, err)
	}
	for name, path := range map[string]string{
		"release build":  fakeKudurru(t, "-trimpath"),
		"other tag":      fakeKudurru(t, "-trimpath", "-tags=notlicensetest"),
		"missing binary": filepath.Join(t.TempDir(), "kudurru"),
	} {
		t.Setenv(EnvLicensedBinary, path)
		if got, err := BuildLicensed(t.TempDir()); err == nil || !strings.Contains(err.Error(), EnvLicensedBinary) {
			t.Errorf("%s: BuildLicensed = %q, %v; want an error naming %s", name, got, err, EnvLicensedBinary)
		}
	}
}

// KUDURRU_E2E_BINARY still names the ordinary binary and is not confused
// with the licensed one.
func TestBuildPrebuilt(t *testing.T) {
	t.Setenv(EnvLicensedBinary, "/nonexistent/licensed")
	t.Setenv(EnvBinary, "relative/kudurru")
	got, err := Build(t.TempDir())
	wd, _ := os.Getwd()
	if err != nil || got != filepath.Join(wd, "relative", "kudurru") {
		t.Fatalf("Build with %s = %q, %v", EnvBinary, got, err)
	}
}
