package kudurrutest

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// versionScript is an executable that answers `version` with line.
func versionScript(t *testing.T, line string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kudurru")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+line+"'\n"), 0o700); err != nil { //nolint:gosec // G306: a test executable
		t.Fatal(err)
	}
	return path
}

// An upgrade test must start from the previous release: a binary that
// reports another version (the build under test, a typo in the variable) is
// refused before any node starts.
func TestCheckPreviousRequiresThePreviousRelease(t *testing.T) {
	if err := checkPrevious(versionScript(t, "kudurru "+PreviousRelease+" (89c59f89, go1.27.3, linux/amd64)")); err != nil {
		t.Fatalf("the previous release was refused: %v", err)
	}
	for _, line := range []string{"kudurru 0.2.0 (abc1234, go1.27.3, linux/amd64)", "kudurru " + PreviousRelease + "1 (x)", "kudurru dev (unknown)", ""} {
		err := checkPrevious(versionScript(t, line))
		if err == nil || !strings.Contains(err.Error(), EnvPreviousBinary) {
			t.Errorf("%q: checkPrevious = %v, want an error naming %s", line, err, EnvPreviousBinary)
		}
	}
	if err := checkPrevious(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing binary was accepted")
	}
}

// The release tree is unpacked below the directory it was given: files keep
// their content and executable bit, the commit header of git archive is
// skipped, and an entry that would leave the directory or is a link is
// refused.
func TestUntarStaysBelowTheDirectory(t *testing.T) {
	archive := func(entries ...tar.Header) *bytes.Buffer {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for _, h := range entries {
			body := ""
			if h.Typeflag == tar.TypeReg {
				body = "content of " + h.Name
				h.Size = int64(len(body))
			}
			if err := w.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return &b
	}
	dst := t.TempDir()
	if err := untar(archive(
		tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "89c59f89"}},
		tar.Header{Typeflag: tar.TypeDir, Name: "cmd/", Mode: 0o755},
		tar.Header{Typeflag: tar.TypeReg, Name: "cmd/kudurru/main.go", Mode: 0o644},
		tar.Header{Typeflag: tar.TypeReg, Name: "hack/run.sh", Mode: 0o755},
	), dst); err != nil {
		t.Fatal(err)
	}
	for name, executable := range map[string]bool{"cmd/kudurru/main.go": false, "hack/run.sh": true} {
		b, err := os.ReadFile(filepath.Join(dst, name)) //nolint:gosec // the file this test unpacked
		info, statErr := os.Stat(filepath.Join(dst, name))
		if err != nil || statErr != nil || string(b) != "content of "+name || (info.Mode()&0o100 != 0) != executable {
			t.Errorf("%s: %q, mode %v, %v %v", name, b, info.Mode(), err, statErr)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "pax_global_header")); err == nil {
		t.Error("the commit header was written as a file")
	}
	for name, h := range map[string]tar.Header{
		"parent directory": {Typeflag: tar.TypeReg, Name: "../outside", Mode: 0o644},
		"absolute path":    {Typeflag: tar.TypeReg, Name: "/tmp/outside", Mode: 0o644},
		"symbolic link":    {Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc"},
		"hard link":        {Typeflag: tar.TypeLink, Name: "link", Linkname: "cmd/kudurru/main.go"},
	} {
		parent := t.TempDir()
		if err := untar(archive(h), filepath.Join(parent, "src")); err == nil {
			t.Errorf("%s: unpacked", name)
		}
		if _, err := os.Stat(filepath.Join(parent, "outside")); err == nil {
			t.Errorf("%s: wrote outside the directory", name)
		}
	}
}
