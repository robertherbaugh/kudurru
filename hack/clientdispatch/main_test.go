package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestGeneratedDispatchMatchesEveryOpenAPIOperation(t *testing.T) {
	schema, err := os.ReadFile("../../api/openapi-3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	client, err := os.ReadFile("../../internal/apiclient/openapi/zz_generated.client.go")
	if err != nil {
		t.Fatal(err)
	}
	generated, count, err := generate(schema, client)
	if err != nil || count == 0 {
		t.Fatal("cannot reproduce complete OpenAPI dispatch", err)
	}
	current, err := os.ReadFile("../../internal/apiclient/openapi/zz_generated.dispatch.go")
	if err != nil {
		t.Fatal("run GOMAXPROCS=2 go run -p 2 ./hack/clientdispatch from the repository root", err)
	}
	if !bytes.Equal(current, generated) {
		t.Fatal("generated dispatch drift: regenerate after the OpenAPI client")
	}
	for range 3 {
		again, n, err := generate(schema, client)
		if err != nil || n != count || !bytes.Equal(again, generated) {
			t.Fatal("dispatch generation depends on map iteration order")
		}
	}
}

func TestGeneratorRefusesIncompleteOrUnsupportedDispatch(t *testing.T) {
	const schema = `{"paths":{"/example/{id}":{"get":{"operationId":"example"}}}}`
	client := "package openapi\ntype Client struct{}\n// Corresponds with GET /example/{id} (the `Example` operationId).\nfunc(c *Client) Example(ctx context.Context, id string, reqEditors ...RequestEditorFn) (*http.Response,error) {return nil,nil}\n"
	for _, tc := range []struct {
		name, schema, client string
	}{
		{"empty", `{"paths":{}}`, client},
		{"missing", schema, "package openapi"},
		{"wrong method", schema, strings.Replace(client, "GET /example", "POST /example", 1)},
		{"wrong path", schema, strings.Replace(client, "/example/{id}", "/other/{id}", 1)},
		{"path type", schema, strings.Replace(client, "id string", "id bool", 1)},
		{"parameter count", schema, strings.Replace(client, "id string, ", "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := generate([]byte(tc.schema), []byte(tc.client)); err == nil {
				t.Fatal("generator accepted a dispatch that cannot cover the specification")
			}
		})
	}
}

func TestGeneratorPrefersStaticPathsAndRawBodyMethod(t *testing.T) {
	schema := []byte(`{"paths":{"/zones/{zone}":{"get":{}},"/zones/options":{"get":{},"post":{}}}}`)
	client := []byte("package openapi\ntype Client struct{}\n" +
		"// Corresponds with GET /zones/{zone} (the `Zone` operationId).\nfunc(c *Client) Zone(ctx context.Context, zone string, reqEditors ...RequestEditorFn) (*http.Response,error) {return nil,nil}\n" +
		"// Corresponds with GET /zones/options (the `Options` operationId).\nfunc(c *Client) Options(ctx context.Context, reqEditors ...RequestEditorFn) (*http.Response,error) {return nil,nil}\n" +
		"// Corresponds with POST /zones/options (the `SetOptions` operationId).\nfunc(c *Client) SetOptions(ctx context.Context, body OptionsBody, reqEditors ...RequestEditorFn) (*http.Response,error) {return nil,nil}\n" +
		"// Corresponds with POST /zones/options (the `SetOptions` operationId).\nfunc(c *Client) SetOptionsWithBody(ctx context.Context, contentType string, body io.Reader, reqEditors ...RequestEditorFn) (*http.Response,error) {return nil,nil}\n")
	got, n, err := generate(schema, client)
	if err != nil || n != 3 {
		t.Fatal("valid generated operations rejected", err)
	}
	text := string(got)
	if strings.Index(text, "c.Options(") > strings.Index(text, "c.Zone(") || !strings.Contains(text, "c.SetOptionsWithBody(") || strings.Contains(text, "c.SetOptions(") {
		t.Fatal("dispatch lost static precedence or remarshal-free body selection")
	}
}
