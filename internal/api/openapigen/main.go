// Command openapigen writes the kudurru OpenAPI documents (make openapi):
// api/openapi.json (OpenAPI 3.1, what /api/v1/openapi.json serves) and
// api/openapi-3.0.json (the 3.0 downgrade the Go client is generated
// from). TestOpenAPIDrift in internal/api fails when the committed files
// differ from the registered routes.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/robertherbaugh/kudurru/internal/api"
)

func main() {
	dir := flag.String("dir", "api", "directory the documents are written to")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	for _, f := range []struct {
		name string
		v30  bool
	}{{api.OpenAPIFile, false}, {api.OpenAPI30File, true}} {
		doc, err := api.OpenAPIDocument(f.v30)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), doc, 0o644); err != nil { //nolint:gosec // a public document
			return err
		}
	}
	return nil
}
