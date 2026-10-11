package listdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// permissive lists the SPDX identifiers embedded data may carry; copyleft
// and non-commercial licences are refused (CONTRIBUTING.md, §6.4).
var permissive = map[string]bool{
	"MIT": true, "BSD-2-Clause": true, "BSD-3-Clause": true, "Apache-2.0": true,
	"ISC": true, "0BSD": true, "CC0-1.0": true, "Unlicense": true,
}

// TestProvenance is the licences.yml-style check for embedded data: the
// provenance file names an allowed licence, the licence text is present,
// and the recorded digest is the digest of the embedded bytes.
func TestProvenance(t *testing.T) {
	prov := Provenance()
	m := regexp.MustCompile(`(?m)^SPDX-License-Identifier: (\S+)$`).FindStringSubmatch(prov)
	if m == nil {
		t.Fatal("PROVENANCE.md has no SPDX-License-Identifier line")
	}
	if !permissive[m[1]] {
		t.Fatalf("embedded list licence %q is not on the permissive allow-list", m[1])
	}
	for _, banned := range []string{"GPL", "EUPL", "NonCommercial", "CC-BY-NC", "hagezi", "AdGuard"} {
		if regexp.MustCompile(`(?m)^SPDX-License-Identifier: .*` + banned).MatchString(prov) {
			t.Errorf("provenance licence line mentions %s", banned)
		}
	}
	if !strings.Contains(prov, "| Licence | "+m[1]) {
		t.Errorf("the provenance table does not name the licence %s", m[1])
	}
	lic := License()
	if m[1] == "MIT" && (!strings.Contains(lic, "MIT License") || !strings.Contains(lic, "Permission is hereby granted")) {
		t.Error("LICENSE is not the MIT licence text")
	}
	if !strings.Contains(lic, "Copyright") {
		t.Error("LICENSE carries no copyright line")
	}
	sum := sha256.Sum256(StarterHosts())
	if want := hex.EncodeToString(sum[:]); !strings.Contains(prov, "| SHA-256 | `"+want+"` |") {
		t.Errorf("provenance does not record the embedded file's SHA-256 %s", want)
	}
	if !strings.Contains(prov, "Upstream commit") || !strings.Contains(prov, "https://github.com/StevenBlack/hosts") {
		t.Error("provenance lacks the upstream repository or commit")
	}
}

func TestStarterBlockList(t *testing.T) {
	bl, err := StarterBlockList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bl.Name != StarterName || bl.Namespace != "" || bl.Annotations[ProvenanceAnnotation] != StarterProvenance {
		t.Fatalf("meta = %+v", bl.ObjectMeta)
	}
	if len(bl.Spec.Sources) != 1 {
		t.Fatalf("sources = %d", len(bl.Spec.Sources))
	}
	src := bl.Spec.Sources[0]
	if src.Name != StarterSourceName || src.Format != "domains" || src.URL != "" {
		t.Errorf("source = %+v", src)
	}
	// 2,854 rules, two of them duplicates (the corpus golden file).
	if n := len(src.Inline); n != 2852 {
		t.Errorf("domains = %d, want 2852", n)
	}
	size := 0
	for _, d := range src.Inline {
		size += len(d) + 3
	}
	if size > 256<<10 {
		t.Errorf("inline source is %d bytes; keep the object far below the 1.5 MiB etcd cap", size)
	}
	// The inline source compiles to the same entries as the original file.
	spec, err := blocking.SpecFromAPI("default/starter", &bl.Spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := blocking.Compile(spec, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	orig, err := blocking.Compile(blocking.Spec{Name: "orig", Mode: blocking.ModeBlock, Sources: []blocking.Source{
		blocking.BytesSource("hosts", parser.FormatHosts, StarterHosts()),
	}}, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Stats().Entries != orig.Stats().Entries || l.Stats().Invalid != 0 {
		t.Errorf("entries %d (invalid %d), original %d", l.Stats().Entries, l.Stats().Invalid, orig.Stats().Entries)
	}
	if _, ok := l.Match("ad-assets.futurecdn.net"); !ok {
		t.Error("first entry not matched")
	}
	if _, ok := l.Match("sub.ad-assets.futurecdn.net"); ok {
		t.Error("hosts entries must match exactly, not subdomains")
	}
}
