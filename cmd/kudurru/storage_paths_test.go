package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestStoragePreflightRefusesBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  func(*testing.T, *config.Config)
	}{
		{"relative", func(_ *testing.T, c *config.Config) { c.Node.QuerylogDir = "relative" }},
		{"dangling-directory-symlink", func(t *testing.T, c *config.Config) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), c.Node.QuerylogDir); err != nil {
				t.Fatal(err)
			}
		}},
		{"non-directory", func(t *testing.T, c *config.Config) { writeStorageFixture(t, c.Node.QuerylogDir, 0o600) }},
		{"unreadable", func(t *testing.T, c *config.Config) { mkdirStorageFixture(t, c.Node.QuerylogDir, 0o300) }},
		{"unwritable", func(t *testing.T, c *config.Config) { mkdirStorageFixture(t, c.Node.QuerylogDir, 0o500) }},
		{"unsearchable", func(t *testing.T, c *config.Config) { mkdirStorageFixture(t, c.Node.QuerylogDir, 0o600) }},
		{"database-under-other-directory", func(_ *testing.T, c *config.Config) {
			c.Node.LocalDir = filepath.Join(c.Node.QuerylogDir, "querylog.db-wal", "nested")
		}},
		{"readonly-database", func(t *testing.T, c *config.Config) {
			mkdirStorageFixture(t, c.Node.QuerylogDir, 0o750)
			writeStorageFixture(t, filepath.Join(c.Node.QuerylogDir, "querylog.db"), 0o400)
		}},
		{"reserved-state-name", func(_ *testing.T, c *config.Config) { c.Node.QuerylogDir = filepath.Join(c.Node.DataDir, "state.db") }},
		{"reserved-dhcp-settings", func(_ *testing.T, c *config.Config) {
			c.Node.LocalDir = filepath.Join(c.Node.DataDir, "dhcp-settings.json")
		}},
		{"reserved-dhcp-claims", func(_ *testing.T, c *config.Config) {
			c.Node.QuerylogDir = filepath.Join(c.Node.DataDir, ".dhcp-dns-claims.sqlite-wal")
		}},
		{"configured-settings-file", func(_ *testing.T, c *config.Config) {
			c.SettingsFile = filepath.Join(c.Node.QuerylogDir, "querylog.db")
		}},
		{"configured-admin-socket", func(_ *testing.T, c *config.Config) { c.Auth.AdminSocket.Path = c.Node.LocalDir }},
		{"configured-tsig-file", func(_ *testing.T, c *config.Config) {
			c.TSIGKeys = []config.TSIGKey{{SecretFile: filepath.Join(c.Node.QuerylogDir, "querylog.db-wal")}}
		}},
		{"reserved-keys", func(_ *testing.T, c *config.Config) {
			c.Node.LocalDir = filepath.Join(c.Node.DataDir, "keys", "nested")
		}},
		{"symlink-database", func(t *testing.T, c *config.Config) {
			mkdirStorageFixture(t, c.Node.QuerylogDir, 0o750)
			target := filepath.Join(t.TempDir(), "protected")
			writeStorageFixture(t, target, 0o600)
			if err := os.Symlink(target, filepath.Join(c.Node.QuerylogDir, "querylog.db")); err != nil {
				t.Fatal(err)
			}
		}},
		{"sidecar-directory", func(t *testing.T, c *config.Config) {
			mkdirStorageFixture(t, filepath.Join(c.Node.QuerylogDir, "querylog.db-wal"), 0o750)
		}},
		{"sidecar-symlink", func(t *testing.T, c *config.Config) {
			mkdirStorageFixture(t, c.Node.QuerylogDir, 0o750)
			if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), filepath.Join(c.Node.QuerylogDir, "querylog.db-shm")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink-protected-file", func(t *testing.T, c *config.Config) {
			if runtime.GOOS == "windows" {
				t.Skip("Unix link-count protection")
			}
			mkdirStorageFixture(t, c.Node.QuerylogDir, 0o750)
			protected := filepath.Join(t.TempDir(), "settings")
			writeStorageFixture(t, protected, 0o600)
			if err := os.Link(protected, filepath.Join(c.Node.QuerylogDir, "querylog.db")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg, err := config.Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Node.DataDir = filepath.Join(root, "untouched-data")
			cfg.Node.LocalDir = filepath.Join(root, "untouched-local")
			cfg.Node.QuerylogDir = filepath.Join(root, "query")
			tc.bad(t, cfg)
			// The actual serving entry must reject before state.db/bootstrap or
			// even the otherwise-valid local directory is created.
			if err := runServe(t.Context(), cfg, ""); err == nil {
				t.Fatal("invalid storage layout started")
			}
			if _, err := os.Lstat(cfg.Node.DataDir); !os.IsNotExist(err) {
				t.Fatal("invalid layout mutated node data directory", err)
			}
			if cfg.Node.LocalDir == filepath.Join(root, "untouched-local") {
				if _, err := os.Lstat(cfg.Node.LocalDir); !os.IsNotExist(err) {
					t.Fatal("invalid layout created local directory", err)
				}
			}
		})
	}
}

func TestStoragePreflightResolvesDirectoryAliasesAndKeepsModes(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	mkdirStorageFixture(t, actual, 0o700)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir, cfg.Node.LocalDir, cfg.Node.QuerylogDir = alias, actual, alias
	if err := prepareStoragePaths(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(actual)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("existing directory permissions changed", err)
	}
	cfg.Node.LocalDir = filepath.Join(alias, "keys")
	if err := prepareStoragePaths(cfg); err == nil {
		t.Fatal("alias bypassed reserved directory check")
	}
}

func writeStorageFixture(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("protected fixture"), mode); err != nil {
		t.Fatal(err)
	}
}

func mkdirStorageFixture(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o750) }) //nolint:gosec // Restore search permission on a test directory for cleanup.
}

func TestStoragePreflightLeavesExistingStateUntouched(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	state := filepath.Join(cfg.Node.DataDir, "state.db")
	writeStorageFixture(t, state, 0o600)
	before, err := os.ReadFile(state) //nolint:gosec // Fixed fixture filename under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.QuerylogDir = filepath.Join(t.TempDir(), "not-directory")
	writeStorageFixture(t, cfg.Node.QuerylogDir, 0o600)
	if err := runServe(t.Context(), cfg, ""); err == nil {
		t.Fatal("invalid target accepted")
	}
	after, err := os.ReadFile(state) //nolint:gosec // Fixed fixture filename under t.TempDir.
	if err != nil || string(after) != string(before) {
		t.Fatal("preflight changed existing state", err)
	}
	entries, err := os.ReadDir(cfg.Node.DataDir)
	if err != nil || len(entries) != 1 {
		t.Fatal("preflight created product files", err)
	}
}
