package api

import (
	"log/slog"
	"net/http"
	"os"
	"testing"
)

func TestDebugPanic(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil)) })
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com."))
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com."))
	h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "viewer", nil)
}
