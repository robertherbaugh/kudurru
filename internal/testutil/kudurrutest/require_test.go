package kudurrutest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// probe is a testing.TB that records how a helper ended the test instead of
// ending the test that runs it.
type probe struct {
	testing.TB
	skipped, failed bool
	message         string
}

func (p *probe) Helper() {}

func (p *probe) Skip(args ...any) {
	p.skipped, p.message = true, fmt.Sprint(args...)
	runtime.Goexit()
}

func (p *probe) Fatalf(format string, args ...any) {
	p.failed, p.message = true, fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// outcome runs f on its own goroutine (Skip and Fatalf end the goroutine
// that calls them) and returns what it did to the test.
func outcome(t *testing.T, f func(testing.TB)) *probe {
	t.Helper()
	p := &probe{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(p)
	}()
	<-done
	return p
}

// fakeProgram writes an executable named name into a new directory and
// returns its path.
func fakeProgram(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: a test executable
		t.Fatal(err)
	}
	return path
}

// noBIND makes the host look like one without BIND 9: an empty PATH and no
// distribution directories.
func noBIND(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv(EnvNamed, "")
	t.Setenv(EnvRequireBIND, "")
	oldNamed, oldTools := namedDirs, bindToolDirs
	namedDirs, bindToolDirs = nil, nil
	t.Cleanup(func() { namedDirs, bindToolDirs = oldNamed, oldTools })
}

// A missing program skips the test; the require variable makes the same
// condition a failure that names the variable and the reason.
func TestUnavailableSkipsUnlessRequired(t *testing.T) {
	const require = "KUDURRU_REQUIRE_TEST_PROGRAM"
	t.Setenv(require, "")
	got := outcome(t, func(tb testing.TB) { Unavailable(tb, require, "%s is not installed", "program") })
	if !got.skipped || got.failed || got.message != "program is not installed" {
		t.Fatalf("without %s: skipped=%t failed=%t message=%q", require, got.skipped, got.failed, got.message)
	}
	t.Setenv(require, "1")
	got = outcome(t, func(tb testing.TB) { Unavailable(tb, require, "%s is not installed", "program") })
	if got.skipped || !got.failed || !strings.Contains(got.message, require) || !strings.Contains(got.message, "program is not installed") {
		t.Fatalf("with %s: skipped=%t failed=%t message=%q", require, got.skipped, got.failed, got.message)
	}
}

func TestNamedSkipsOrFailsWithoutBIND(t *testing.T) {
	noBIND(t)
	got := outcome(t, func(tb testing.TB) { Named(tb) })
	if !got.skipped || got.failed {
		t.Fatalf("no named: skipped=%t failed=%t message=%q", got.skipped, got.failed, got.message)
	}
	t.Setenv(EnvRequireBIND, "1")
	got = outcome(t, func(tb testing.TB) { Named(tb) })
	if got.skipped || !got.failed || !strings.Contains(got.message, EnvRequireBIND) {
		t.Fatalf("no named, required: skipped=%t failed=%t message=%q", got.skipped, got.failed, got.message)
	}
}

func TestNamedSearchOrder(t *testing.T) {
	noBIND(t)
	onPath, installed, explicit := fakeProgram(t, "named"), fakeProgram(t, "named"), fakeProgram(t, "named-9.18")
	var path string
	find := func() *probe { return outcome(t, func(tb testing.TB) { path = Named(tb) }) }

	namedDirs = []string{filepath.Dir(installed)}
	if got := find(); got.skipped || got.failed || path != installed {
		t.Fatalf("distribution directory: %q, skipped=%t failed=%t %q", path, got.skipped, got.failed, got.message)
	}
	t.Setenv("PATH", filepath.Dir(onPath))
	if got := find(); got.skipped || got.failed || path != onPath {
		t.Fatalf("PATH before the distribution directory: %q, %q", path, got.message)
	}
	t.Setenv(EnvNamed, explicit)
	if got := find(); got.skipped || got.failed || path != explicit {
		t.Fatalf("%s first: %q, %q", EnvNamed, path, got.message)
	}
	// An explicit binary that is not there is never replaced by another one,
	// with or without the require variable.
	missing := filepath.Join(t.TempDir(), "named")
	t.Setenv(EnvNamed, missing)
	if got := find(); !got.failed || got.skipped || !strings.Contains(got.message, missing) {
		t.Fatalf("missing %s: skipped=%t failed=%t message=%q", EnvNamed, got.skipped, got.failed, got.message)
	}
}

func TestBINDToolSkipsOrFailsWhenMissing(t *testing.T) {
	noBIND(t)
	tool := fakeProgram(t, "dnssec-keygen")
	var path string
	find := func(name string) *probe { return outcome(t, func(tb testing.TB) { path = BINDTool(tb, name) }) }

	bindToolDirs = []string{filepath.Dir(tool)}
	if got := find("dnssec-keygen"); got.skipped || got.failed || path != tool {
		t.Fatalf("installed tool: %q, skipped=%t failed=%t %q", path, got.skipped, got.failed, got.message)
	}
	if got := find("dnssec-signzone"); !got.skipped || got.failed || !strings.Contains(got.message, "dnssec-signzone") {
		t.Fatalf("missing tool: skipped=%t failed=%t message=%q", got.skipped, got.failed, got.message)
	}
	t.Setenv(EnvRequireBIND, "1")
	if got := find("dnssec-signzone"); got.skipped || !got.failed || !strings.Contains(got.message, EnvRequireBIND) {
		t.Fatalf("missing tool, required: skipped=%t failed=%t message=%q", got.skipped, got.failed, got.message)
	}
}
