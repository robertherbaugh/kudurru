package catalogue

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
)

// Icon size limits: the binary carries every icon (D75 size budget).
const (
	maxIconBytes  = 16 << 10
	maxIconsBytes = 256 << 10
)

// safeElements are the SVG elements an icon may use: shapes, paint
// servers, grouping and reuse inside the document, and style sheets. No
// script, foreignObject, image, a, animation or font element.
var safeElements = map[string]bool{
	"svg": true, "g": true, "path": true, "circle": true, "ellipse": true, "rect": true, "line": true,
	"polyline": true, "polygon": true, "defs": true, "lineargradient": true, "radialgradient": true,
	"stop": true, "clippath": true, "mask": true, "use": true, "symbol": true, "style": true,
	"title": true, "desc": true,
	// Filter primitives that only compute from the document's own pixels
	// (no feImage, which can load a URL).
	"filter": true, "fegaussianblur": true, "feflood": true, "feblend": true, "fecolormatrix": true,
	"fecomposite": true, "feoffset": true, "femerge": true, "femergenode": true,
}

// urlRef finds url(...) references; only same-document ones (#id) pass.
var urlRef = regexp.MustCompile(`(?i)url\(\s*(['"]?)([^)'"]*)`)

func checkRefs(where, v string) error {
	low := strings.ToLower(v)
	for _, bad := range []string{"javascript:", "@import", "expression(", "data:"} {
		if strings.Contains(low, bad) {
			return fmt.Errorf("%s contains %q", where, bad)
		}
	}
	for _, m := range urlRef.FindAllStringSubmatch(v, -1) {
		if !strings.HasPrefix(strings.TrimSpace(m[2]), "#") {
			return fmt.Errorf("%s references %q outside the document", where, m[2])
		}
	}
	return nil
}

// checkSVG is the sanitiser check every embedded icon must pass: well-formed
// XML with an svg root, allow-listed elements only, no event handler
// attributes, no entity or doctype declarations, and href and url()
// references only to fragments of the same document.
func checkSVG(b []byte) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	depth, inStyle := 0, false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.Directive:
			return fmt.Errorf("declaration <!%s> is not allowed", bytes.Fields(t)[0])
		case xml.ProcInst:
			if t.Target != "xml" {
				return fmt.Errorf("processing instruction <?%s?> is not allowed", t.Target)
			}
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if depth == 0 && name != "svg" {
				return fmt.Errorf("root element <%s>, want <svg>", t.Name.Local)
			}
			if !safeElements[name] {
				return fmt.Errorf("element <%s> is not allowed", t.Name.Local)
			}
			for _, a := range t.Attr {
				an := strings.ToLower(a.Name.Local)
				where := fmt.Sprintf("<%s %s>", t.Name.Local, an)
				switch {
				case strings.HasPrefix(an, "on"):
					return fmt.Errorf("event handler attribute %s", where)
				case an == "href" && !strings.HasPrefix(strings.TrimSpace(a.Value), "#"):
					return fmt.Errorf("%s references %q outside the document", where, a.Value)
				}
				if err := checkRefs(where, a.Value); err != nil {
					return err
				}
			}
			inStyle = name == "style"
			depth++
		case xml.EndElement:
			depth--
			inStyle = false
		case xml.CharData:
			if inStyle {
				if err := checkRefs("<style>", string(t)); err != nil {
					return err
				}
			}
		}
	}
	if depth != 0 {
		return errors.New("unbalanced document")
	}
	return nil
}

// TestIconsSafe scans every embedded SVG.
func TestIconsSafe(t *testing.T) {
	slugs := IconSlugs()
	if len(slugs) == 0 {
		t.Fatal("no icons embedded")
	}
	total := 0
	for _, s := range slugs {
		ic, _ := Icon(s)
		total += len(ic.SVG)
		if len(ic.SVG) > maxIconBytes {
			t.Errorf("%s.svg is %d bytes, over %d", s, len(ic.SVG), maxIconBytes)
		}
		if !idPattern.MatchString(s) {
			t.Errorf("icon file %s.svg: the slug must match %s", s, IDPattern)
		}
		if err := checkSVG(ic.SVG); err != nil {
			t.Errorf("%s.svg: %v", s, err)
		}
	}
	if total > maxIconsBytes {
		t.Errorf("icons total %d bytes, over %d", total, maxIconsBytes)
	}
}

// TestCheckSVG proves the sanitiser check refuses what it must.
func TestCheckSVG(t *testing.T) {
	ok := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><defs><linearGradient id="a"/></defs><style>.a{fill:url(#a)}</style><path fill="url(#a)" d="M0 0h1v1z"/><use href="#a"/></svg>`
	if err := checkSVG([]byte(ok)); err != nil {
		t.Fatalf("a safe icon was refused: %v", err)
	}
	for name, doc := range map[string]string{
		"script":         `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"event handler":  `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`,
		"foreignObject":  `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject/></svg>`,
		"image":          `<svg xmlns="http://www.w3.org/2000/svg"><image href="https://x.example/a.png"/></svg>`,
		"external href":  `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="https://x.example/a.svg#b"/></svg>`,
		"external url":   `<svg xmlns="http://www.w3.org/2000/svg"><path fill="url(https://x.example/p)"/></svg>`,
		"style import":   `<svg xmlns="http://www.w3.org/2000/svg"><style>@import url(https://x.example/a.css);</style></svg>`,
		"style url":      `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{background:url('//x.example/t')}</style></svg>`,
		"javascript url": `<svg xmlns="http://www.w3.org/2000/svg"><use href="javascript:alert(1)"/></svg>`,
		"entity":         `<!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"not svg":        `<html/>`,
		"feImage":        `<svg xmlns="http://www.w3.org/2000/svg"><filter id="f"><feImage href="#a"/></filter></svg>`,
	} {
		if err := checkSVG([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestIconProvenance: every embedded icon has its row with its SHA-256,
// the licence is Apache-2.0 and the commit is recorded.
func TestIconProvenance(t *testing.T) {
	prov := IconProvenance()
	if !strings.Contains(prov, "SPDX-License-Identifier: Apache-2.0") || !strings.Contains(IconLicense(), "Version 2.0, January 2004") {
		t.Error("icons licence: PROVENANCE.md or LICENSE is not Apache-2.0")
	}
	if !regexp.MustCompile("Upstream commit \\| `[0-9a-f]{40}`").MatchString(prov) {
		t.Error("PROVENANCE.md records no upstream commit")
	}
	for _, s := range IconSlugs() {
		ic, _ := Icon(s)
		sum := sha256.Sum256(ic.SVG)
		row := fmt.Sprintf("| `%s` | `svg/%s.svg` | %d | ", s, s, len(ic.SVG))
		i := strings.Index(prov, row)
		if i < 0 {
			t.Errorf("PROVENANCE.md has no row %q", row)
			continue
		}
		if line := prov[i : strings.IndexByte(prov[i:], '\n')+i]; !strings.Contains(line, "`"+hex.EncodeToString(sum[:])+"`") {
			t.Errorf("row of %s does not record SHA-256 %x", s, sum)
		}
	}
}

// TestCatalogueIcons: the embedded catalogue names only icons the binary
// has, the dark-background variants exist, and the ID fallback works.
func TestCatalogueIcons(t *testing.T) {
	c := Embedded()
	named := 0
	for _, s := range c.Services {
		if s.Icon != "" {
			named++
			if !HasIcon(s.Icon) {
				t.Errorf("service %s names icon %q, which is not embedded", s.ID, s.Icon)
			}
		}
	}
	for _, e := range c.SafeSearch {
		if e.Icon != "" && !HasIcon(e.Icon) {
			t.Errorf("engine %s names icon %q, which is not embedded", e.ID, e.Icon)
		}
	}
	if named == 0 {
		t.Error("no service names an icon")
	}
	if svc, _ := c.Service("twitter"); svc.IconSlug() != "x" {
		t.Errorf("twitter icon = %q", svc.IconSlug())
	}
	if svc, _ := c.Service("roblox"); svc.IconSlug() != "" {
		t.Errorf("roblox icon = %q, want none", svc.IconSlug())
	}
	byID := Service{ID: "netflix"}
	if byID.IconSlug() != "netflix" {
		t.Errorf("ID fallback = %q", byID.IconSlug())
	}
	unknown := Service{ID: "nothing-here", Icon: "from-a-newer-build"}
	if unknown.IconSlug() != "" {
		t.Errorf("an icon this build lacks resolved to %q", unknown.IconSlug())
	}
	for base, v := range onDark {
		if !HasIcon(base) || !HasIcon(v) {
			t.Errorf("dark variant %s -> %s: missing file", base, v)
		}
	}
	if _, err := Parse([]byte(strings.Replace(string(embeddedJSON), `"icon": "x"`, `"icon": "Not A Slug"`, 1)), "test"); err == nil {
		t.Error("an invalid icon slug was accepted")
	}
}
