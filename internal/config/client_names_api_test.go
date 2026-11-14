package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func TestReversePTRActualAPIHostOnly(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	srv, err := api.New(api.Options{Config: cfg, Store: st, Validator: v, Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.Grant("admin", "admin")), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
		for _, key := range config.Schema {
			if !strings.HasPrefix(key.Path, "clientNames.reversePTR.") {
				continue
			}
			for _, value := range []any{key.Default, nil} {
				body, err := json.Marshal(map[string]any{"clientNames": map[string]any{"reversePTR": map[string]any{strings.TrimPrefix(key.Path, "clientNames.reversePTR."): value}}})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPatch, route, bytes.NewReader(body))
				req.Header.Set("Authorization", apitest.Bearer("admin"))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				var result struct {
					Error api.ErrorBody `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusBadRequest || result.Error.Key != api.KeyHostOnly {
					t.Fatal("host PTR authority remotely writable", route, key.Path, rec.Code, result.Error.Key)
				}
			}
		}
	}
}
