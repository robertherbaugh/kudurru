package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/apiclient/openapi"
)

// This crosses every documented operation, including operations the CLI has no
// dedicated verb for. It proves coverage against the schema, correct generated
// path construction, and no fallback/retry after a generated transport error.
// Actual-router CLI and SQLite auth conformance live in cmd/kudurru.
func TestGeneratedTransportEveryOpenAPIOperation(t *testing.T) {
	schema, err := os.ReadFile("../../api/openapi-3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(schema, &spec) != nil {
		t.Fatal("decode operation inventory")
	}
	for path, item := range spec.Paths {
		for verb := range item {
			switch verb {
			case "get", "put", "post", "delete", "options", "head", "patch", "trace":
			default:
				continue
			}
			t.Run(verb+" "+path, func(t *testing.T) {
				parts := strings.Split(path, "/")
				for i, part := range parts {
					if strings.HasPrefix(part, "{") {
						parts[i] = Seg("fixture/a b%")
						if part == "{id}" && path == "/queries/{id}/actions" {
							parts[i] = "7"
						}
					}
				}
				escaped := "/proxy/api/v1" + strings.Join(parts, "/")
				wantPath, _ := url.PathUnescape(escaped)
				method := strings.ToUpper(verb)
				var edits, calls int
				failure := errors.New("controlled transport failure")
				c, err := New("https://fixture.invalid/proxy", Options{Token: "fixture-token", HTTPClient: &http.Client{Transport: totpTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					body, _ := io.ReadAll(r.Body)
					if r.Method != method || r.URL.Path != wantPath || r.URL.RawQuery != "opaque=a&opaque=b" || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("If-Match") != "original-version" || !bytes.Equal(body, []byte("exact-body")) {
						t.Error("generated operation changed path, query, headers or body")
					}
					return nil, failure
				})}})
				if err != nil {
					t.Fatal("construct generated adapter")
				}
				c.generated.RequestEditors = []openapi.RequestEditorFn{func(_ context.Context, r *http.Request) error {
					edits++
					if r.Method != method || r.URL.Path != wantPath {
						t.Error("wrong generated operation selected before CLI editor")
					}
					return nil
				}}
				_, err = c.Raw(t.Context(), Request{Method: method, Path: strings.Join(parts, "/"), Query: url.Values{"opaque": {"a", "b"}}, Body: strings.NewReader("exact-body"), Header: http.Header{"If-Match": {"original-version"}}})
				if !errors.Is(err, failure) || edits != 1 || calls != 1 {
					t.Fatal("documented request bypassed generated operation or retried its failure")
				}
			})
		}
	}
}

func TestGeneratedTransportExplicitFallbackAndFinalErrors(t *testing.T) {
	for _, path := range []string{"/undocumented", "/api/v2/version"} {
		t.Run(path, func(t *testing.T) {
			var edits, calls int
			c, _ := New("https://fixture.invalid", Options{HTTPClient: &http.Client{Transport: totpTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, context.Canceled
			})}})
			c.generated.RequestEditors = []openapi.RequestEditorFn{func(context.Context, *http.Request) error { edits++; return nil }}
			_, err := c.Raw(t.Context(), Request{Path: path})
			if !errors.Is(err, context.Canceled) || calls != 1 || edits != 0 {
				t.Fatal("unknown route was dispatched through an invented operation")
			}
		})
	}
	var calls int
	c, _ := New("https://fixture.invalid", Options{HTTPClient: &http.Client{Transport: totpTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, context.Canceled
	})}})
	_, err := c.Raw(t.Context(), Request{Method: http.MethodPost, Path: "/queries/not-an-integer/actions"})
	if err == nil || calls != 0 {
		t.Fatal("generated parameter failure fell back to a raw dispatch")
	}
	// A trusted generated builder must also match the original requested path.
	// Changing its configured base is rejected, never repaired by an editor.
	c.generated.Server += "wrong/"
	_, err = c.Raw(t.Context(), Request{Path: "/version"})
	if err == nil || calls != 0 {
		t.Fatal("operation path mismatch reached the network")
	}
}

func TestGeneratedTransportPublicHeaderPolicy(t *testing.T) {
	var calls int
	c, err := New("https://fixture.invalid/proxy", Options{Token: "ambient-token", UserAgent: "configured-agent", HTTPClient: &http.Client{Transport: totpTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "fixture.invalid" || r.URL.Path != "/proxy/api/v1/auth/tokens" || r.Header.Get("Authorization") != "Bearer explicit-token" || r.Header.Get("Accept") != "application/custom" || r.Header.Get("Content-Type") != "application/yaml" || r.Header.Get("User-Agent") != "configured-agent" || r.Header.Get(CSRFHeader) != "installed-csrf" {
			t.Error("public request headers changed CLI precedence")
		}
		if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" || r.GetBody != nil {
			t.Error("single-use generated request regained replay facilities")
		}
		return nil, context.Canceled
	})}})
	if err != nil {
		t.Fatal("construct header policy client")
	}
	c.SetCSRF("installed-csrf")
	_, err = c.raw(t.Context(), Request{Method: http.MethodPost, Path: "/auth/tokens", ContentType: "application/yaml", Body: strings.NewReader("exact-body"), Header: http.Header{
		"Authorization":     {"Bearer explicit-token"},
		"Accept":            {"application/custom"},
		"Content-Type":      {"ignored"},
		"User-Agent":        {"ignored"},
		CSRFHeader:          {"ignored"},
		"Idempotency-Key":   {"ignored"},
		"X-Idempotency-Key": {"ignored"},
	}}, true)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("generated request changed cancellation or dispatch count")
	}
}

func TestGeneratedTransportPreservesEscapedSegmentBoundaries(t *testing.T) {
	for _, changeBoundary := range []bool{false, true} {
		var calls int
		c, _ := New("https://fixture.invalid/proxy", Options{HTTPClient: &http.Client{Transport: totpTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, context.Canceled
		})}})
		if changeBoundary {
			c.generated.RequestEditors = []openapi.RequestEditorFn{func(_ context.Context, r *http.Request) error {
				// Model a generated builder/editor losing an encoded slash.
				// This private seam is not exposed by the public CLI adapter.
				r.URL.RawPath = ""
				return nil
			}}
		}
		_, err := c.Raw(t.Context(), Request{Path: "/objects/dnszones/a%2fb"})
		if changeBoundary {
			if err == nil || calls != 0 {
				t.Fatal("equal decoded paths hid a changed route segment boundary")
			}
		} else if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatal("equivalent percent encoding was refused or double escaped")
		}
	}
}
