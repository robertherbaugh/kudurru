package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// QuerylogDataDir returns the host-selected directory without moving existing
// files. Empty overrides preserve the original node.dataDir layout.
func (c *Config) QuerylogDataDir() string {
	if c.Node.QuerylogDir != "" {
		return c.Node.QuerylogDir
	}
	return c.Node.DataDir
}

// LocalDataDir holds the node's last-good snapshots, transferred data and cache.
func (c *Config) LocalDataDir() string {
	if c.Node.LocalDir != "" {
		return c.Node.LocalDir
	}
	return c.Node.DataDir
}

// ValidateStorageDirs checks syntax without touching the filesystem. Serving
// additionally checks the complete effective layout before creating any files.
func (c *Config) ValidateStorageDirs() error {
	for _, item := range []struct{ key, value string }{
		{"node.querylogDir", c.Node.QuerylogDir}, {"node.localDir", c.Node.LocalDir},
	} {
		if item.value != "" && (len(item.value) > 4096 || !filepath.IsAbs(item.value) || filepath.Clean(item.value) != item.value || strings.ContainsAny(item.value, "\x00?#")) {
			return fmt.Errorf("%s: must be empty or a clean absolute directory path without URI parameters", item.key)
		}
	}
	return nil
}
