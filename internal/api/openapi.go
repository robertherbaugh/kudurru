package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// OpenAPI document files under api/ (make openapi). The 3.1 document is
// what /api/v1/openapi.json serves; the 3.0 downgrade feeds generators
// that do not read 3.1 yet (oapi-codegen for internal/apiclient).
const (
	OpenAPIFile   = "openapi.json"
	OpenAPI30File = "openapi-3.0.json"
)

// MarshalOpenAPI renders the OpenAPI document of every registered route
// as indented JSON with a trailing newline; v30 selects the OpenAPI 3.0
// downgrade.
func (s *Server) MarshalOpenAPI(v30 bool) ([]byte, error) {
	var (
		raw []byte
		err error
	)
	if v30 {
		raw, err = s.api.OpenAPI().Downgrade()
	} else {
		raw, err = json.Marshal(s.api.OpenAPI())
	}
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// OpenAPIDocument builds a server with no runtime dependencies and
// renders its OpenAPI document: the committed api/openapi.json (v30
// false) and api/openapi-3.0.json (v30 true). The document does not
// depend on the build version, the node or the configuration.
func OpenAPIDocument(v30 bool) ([]byte, error) {
	v, err := validator.New()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load("", nil)
	if err != nil {
		return nil, err
	}
	cfg.Listeners.API.PathPrefix = ""
	s, err := New(Options{
		Store: memory.New(), Validator: v, Config: cfg, NodeName: "openapi",
		Version: version.Info{Version: APIVersion}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return nil, fmt.Errorf("api: build server for the OpenAPI document: %w", err)
	}
	return s.MarshalOpenAPI(v30)
}
