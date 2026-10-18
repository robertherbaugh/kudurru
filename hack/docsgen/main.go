// Command docsgen writes the generated reference pages of the docs site
// (docs-site/content/reference):
//
//   - configuration.md from the settings metadata table
//     (internal/config.Schema, the table `kudurru config schema` prints);
//   - crds/<kind>.md from the OpenAPI v3 schemas of the CRDs in
//     deploy/kustomize/crd/bases (what `make manifests` writes);
//   - api/endpoints.md from the OpenAPI document api/openapi.json (what
//     `make openapi` writes).
//
// `make docs-gen` runs it. TestReferenceDrift fails when the committed
// pages are stale, so a change to the schema table, the CRD types or the
// API routes has to regenerate them in the same commit.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Output locations, relative to the repository root.
const (
	referenceDir = "docs-site/content/reference"
	configPage   = referenceDir + "/configuration.md"
	crdDir       = referenceDir + "/crds"
	endpointPage = referenceDir + "/api/endpoints.md"
	crdBases     = "deploy/kustomize/crd/bases"
	openAPIFile  = "api/openapi.json"
)

// page is one generated file.
type page struct {
	Path string // relative to the repository root
	Body []byte
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := run(*root); err != nil {
		fmt.Fprintln(os.Stderr, "docsgen:", err)
		os.Exit(1)
	}
}

func run(root string) error {
	pages, err := generate(root)
	if err != nil {
		return err
	}
	for _, p := range pages {
		path := filepath.Join(root, p.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, p.Body) { //nolint:gosec // a page this program writes
			continue
		}
		if err := os.WriteFile(path, p.Body, 0o644); err != nil { //nolint:gosec // documentation, world-readable on purpose
			return err
		}
		fmt.Println("wrote", p.Path)
	}
	stale, err := stalePages(root, pages)
	if err != nil {
		return err
	}
	for _, s := range stale {
		if err := os.Remove(filepath.Join(root, s)); err != nil {
			return err
		}
		fmt.Println("removed", s)
	}
	return nil
}

// generate renders every reference page from the sources under root.
func generate(root string) ([]page, error) {
	cfg, err := configReference()
	if err != nil {
		return nil, err
	}
	pages := []page{{Path: configPage, Body: cfg}}
	crds, err := crdReference(filepath.Join(root, crdBases))
	if err != nil {
		return nil, err
	}
	pages = append(pages, crds...)
	api, err := endpointReference(filepath.Join(root, openAPIFile))
	if err != nil {
		return nil, err
	}
	return append(pages, page{Path: endpointPage, Body: api}), nil
}

// stalePages lists generated CRD pages whose CRD no longer exists.
func stalePages(root string, pages []page) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, crdDir))
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, e := range entries {
		rel := crdDir + "/" + e.Name()
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		if !slices.ContainsFunc(pages, func(p page) bool { return p.Path == rel }) {
			stale = append(stale, rel)
		}
	}
	return stale, nil
}
