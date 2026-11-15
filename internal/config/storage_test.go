package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func TestStorageDirectoryDefaultsAndEnvironment(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.LocalDir != "" || cfg.Node.QuerylogDir != "" || cfg.LocalDataDir() != cfg.Node.DataDir || cfg.QuerylogDataDir() != cfg.Node.DataDir {
		t.Fatal("default database placement changed")
	}
	local, query := filepath.Join(t.TempDir(), "local"), filepath.Join(t.TempDir(), "query")
	cfg, err = config.Load("", []string{"KUDURRU_NODE__LOCALDIR=" + local, "KUDURRU_NODE__QUERYLOGDIR=" + query})
	if err != nil || cfg.LocalDataDir() != local || cfg.QuerylogDataDir() != query {
		t.Fatal("host environment selection failed", err)
	}
	for _, key := range []string{"node.localDir", "node.querylogDir"} {
		meta, ok := config.Lookup(key)
		if !ok || !meta.HostOnly || !meta.RestartRequired || meta.Scope != config.ScopeNode || config.PublicClusterKey(meta) {
			t.Fatal("storage directory must be a host-only restart setting", key)
		}
	}
	for _, value := range []string{"relative/path", "/tmp/../elsewhere", "/tmp/db?mode=memory", "/tmp/db#fragment", "/tmp/db\x00"} {
		if _, err := config.Load("", []string{"KUDURRU_NODE__LOCALDIR=" + value}); err == nil {
			t.Errorf("accepted invalid directory syntax %q", value)
		}
	}
}

func TestStorageDirectoriesRefusedByPublicSettings(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Options{Config: cfg, Store: st, Validator: v,
		Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.Grant("admin", "admin")), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
		for _, key := range []string{"localDir", "querylogDir"} {
			body, _ := json.Marshal(map[string]any{"node": map[string]any{key: filepath.Join(t.TempDir(), "new")}})
			req := httptest.NewRequest(http.MethodPatch, route, bytes.NewReader(body))
			req.Header.Set("Authorization", apitest.Bearer("admin"))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			var result struct{ Error api.ErrorBody }
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadRequest || result.Error.Key != api.KeyHostOnly {
				t.Fatalf("%s accepted host storage change: status=%d key=%s", route, w.Code, result.Error.Key)
			}
		}
	}
}
