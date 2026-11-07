package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNodeLogOverlayOriginalHostEnvironmentAndMerge(t *testing.T) {
	for _, tc := range []struct {
		name, file    string
		env           []string
		level, format string
		locked        bool
	}{
		{name: "defaults", level: "debug", format: "text"},
		{name: "host-leaf", file: "log: {level: warn}\n", level: "warn", format: "text", locked: true},
		{name: "explicit-default", file: "log: {level: info, format: json}\n", level: "info", format: "json", locked: true},
		{name: "environment", env: []string{"KUDURRU_LOG__LEVEL=error", "KUDURRU_LOG__FORMAT=json"}, level: "error", format: "json"},
		{name: "environment-over-host", file: "log: {level: warn}\n", env: []string{"KUDURRU_LOG__LEVEL=error"}, level: "error", format: "text", locked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.file != "" {
				path = filepath.Join(t.TempDir(), "node.yaml")
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			host, err := Load(path, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			baseline := host.Log
			level, format := "debug", "text"
			next, err := WithNodeLogOverlay(host, &level, &format)
			if err != nil || next.Log.Level != tc.level || next.Log.Format != tc.format {
				t.Fatal("overlay/source priority mismatch", err, next.Log)
			}
			if host.Log != baseline || NodeLogLocked(next, "log.level") != tc.locked {
				t.Fatal("overlay mutated/lost original source")
			}
			fresh, err := Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			merged, err := MergeCluster(next, fresh)
			if err != nil || merged.Log != next.Log || !reflect.DeepEqual(merged.NodeLogSource, next.NodeLogSource) {
				t.Fatal("cluster publication lost local baseline/locks", err)
			}
			restored, err := WithNodeLogOverlay(merged, nil, nil)
			if err != nil || restored.Log != baseline {
				t.Fatal("reset failed to restore original host baseline", err)
			}
			if len(merged.NodeLogSource.FileLocked) > 0 {
				merged.NodeLogSource.FileLocked[0] = "changed"
				if next.NodeLogSource.FileLocked[0] == "changed" {
					t.Fatal("source metadata aliases successor")
				}
			}
		})
	}
}
