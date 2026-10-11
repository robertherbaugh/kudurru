// Package listdata embeds the starter blocklist kudurru seeds on first
// boot (docs/architecture.md §6.4): Steven Black's own ad-hoc hosts list
// (MIT), with its licence and a provenance file. Only permissively licensed
// data may be embedded here; TestProvenance enforces that the provenance
// file names the licence and the recorded digest matches the bytes.
package listdata

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

//go:embed stevenblack/hosts stevenblack/LICENSE stevenblack/PROVENANCE.md
var files embed.FS

// Names used by the starter BlockList.
const (
	// StarterName is the BlockList's name; bootstrap puts it in the seed
	// namespace and adds it to Policy "default".
	StarterName = "starter"
	// StarterSourceName names its single inline source.
	StarterSourceName = "stevenblack-adhoc"
	// ProvenanceAnnotation carries a one-line provenance on the object.
	ProvenanceAnnotation = "kudurru.io/provenance"
	// StarterProvenance is the value of ProvenanceAnnotation.
	StarterProvenance = "github.com/StevenBlack/hosts data/StevenBlack/hosts@2025dea10a1d (MIT, Copyright (c) 2023 Steven Black); embedded, see internal/listdata/stevenblack/PROVENANCE.md"
)

func mustRead(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("listdata: embedded %s missing: %v", name, err))
	}
	return b
}

// StarterHosts returns the embedded hosts file, byte for byte as fetched.
func StarterHosts() []byte { return bytes.Clone(mustRead("stevenblack/hosts")) }

// License returns the licence text of the embedded list.
func License() string { return string(mustRead("stevenblack/LICENSE")) }

// Provenance returns the provenance file of the embedded list.
func Provenance() string { return string(mustRead("stevenblack/PROVENANCE.md")) }

// StarterDomains returns the host names of the embedded list in file
// order, deduplicated, normalised by the blocking parser.
func StarterDomains() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	_, err := parser.Parse(bytes.NewReader(mustRead("stevenblack/hosts")), parser.Options{Format: parser.FormatHosts, KeepUnsupported: -1, KeepInvalid: -1},
		func(r parser.Rule) error {
			if r.Kind != parser.KindExact || r.Exception {
				return nil
			}
			name := strings.Clone(r.Pattern)
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("listdata: parse the embedded starter list: %w", err)
	}
	return out, nil
}

// StarterBlockList builds the starter BlockList "starter": block mode, one
// inline domains source. Its signature matches
// bootstrap.StarterBlockListFunc; bootstrap fills in the namespace.
func StarterBlockList(context.Context) (*v1alpha1.BlockList, error) {
	domains, err := StarterDomains()
	if err != nil {
		return nil, err
	}
	return &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{
			Name:        StarterName,
			Annotations: map[string]string{ProvenanceAnnotation: StarterProvenance},
		},
		Spec: v1alpha1.BlockListSpec{
			Mode: "block",
			Sources: []v1alpha1.ListSource{{
				Name:   StarterSourceName,
				Format: "domains",
				Inline: domains,
			}},
		},
	}, nil
}
