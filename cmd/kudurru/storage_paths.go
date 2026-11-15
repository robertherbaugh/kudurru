package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// resolveStorageDir resolves existing ancestors without creating anything.
func resolveStorageDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return parent, nil
	}
	if !os.IsNotExist(err) || filepath.Dir(absolute) == absolute {
		return "", err
	}
	// An existing dangling symlink is not a missing directory. Otherwise
	// MkdirAll would reject it only after creating other selected targets.
	if _, linkErr := os.Lstat(absolute); linkErr == nil {
		return "", fmt.Errorf("existing path cannot be resolved: %w", err)
	} else if !os.IsNotExist(linkErr) {
		return "", linkErr
	}
	parent, err = resolveStorageDir(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

// prepareStoragePaths checks every target before any mkdir or database open.
// Directories are operator-controlled across this preflight and startup; this
// does not promise atomicity against concurrent hostile filesystem replacement.
func prepareStoragePaths(cfg *config.Config) error {
	if err := cfg.ValidateStorageDirs(); err != nil {
		return err
	}
	if cfg.Node.DataDir == "" {
		return fmt.Errorf("node.dataDir: directory is required")
	}
	dirs := []struct{ key, path, file string }{
		{"node.dataDir", cfg.Node.DataDir, sqlite.FileName},
		{"node.localDir", cfg.LocalDataDir(), localdb.FileName},
		{"node.querylogDir", cfg.QuerylogDataDir(), sqlitelog.FileName},
	}
	for i := range dirs {
		resolved, err := resolveStorageDir(dirs[i].path)
		if err != nil {
			return fmt.Errorf("%s: %w", dirs[i].key, err)
		}
		dirs[i].path = resolved
	}
	// A new directory must not occupy another product file/directory even
	// before that component has created it. Resolve aliases before comparing.
	for _, dir := range dirs[1:] {
		rel, err := filepath.Rel(dirs[0].path, dir.path)
		if err != nil {
			// Different Windows volumes have no parent/child relationship.
			continue
		}
		first := strings.Split(rel, string(filepath.Separator))[0]
		if first != "." && first != ".." && (strings.HasSuffix(first, ".db") || strings.Contains(first, ".db-") || strings.HasSuffix(first, ".db.lock") || first == "keys" || first == "lists" || first == "catalogue" || first == "dhcp-raft" || first == "dhcp-kube" || first == "settings.yaml" || first == "license.key" || first == "dhcp-settings.json" || first == "dhcp-settings.lock" || first == "dhcp.lock" || strings.HasPrefix(first, ".dhcp-dns-claims.sqlite") || strings.HasPrefix(first, "ui-settings-")) {
			return fmt.Errorf("%s: directory collides with reserved node data", dir.key)
		}
	}
	// A selected directory must not become another selected database or
	// sidecar, including when neither is beneath node.dataDir.
	for _, target := range dirs {
		for _, other := range dirs {
			for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
				leaf := filepath.Join(other.path, other.file) + suffix
				rel, err := filepath.Rel(leaf, target.path)
				if err != nil {
					continue // Distinct volumes cannot contain each other.
				}
				if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
					return fmt.Errorf("%s: directory collides with a selected database or sidecar", target.key)
				}
			}
		}
	}
	// Also protect explicitly configured product files outside node.dataDir.
	// This examines only named paths, never directory contents or credentials.
	protected := []string{cfg.Source, cfg.SettingsFile, cfg.Auth.AdminSocket.Path}
	for _, key := range cfg.TSIGKeys {
		protected = append(protected, key.SecretFile)
	}
	for _, path := range protected {
		if path == "" {
			continue
		}
		resolved, err := resolveStorageDir(path)
		if err != nil {
			return fmt.Errorf("storage: resolve configured product path: %w", err)
		}
		for _, dir := range dirs {
			rel, err := filepath.Rel(resolved, dir.path)
			if err != nil {
				continue // Distinct volumes cannot contain each other.
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return fmt.Errorf("%s: directory collides with a configured product file", dir.key)
			}
			for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
				if filepath.Join(dir.path, dir.file)+suffix == resolved {
					return fmt.Errorf("%s: database or sidecar collides with a configured product file", dir.key)
				}
			}
		}
	}
	var leaves []os.FileInfo
	for _, dir := range dirs {
		// Check the nearest existing directory too, before creating missing
		// parents. No chmod or probe-file creation changes existing permissions.
		for parent := dir.path; ; parent = filepath.Dir(parent) {
			info, err := os.Stat(parent)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: %w", dir.key, err)
			}
			if !info.IsDir() || info.Mode().Perm()&0o444 == 0 || info.Mode().Perm()&0o222 == 0 || info.Mode().Perm()&0o111 == 0 {
				return fmt.Errorf("%s: existing directory must be readable, writable and searchable", dir.key)
			}
			if err := storageAccess(parent, info); err != nil {
				return fmt.Errorf("%s: directory access: %w", dir.key, err)
			}
			break
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
			path := filepath.Join(dir.path, dir.file) + suffix
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: %w", dir.key, err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o444 == 0 || info.Mode().Perm()&0o222 == 0 {
				return fmt.Errorf("%s: database and sidecars must be regular readable/writable files, without symlinks", dir.key)
			}
			if err := storageAccess(path, info); err != nil {
				return fmt.Errorf("%s: file access: %w", dir.key, err)
			}
			for _, previous := range leaves {
				if os.SameFile(info, previous) {
					return fmt.Errorf("%s: database or sidecar aliases another product file", dir.key)
				}
			}
			leaves = append(leaves, info)
		}
	}
	// Creation begins only after the complete layout has passed inspection.
	for _, dir := range dirs {
		if err := os.MkdirAll(dir.path, 0o750); err != nil {
			return fmt.Errorf("%s: create directory: %w", dir.key, err)
		}
	}
	return nil
}
