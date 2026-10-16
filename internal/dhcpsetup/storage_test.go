package dhcpsetup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func setupDiskBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // Test paths are created inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSetupStorageFailureAtomicity(t *testing.T) {
	for _, fault := range []string{"file-sync", "rename", "directory-sync", "cancel-before-rename", "cancel-after-rename"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			m := setupTestOpen(t, dir, config.DHCP{}, nil)
			before := setupTestSave(t, m, false, []config.DHCPScope{setupTestScope()}, false)
			original := setupDiskBytes(t, m.path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("injected storage failure")
			m.syncFile = func(f *os.File) error {
				stat, err := f.Stat()
				if err != nil {
					return err
				}
				if fault == "file-sync" && !stat.IsDir() || fault == "directory-sync" && stat.IsDir() {
					return injected
				}
				if fault == "cancel-before-rename" && !stat.IsDir() {
					cancel()
				}
				return f.Sync()
			}
			m.rename = func(oldpath, newpath string) error {
				if fault == "rename" {
					return injected
				}
				if err := os.Rename(oldpath, newpath); err != nil {
					return err
				}
				if fault == "cancel-after-rename" {
					cancel()
				}
				return nil
			}
			next, err := m.Save(ctx, before.Revision, true, before.Scopes, false, before.ScopeRevisions)
			if fault == "cancel-after-rename" {
				if err != nil || next.Revision != before.Revision+1 || !next.Enabled {
					t.Fatalf("completed durable replacement reported cancellation: %+v %v", next, err)
				}
			} else {
				if err == nil {
					t.Fatal("failed replacement returned success")
				}
				if !reflect.DeepEqual(before, m.State()) {
					t.Fatal("failed save published memory state")
				}
				if fault != "directory-sync" && !bytes.Equal(original, setupDiskBytes(t, m.path)) {
					t.Fatal("failure before rename changed saved bytes")
				}
			}
			if fault == "directory-sync" {
				m.syncFile = (*os.File).Sync
				if _, err := m.Save(context.Background(), before.Revision, false, before.Scopes, false, before.ScopeRevisions); err == nil || !strings.Contains(err.Error(), "restart") {
					t.Fatalf("uncertain replacement allowed next save: %v", err)
				}
			}
			entries, err := filepath.Glob(filepath.Join(dir, ".dhcp-settings-*"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files leaked: %v %v", entries, err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := setupTestOpen(t, dir, config.DHCP{}, nil)
			got := setupTestRead(t, reopened)
			replaced := fault == "directory-sync" || fault == "cancel-after-rename"
			if got.Enabled != replaced || got.Revision != before.Revision+boolRevision(replaced) {
				t.Fatalf("reopen chose incorrect on-disk outcome: %+v", got)
			}
		})
	}
}

func boolRevision(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func TestSetupRejectsCorruptRegistryWithoutReplacingIt(t *testing.T) {
	for name, data := range map[string]string{
		"truncated": "{",
		"version":   `{"version":2,"revision":1}`,
		"revision":  `{"version":1,"revision":0}`,
		"unknown":   `{"version":1,"revision":1,"typo":true}`,
		"trailing":  `{"version":1,"revision":1} {}`,
		"plans":     `{"version":1,"revision":1,"scopes":[],"plans":[{"name":"default","token":"seed","expectedRevision":0}]}`,
		"oversize":  strings.Repeat(" ", maxSettingsBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, settingsFilename)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if m, err := Open(dir, config.DHCP{}, nil); !errors.Is(err, ErrInvalidConfiguration) {
				if m != nil {
					_ = m.Close()
				}
				t.Fatalf("corrupt registry opened: %v", err)
			}
			if !bytes.Equal(setupDiskBytes(t, path), []byte(data)) {
				t.Fatal("corrupt registry bytes overwritten")
			}
			// A failed Open must release its lifetime lock for repair.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			_ = setupTestOpen(t, dir, config.DHCP{}, nil)
		})
	}
}

func TestSetupPrivateFilesAndLifetimeLock(t *testing.T) {
	dir := t.TempDir()
	m := setupTestOpen(t, dir, config.DHCP{}, nil)
	if other, err := Open(dir, config.DHCP{}, nil); err == nil {
		_ = other.Close()
		t.Fatal("disabled setup allowed another owner")
	}
	alias := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(alias, config.DHCP{}, nil); err == nil {
		_ = other.Close()
		t.Fatal("directory alias bypassed lock")
	}
	setupTestSave(t, m, false, nil, false)
	// An empty fresh no-op need not create a registry; create a real scope.
	setupTestSave(t, m, false, []config.DHCPScope{setupTestScope()}, false)
	for _, filename := range []string{settingsFilename, lockFilename} {
		stat, err := os.Stat(filepath.Join(dir, filename))
		if err != nil || stat.Mode().Perm() != 0o600 {
			t.Fatalf("private %s mode: %v %v", filename, stat, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	_ = setupTestOpen(t, alias, config.DHCP{}, nil)
}
