package validator

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

// crdFiles holds byte-for-byte copies of config/crd/bases/*.yaml. `make
// manifests` regenerates config/crd/bases with controller-gen and copies the
// result here; TestEmbeddedCRDsMatchManifests fails when the two drift.
//
//go:embed crds/*.yaml
var crdFiles embed.FS

// EmbeddedCRDs returns the CustomResourceDefinition manifests compiled into
// the binary, keyed by file name (e.g. "kudurru.io_dnszones.yaml"). They are
// the same documents the kudurru-crds Helm chart installs.
func EmbeddedCRDs() (map[string][]byte, error) {
	names, err := fs.Glob(crdFiles, "crds/*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make(map[string][]byte, len(names))
	for _, n := range names {
		b, err := crdFiles.ReadFile(n)
		if err != nil {
			return nil, fmt.Errorf("validator: read embedded %s: %w", n, err)
		}
		out[n[len("crds/"):]] = b
	}
	return out, nil
}
