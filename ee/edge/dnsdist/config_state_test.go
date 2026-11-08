//go:build unix

package dnsdist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

func TestManagedCachePolicyRetainedAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "nested", "config")
	first, err := acquireConfigState(path, true)
	if err != nil {
		t.Fatal(err)
	}
	id := first.bypassID
	if len(id) != 64 {
		t.Fatal("missing durable identity")
	}
	original, err := os.ReadFile(filepath.Join(path, managedBypassFile)) //nolint:gosec // fixed filename in this test's private directory
	if err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if refused, err := acquireConfigState(path, false); err == nil {
			_ = refused.close()
			t.Fatal("restart disabled retained bypass")
		}
		next, err := acquireConfigState(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if next.bypassID != id {
			t.Fatal("restart replaced managed deployment identity")
		}
		if err := next.close(); err != nil {
			t.Fatal(err)
		}
	}
	current, err := os.ReadFile(filepath.Join(path, managedBypassFile)) //nolint:gosec // same exact private fixture after restart
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("restart or refusal altered retained policy", err)
	}
}

func TestManagedCachePolicyRefusesDamagedOrIndirectStorage(t *testing.T) {
	valid := managedBypassHeader + strings.Repeat("ab", 32) + "\n"
	for _, tc := range []struct {
		name string
		body string
	}{
		{"interrupted-empty-write", ""},
		{"interrupted-header", managedBypassHeader},
		{"unknown-version", strings.Replace(valid, "/v1", "/v2", 1)},
		{"zero-identity", managedBypassHeader + strings.Repeat("0", 64) + "\n"},
		{"noncanonical-identity", managedBypassHeader + strings.Repeat("AB", 32) + "\n"},
		{"extra-data", valid + "x"},
		{"oversized", strings.Repeat("x", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, managedBypassFile)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{false, true} {
				if state, err := acquireConfigState(dir, enabled); err == nil {
					_ = state.close()
					t.Fatal("damaged retained policy admitted a supervisor", enabled)
				}
			}
			got, err := os.ReadFile(path) //nolint:gosec // retained-policy file created by this test
			if err != nil || string(got) != tc.body {
				t.Fatal("refusal repaired or removed evidence", err)
			}
		})
	}
	for _, kind := range []string{"directory", "symlink", "hardlink", "fifo", "public"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, managedBypassFile)
			source := filepath.Join(t.TempDir(), "original")
			if err := os.WriteFile(source, []byte(valid), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "symlink":
				err = os.Symlink(source, path)
			case "hardlink":
				err = os.Link(source, path)
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "public":
				err = os.WriteFile(path, []byte(valid), 0o600)
				if err == nil {
					err = os.Chmod(path, 0o644) //nolint:gosec // deliberately invalid retained-policy fixture
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{false, true} {
				if state, err := acquireConfigState(dir, enabled); err == nil {
					_ = state.close()
					t.Fatal("indirect/nonprivate policy admitted a supervisor", enabled)
				}
			}
			got, err := os.ReadFile(source) //nolint:gosec // unrelated original fixture created by this test
			if err != nil || string(got) != valid {
				t.Fatal("refusal changed unrelated file", err)
			}
		})
	}
}

func TestManagedCacheConfigDirectoryOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	first, err := acquireConfigState(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.close() }()
	// The alternate spelling resolves to the same inode; there is no removable
	// lock sidecar whose replacement can create a second cooperating owner.
	alias := path + "/../config"
	for _, enabled := range []bool{false, true} {
		if second, err := acquireConfigState(alias, enabled); err == nil {
			_ = second.close()
			t.Fatal("two supervisors owned the same configuration directory")
		}
	}
	if _, err := os.Stat(filepath.Join(path, managedBypassFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused contender changed static owner's policy", err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireConfigState(alias, true)
	if err != nil {
		t.Fatal("released directory could not enter managed mode", err)
	}
	if err := next.close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCacheConfigOwnershipOtherProcess(t *testing.T) {
	if path := os.Getenv("KUDURRU_TEST_EDGE_CONFIG_OWNER"); path != "" {
		state, err := acquireConfigState(path, false)
		if err == nil {
			_ = state.close()
			t.Fatal("child process bypassed configuration ownership")
		}
		return
	}
	path := t.TempDir()
	state, err := acquireConfigState(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.close() }()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestManagedCacheConfigOwnershipOtherProcess$", "-test.timeout=5s") //nolint:gosec // reexecutes only this test binary for an actual process lock check
	cmd.Env = append(os.Environ(), "KUDURRU_TEST_EDGE_CONFIG_OWNER="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("separate process lock check: %v\n%s", err, out)
	}
}

func TestManagedCacheSupervisorRetainsPolicyBeforeChild(t *testing.T) {
	failure := errors.New("original credential preparation refused")
	h := newHarness(t, func(c *Config) {
		c.Options.ManagedCacheBypass = true
		c.NewCredentials = func() (edge.Credentials, error) { return edge.Credentials{}, failure }
	})
	select {
	case err := <-h.done:
		h.cancel()
		h.cancel = nil
		if !errors.Is(err, failure) {
			t.Fatal("lost original preparation error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not finish")
	}
	if state, err := acquireConfigState(h.dir, false); err == nil {
		_ = state.close()
		t.Fatal("failed child preparation erased retained bypass")
	}
	state, err := acquireConfigState(h.dir, true)
	if err != nil {
		t.Fatal("failed preparation retained directory ownership", err)
	}
	if err := state.close(); err != nil {
		t.Fatal(err)
	}
	h.sup.mu.Lock()
	installed := h.sup.managedBypassInstalledLocked()
	child := h.sup.child
	h.sup.mu.Unlock()
	if installed || child != nil {
		t.Fatal("durable file substituted for actual installed child")
	}
}

func TestManagedCacheSupervisorOwnershipAndShutdown(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Options.ManagedCacheBypass = true })
	h.waitFor("actual supervised child acknowledged", h.ready)
	response := httptest.NewRecorder()
	h.sup.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/status", nil))
	var body struct {
		Identity  string `json:"managedCacheBypassIdentity"`
		Installed bool   `json:"managedCacheBypassInstalled"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.Installed || len(body.Identity) != 64 {
		t.Fatal("status lacks original retained identity and separate installed evidence", err)
	}
	contenderConfig := h.sup.cfg
	contenderConfig.Registerer, contenderConfig.Gatherer = nil, nil
	contenderConfig.NewCredentials = func() (edge.Credentials, error) {
		t.Error("contender reached credential/render/child preparation")
		return edge.Credentials{}, errors.New("must not enter")
	}
	contender, err := New(contenderConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := contender.Run(t.Context()); err == nil {
		t.Fatal("live supervisor admitted a second directory owner")
	}
	h.stop()
	h.sup.mu.Lock()
	child, installed := h.sup.child, h.sup.managedBypassInstalledLocked()
	h.sup.mu.Unlock()
	if child.Running() || installed {
		t.Fatal("shutdown retained child or installed evidence")
	}
	state, err := acquireConfigState(h.dir, true)
	if err != nil {
		t.Fatal("joined shutdown failed to release ownership", err)
	}
	defer func() { _ = state.close() }()
	if state.bypassID != body.Identity {
		t.Fatal("shutdown replaced persistent identity")
	}
}
