package kudurrutest

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// EnvLicensedBinary names a prebuilt kudurru binary built with
// -tags licensetest, used by the licensed fixtures instead of building one
// (BuildLicensed).
const EnvLicensedBinary = "KUDURRU_E2E_LICENSED_BINARY"

// licenseTestTag is the build tag that makes a binary also trust the
// throwaway license key of internal/license/testdata
// (internal/license/testkey.go). A release binary never carries it.
const licenseTestTag = "licensetest"

// BuildLicensed is Build for the fixtures that need a license key (more than
// one node, the audit trail): it compiles ./cmd/kudurru with
// -tags licensetest into dir, so the binary trusts the test key, and returns
// the binary path. KUDURRU_E2E_LICENSED_BINARY short-circuits the build with
// a prebuilt binary (the release build flags plus the tag); a binary without
// the tag is refused here, because every node started from it would refuse
// the test key later.
func BuildLicensed(dir string) (string, error) {
	p := os.Getenv(EnvLicensedBinary)
	if p == "" {
		return goBuild(filepath.Join(dir, "kudurru-"+licenseTestTag), "-tags="+licenseTestTag)
	}
	path, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("kudurrutest: %s=%s: %w", EnvLicensedBinary, p, err)
	}
	if !slices.Contains(strings.Split(buildSetting(info, "-tags"), ","), licenseTestTag) {
		return "", fmt.Errorf("kudurrutest: %s=%s was not built with -tags %s: it does not trust the test license key", EnvLicensedBinary, p, licenseTestTag)
	}
	return path, nil
}

// goBuild compiles ./cmd/kudurru to out (CGO_ENABLED=0, as shipped).
func goBuild(out string, flags ...string) (string, error) {
	root, err := ModuleRoot()
	if err != nil {
		return "", err
	}
	args := append([]string{"build", "-trimpath"}, flags...)
	cmd := exec.Command("go", append(args, "-o", out, "./cmd/kudurru")...) //nolint:gosec // fixed arguments
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("kudurrutest: go build: %w\n%s", err, b)
	}
	return out, nil
}

// Describe identifies the kudurru binary at path in one line for the test
// log, so that a recorded run names the artifact it exercised: what
// `kudurru version` prints (version and commit of a release build), the
// SHA-256 of the file, and the build tags, cgo setting and commit that the
// Go toolchain recorded in the binary.
func Describe(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version") //nolint:gosec // the test names the binary
	cmd.Env = cleanEnv()
	version, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("kudurrutest: %s version: %w", path, err)
	}
	f, err := os.Open(path) //nolint:gosec // the test names the binary
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("kudurrutest: %s: %w", path, err)
	}
	tags := buildSetting(info, "-tags")
	if tags == "" {
		tags = "none"
	}
	line := fmt.Sprintf("%s: %s; sha256 %s; tags %s; CGO_ENABLED=%s", path, strings.TrimSpace(string(version)),
		hex.EncodeToString(sum.Sum(nil)), tags, buildSetting(info, "CGO_ENABLED"))
	if rev := buildSetting(info, "vcs.revision"); rev != "" {
		line += "; built from commit " + rev
		if buildSetting(info, "vcs.modified") == "true" {
			line += " with uncommitted changes"
		}
	}
	return line, nil
}

func buildSetting(info *buildinfo.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
