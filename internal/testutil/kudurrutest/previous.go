package kudurrutest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// PreviousRelease is the release the upgrade tests start from: the only
// release this one supports upgrading from (D373).
const PreviousRelease = "0.1.0"

// previousTag is the git tag of PreviousRelease.
const previousTag = "v" + PreviousRelease

// EnvPreviousBinary names a kudurru binary of PreviousRelease to use instead
// of building one from the tag: the published release binary, downloaded and
// checked against the release checksums by whoever sets the variable.
const EnvPreviousBinary = "KUDURRU_E2E_PREVIOUS_BINARY"

// EnvRequirePreviousRelease turns the skip of an upgrade test without a
// previous-release binary into a failure (see EnvRequireBIND). The hosted
// e2e job sets it and fetches the tag.
const EnvRequirePreviousRelease = "KUDURRU_REQUIRE_PREVIOUS_RELEASE"

// errNoPreviousTag means the clone does not have the tag of PreviousRelease
// (a shallow clone, a fork without tags).
var errNoPreviousTag = errors.New("kudurrutest: the " + previousTag + " tag is not in this clone")

// previousBuild is the previous-release binary of this test binary: built
// (or named by KUDURRU_E2E_PREVIOUS_BINARY) once, removed by RemovePrevious.
var previousBuild struct {
	once          sync.Once
	dir, bin, log string
	err           error
}

// Previous returns the kudurru binary of PreviousRelease for an upgrade
// test, built once per test binary. The binary is what
// KUDURRU_E2E_PREVIOUS_BINARY names, or else a build of the release tag in
// this clone (BuildPrevious). Without the variable and without the tag the
// test skips, or fails when KUDURRU_REQUIRE_PREVIOUS_RELEASE is set. The
// test log names the binary (Describe). TestMain calls RemovePrevious.
func Previous(t testing.TB) string {
	t.Helper()
	b := &previousBuild
	b.once.Do(func() {
		if p := os.Getenv(EnvPreviousBinary); p != "" {
			b.bin, b.err = filepath.Abs(p)
		} else if b.dir, b.err = os.MkdirTemp("", "kudurru-e2e-previous-"); b.err == nil {
			b.bin, b.err = BuildPrevious(b.dir)
		}
		if b.err == nil {
			b.err = checkPrevious(b.bin)
		}
		if b.err == nil {
			b.log, b.err = Describe(b.bin)
		}
	})
	if errors.Is(b.err, errNoPreviousTag) {
		Unavailable(t, EnvRequirePreviousRelease, "%v: run `git fetch origin tag %s`, or set %s to a %s binary", b.err, previousTag, EnvPreviousBinary, PreviousRelease)
	}
	if b.err != nil {
		t.Fatalf("previous release binary: %v", b.err)
	}
	t.Logf("previous release binary %s", b.log)
	return b.bin
}

// RemovePrevious removes what Previous and PreviousProgram built.
func RemovePrevious() {
	if previousBuild.dir != "" {
		_ = os.RemoveAll(previousBuild.dir)
	}
	if previousPrograms.dir != "" {
		_ = os.RemoveAll(previousPrograms.dir)
	}
}

// BuildPrevious builds the kudurru binary of PreviousRelease into dir from
// the release tag of this clone and returns its path: the tree of the tag
// (git archive) is unpacked under dir, outside the working tree, built like
// a release (CGO_ENABLED=0, -trimpath, the version and commit of the tag)
// and removed again. Only the binary stays, for the caller to remove with
// dir. It is the released source, not the published file: a release binary
// also embeds the dashboard bundle, which no upgrade test uses.
func BuildPrevious(dir string) (string, error) {
	bin := filepath.Join(dir, "kudurru-"+PreviousRelease)
	return bin, buildPreviousTree(dir, func(ctx context.Context, src, commit string) error {
		const version = "github.com/robertherbaugh/kudurru/internal/version."
		return previousGoBuild(ctx, src, "./cmd/kudurru", bin, "-s -w -X "+version+"Version="+PreviousRelease+" -X "+version+"Commit="+commit)
	})
}

// previousPrograms are the programs PreviousProgram built, by name.
var previousPrograms struct {
	mu   sync.Mutex
	dir  string
	bins map[string]string
}

// PreviousProgram builds source, the text of a Go main package, against the
// packages of PreviousRelease and returns the program's path: a client of
// the previous release's own code (its internal listener client, say) for a
// mixed-version test. The program is built once per name and test binary,
// from the release tag of this clone (a published binary cannot stand in),
// and removed by RemovePrevious. Without the tag the test skips, or fails
// when KUDURRU_REQUIRE_PREVIOUS_RELEASE is set.
func PreviousProgram(t testing.TB, name, source string) string {
	t.Helper()
	p := &previousPrograms
	p.mu.Lock()
	defer p.mu.Unlock()
	if bin, ok := p.bins[name]; ok {
		return bin
	}
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
		t.Fatalf("previous program name %q", name)
	}
	if p.dir == "" {
		dir, err := os.MkdirTemp("", "kudurru-e2e-previous-programs-")
		if err != nil {
			t.Fatal(err)
		}
		p.dir, p.bins = dir, map[string]string{}
	}
	bin := filepath.Join(p.dir, name)
	err := buildPreviousTree(p.dir, func(ctx context.Context, src, _ string) error {
		pkg := filepath.Join(src, "cmd", "e2e-previous-"+name)
		if err := os.MkdirAll(pkg, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pkg, "main.go"), []byte(source), 0o600); err != nil {
			return err
		}
		return previousGoBuild(ctx, src, "./cmd/e2e-previous-"+name, bin, "-s -w")
	})
	if errors.Is(err, errNoPreviousTag) {
		Unavailable(t, EnvRequirePreviousRelease, "%v: run `git fetch origin tag %s`", err, previousTag)
	}
	if err != nil {
		t.Fatalf("previous release program %s: %v", name, err)
	}
	p.bins[name] = bin
	t.Logf("previous release program %s, built from %s", name, previousTag)
	return bin
}

// buildPreviousTree unpacks the tree of the release tag below dir, calls
// build with it and the short commit of the tag, and removes the tree.
func buildPreviousTree(dir string, build func(ctx context.Context, src, commit string) error) error {
	root, err := ModuleRoot()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	rev := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "refs/tags/"+previousTag+"^{commit}") //nolint:gosec // fixed arguments
	rev.Dir = root
	out, err := rev.Output()
	if err != nil {
		return errNoPreviousTag
	}
	commit := strings.TrimSpace(string(out))
	if len(commit) > 8 {
		commit = commit[:8]
	}
	src, err := os.MkdirTemp(dir, "src-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := gitArchive(ctx, root, previousTag, src); err != nil {
		return err
	}
	return build(ctx, src, commit)
}

// previousGoBuild builds pkg of the unpacked release tree src into bin.
func previousGoBuild(ctx context.Context, src, pkg, bin, ldflags string) error {
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, pkg) //nolint:gosec // fixed arguments
	build.Dir = src
	// The build of another checkout: none of this module's build settings.
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("kudurrutest: go build %s of %s: %w\n%s", pkg, previousTag, err, b)
	}
	return nil
}

// checkPrevious refuses a binary that does not report PreviousRelease, so
// that an upgrade test never passes on two builds of the same release.
func checkPrevious(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version") //nolint:gosec // the test names the binary
	cmd.Env = cleanEnv()
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("kudurrutest: %s version: %w", bin, err)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, "kudurru "+PreviousRelease+" ") {
		return fmt.Errorf("kudurrutest: %s is %q, not kudurru %s (%s)", bin, got, PreviousRelease, EnvPreviousBinary)
	}
	return nil
}

// gitArchive unpacks the tree of ref in the repository at root into dst.
func gitArchive(ctx context.Context, root, ref, dst string) error {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", ref) //nolint:gosec // fixed arguments
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("kudurrutest: git archive %s: %w", ref, err)
	}
	unpackErr := untar(stdout, dst)
	if unpackErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("kudurrutest: git archive %s: %w\n%s", ref, err, stderr.String())
	}
	return unpackErr
}

// untar writes the regular files and directories of a tar stream below dst.
// Names that would leave dst and entries of any other type are refused.
func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("kudurrutest: read archive: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // git archive records the commit in one
		}
		if !filepath.IsLocal(h.Name) {
			return fmt.Errorf("kudurrutest: archive entry %q leaves the directory", h.Name)
		}
		path := filepath.Join(dst, h.Name) //nolint:gosec // G305: filepath.IsLocal above refuses entries that leave dst
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			mode := os.FileMode(0o640)
			if h.Mode&0o100 != 0 {
				mode = 0o750
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // a path below dst
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr) //nolint:gosec // the repository's own tree
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			// The release tree has no links; one could redirect later entries.
			return fmt.Errorf("kudurrutest: archive entry %q has unsupported type %q", h.Name, h.Typeflag)
		}
	}
}
