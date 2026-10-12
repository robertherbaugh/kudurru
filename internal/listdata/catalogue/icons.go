package catalogue

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"path"
	"slices"
	"strings"
)

// The service and search-engine logos are SVGs from homarr-labs'
// dashboard-icons (Apache-2.0), embedded unmodified or as svgo optimised
// them; icons/PROVENANCE.md names the upstream commit and file of each,
// TestIconProvenance keeps it current and TestIconsSafe checks that no
// icon carries a script, an event handler or an external reference. The
// API serves them as /api/v1/catalogue/icons/{slug}.svg; the dashboard
// shows a monogram for an entry without one.

//go:embed icons/*.svg
var iconFS embed.FS

//go:embed icons/LICENSE
var iconLicense string

//go:embed icons/PROVENANCE.md
var iconProvenance string

// IconImage is one embedded icon.
type IconImage struct {
	// Slug is the icon's name: the dashboard-icons file name without .svg.
	Slug string
	// SVG is the document.
	SVG []byte
	// ETag is a strong entity tag: the quoted first 16 bytes of the SHA-256.
	ETag string
}

// onDark maps an icon whose artwork is dark on a transparent background to
// its dashboard-icons variant for dark backgrounds.
var onDark = map[string]string{
	"x":             "x-light",
	"threads":       "threads-light",
	"tiktok":        "tiktok-light",
	"control-d":     "control-d-dark",
	"amazon":        "amazon-light",
	"apple-tv-plus": "apple-tv-plus-light",
	"grok":          "grok-dark", // upstream names the white artwork -dark
	"hbo":           "hbo-light",
	"peacock":       "peacock-light",
	"plex":          "plex-light",
	"soundcloud":    "soundcloud-dark", // the default is currentColor, drawn black in <img>
	"tidal":         "tidal-dark",
}

var icons = func() map[string]IconImage {
	out := map[string]IconImage{}
	entries, err := iconFS.ReadDir("icons")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		b, err := iconFS.ReadFile(path.Join("icons", e.Name()))
		if err != nil {
			panic(err)
		}
		slug := strings.TrimSuffix(e.Name(), ".svg")
		sum := sha256.Sum256(b)
		out[slug] = IconImage{Slug: slug, SVG: b, ETag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	}
	return out
}()

// Icon returns the embedded icon slug.
func Icon(slug string) (IconImage, bool) {
	i, ok := icons[slug]
	return i, ok
}

// HasIcon reports whether the binary has icon slug.
func HasIcon(slug string) bool {
	_, ok := icons[slug]
	return ok
}

// IconSlugs returns every embedded icon, sorted.
func IconSlugs() []string {
	out := make([]string, 0, len(icons))
	for s := range icons {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// IconOnDark returns the variant of slug drawn for dark backgrounds, when
// dashboard-icons has one.
func IconOnDark(slug string) (string, bool) {
	v, ok := onDark[slug]
	return v, ok && HasIcon(v)
}

// IconLicense returns the licence text of the icons (Apache-2.0).
func IconLicense() string { return iconLicense }

// IconProvenance returns the icons' provenance file.
func IconProvenance() string { return iconProvenance }

// iconSlug resolves an entry's icon: the document's icon slug when the
// binary has it, else an icon named like the entry's ID, else none. A
// document may name icons a newer build ships; they resolve to none here.
func iconSlug(icon, id string) string {
	switch {
	case icon != "" && HasIcon(icon):
		return icon
	case HasIcon(id):
		return id
	}
	return ""
}

// IconSlug is the service's icon; "" when the binary has none for it.
func (s *Service) IconSlug() string { return iconSlug(s.Icon, s.ID) }

// IconSlug is the engine's icon; "" when the binary has none for it.
func (e *Engine) IconSlug() string { return iconSlug(e.Icon, e.ID) }
