package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// In particular, removal while the process is stopped must cross the security
// owner on the FIRST Load, before the complete serving runtime is constructed.
func TestRunServeSQLiteSourceBindingStartupAndRestart(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "directory"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(license.EnvKey, "")
			t.Setenv(license.EnvKeyFile, "")
			cfg, _ := mountedTestConfig(t)
			cfg.Auth.IdentityProviders = nil
			sourceDir := t.TempDir()
			cfg.ObjectsDir = sourceDir
			cfg.Source = filepath.Join(t.TempDir(), "node.yaml")
			binding := roleBindingManifest("source-operator", "operator", "corp:alice")
			binding.Spec.Scope.Kind = "cluster"
			put := func(present bool) {
				t.Helper()
				if !inline {
					sourceBindingFile(t, sourceDir, present, "operator")
					return
				}
				cfg.ObjectsDir = ""
				next := *cfg
				next.Objects = nil
				if present {
					raw, err := json.Marshal(binding)
					if err != nil {
						t.Fatal("binding encode")
					}
					var obj map[string]any
					if json.Unmarshal(raw, &obj) != nil {
						t.Fatal("binding decode")
					}
					next.Objects = []map[string]any{obj}
				}
				raw, err := json.Marshal(&next)
				if err != nil {
					t.Fatal("node source encode")
				}
				if err := os.WriteFile(cfg.Source, raw, 0o600); err != nil {
					t.Fatal("node source write")
				}
			}
			scheme, err := runtimekinds.NewScheme()
			if err != nil {
				t.Fatal("source scheme")
			}
			v, err := validator.New()
			if err != nil {
				t.Fatal("source validator")
			}
			var removedFloor time.Time
			var initialUID string
			for phase := range 3 {
				put(phase != 1)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- runServe(ctx, cfg, "") }()
				stopped := false
				stop := func() {
					if stopped {
						return
					}
					stopped = true
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error("source mounted serve ended")
						}
					case <-time.After(15 * time.Second):
						t.Error("source mounted serve failed to join")
					}
				}
				t.Cleanup(stop)
				deadline := time.Now().Add(15 * time.Second)
				// Read-only readiness observation is independent of mutation admission.
				// This helper performs no login or mutating HTTP request and no retries
				// of a refused command. API binding occurs after the first source Load.
				for offlineHTTPStatus(cfg.Listeners.API.Address, "/api/v1/auth/status") != http.StatusOK {
					if !time.Now().Before(deadline) {
						stop()
						t.Fatal("source mounted API did not start")
					}
					select {
					case <-time.After(20 * time.Millisecond):
					case <-t.Context().Done():
						stop()
						t.Fatal("source startup canceled")
					}
				}
				stop()
				st, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: privateStoreKinds(), Identity: cfg.Node.Name, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
				if err != nil {
					t.Fatal("source restart inspection open")
				}
				current := sourceBinding(t, st)
				floors := localUsersState(t, st).GrantFloors
				floor := time.Time{}
				for _, item := range floors {
					if item.Grant == "operator" {
						floor = item.ValidAfter
					}
				}
				if err := st.Close(); err != nil {
					t.Fatal("source restart inspection close")
				}
				switch phase {
				case 0:
					if current == nil || !floor.IsZero() {
						t.Fatal("first source Load did not create binding without a floor")
					}
					initialUID = string(current.UID)
				case 1:
					if current != nil || floor.IsZero() {
						t.Fatal("offline source removal bypassed first-Load security effect")
					}
					removedFloor = floor
				case 2:
					if current == nil || string(current.UID) == initialUID || !floor.Equal(removedFloor) {
						t.Fatal("startup regrant reused binding identity or erased durable floor")
					}
				}
			}
		})
	}
}
