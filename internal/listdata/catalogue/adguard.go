package catalogue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// The opt-in AdGuard import (blocking.catalogue.adguardServicesURL): the
// blocked services of AdGuard's HostlistsRegistry, read at runtime from a
// URL the administrator configures and merged into the catalogue in
// service next to kudurru's own entries.
//
// The data is GPL-3.0 and kudurru does not distribute it: nothing of it is
// embedded, vendored, pre-fetched or used in tests (adguard_test.go uses a
// hand-written document in the same format that names example.com
// domains only). The administrator's own instance downloads it; this file
// implements the file format, from the registry's public description of
// it, and copies no data or code.
//
// The format is a JSON object with "blocked_services", an array of
// {"id", "name", "rules", "group", "icon_svg"} (a bare array of those is
// read too), and optionally "groups". IDs are snake_case; kudurru IDs
// are DNS-label-like, so "_" becomes "-" ("amazon_streaming" ->
// "amazon-streaming"). Rules are adblock rules; a rule kudurru cannot
// apply as a plain block entry is dropped (counted in DroppedRules) and a
// service left without rules is skipped. Only rules of the form
// ||<name>^ are imported, where name is a host name below a public
// suffix: the document is third-party data, and a service rule such as
// ||com^ or /.*/ would block every name for each Policy that blocks the
// service's category. icon_svg is ignored: the dashboard draws a monogram
// for imported services.
//
// Merge rules (Holder): a kudurru service with the same ID wins and the
// imported one is dropped (counted as shadowed), so enabling the import
// never changes what an existing Policy's blockedServices block; AdGuard's
// groups map to kudurru categories (adguardGroups), anything else to
// "other", and a Policy that blocks a whole category (blockedCategories)
// blocks the imported services of that category too.

// AdGuard import constants.
const (
	// AdGuardSource is the Source of every imported service.
	AdGuardSource = "adguard-hostlists-registry"
	// AdGuardLicense is the SPDX licence of the imported data.
	AdGuardLicense = "GPL-3.0"
	// MaxAdGuardBytes caps an AdGuard services document (the registry's
	// carries an SVG icon per service).
	MaxAdGuardBytes = 16 << 20
)

// adguardGroups maps the registry's groups to kudurru categories; a group
// not listed, or whose category the catalogue in service lacks, becomes
// CategoryOther.
var adguardGroups = map[string]string{
	"ai":             "ai",
	"dating":         "dating",
	"gambling":       "gambling",
	"gaming":         "gaming",
	"messenger":      "messaging",
	"shopping":       "shopping",
	"social_network": "social",
	"streaming":      "video",
}

// Import is one parsed AdGuard services document. It is immutable.
type Import struct {
	// Services are the importable services, sorted by ID, with Source
	// AdGuardSource and the category their group maps to.
	Services []Service
	// Skipped counts services left out: an ID that does not convert, a
	// duplicate, no name, or no usable rule.
	Skipped int
	// DroppedRules counts rules that are not plain block entries.
	DroppedRules int

	raw    []byte
	digest string
	origin string
}

// Origin is the URL the document came from.
func (i *Import) Origin() string { return i.origin }

// Digest is the lowercase hex SHA-256 of the document.
func (i *Import) Digest() string { return i.digest }

// Raw returns the document bytes.
func (i *Import) Raw() []byte { return bytes.Clone(i.raw) }

type adguardService struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Rules []string `json:"rules"`
	Group string   `json:"group"`
}

// ParseAdGuard decodes an AdGuard HostlistsRegistry services document.
// origin is the URL it came from. A document without a single usable
// service is an error, so a broken download never empties the import.
func ParseAdGuard(b []byte, origin string) (*Import, error) {
	if len(b) > MaxAdGuardBytes {
		return nil, fmt.Errorf("catalogue: AdGuard services document is %d bytes, over the %d byte cap", len(b), MaxAdGuardBytes)
	}
	var list []adguardService
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("catalogue: AdGuard services: %w", err)
		}
	} else {
		var doc struct {
			BlockedServices *[]adguardService `json:"blocked_services"`
		}
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, fmt.Errorf("catalogue: AdGuard services: %w", err)
		}
		if doc.BlockedServices == nil {
			return nil, errors.New(`catalogue: AdGuard services: the document has no "blocked_services"`)
		}
		list = *doc.BlockedServices
	}
	imp := &Import{raw: bytes.Clone(b), digest: digestOf(b), origin: origin}
	seen := map[string]bool{}
	for _, a := range list {
		id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(a.ID)), "_", "-")
		name := strings.TrimSpace(a.Name)
		if !idPattern.MatchString(id) || seen[id] || checkText("name", name) != nil {
			imp.Skipped++
			continue
		}
		rules, dropped := usableRules(a.Rules)
		imp.DroppedRules += dropped
		if len(rules) == 0 {
			imp.Skipped++
			continue
		}
		seen[id] = true
		cat, ok := adguardGroups[a.Group]
		if !ok {
			cat = CategoryOther
		}
		imp.Services = append(imp.Services, Service{ID: id, Name: name, Category: cat, Rules: rules, Source: AdGuardSource, origin: OriginAdGuard})
	}
	if len(imp.Services) == 0 {
		return nil, fmt.Errorf("catalogue: AdGuard services: no usable service among %d entries", len(list))
	}
	slices.SortFunc(imp.Services, func(a, b Service) int { return strings.Compare(a.ID, b.ID) })
	return imp, nil
}

// usableRules keeps the rules that are a plain block entry of a name
// below a public suffix (adguardRule), at most maxRules, and counts the
// others.
func usableRules(in []string) (out []string, dropped int) {
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" || strings.ContainsAny(r, "\n\r") || len(out) == maxRules || !adguardRule(r) {
			dropped++
			continue
		}
		if checkRules([]string{r}) != nil {
			dropped++
			continue
		}
		out = append(out, r)
	}
	return out, dropped
}

// adguardRule reports whether r is ||<name>^ for a canonical host name
// that is not an IP address and not itself a public suffix (com, co.uk,
// github.io), so it blocks one site and its subdomains.
func adguardRule(r string) bool {
	host, ok := strings.CutPrefix(r, "||")
	if !ok {
		return false
	}
	if host, ok = strings.CutSuffix(host, "^"); !ok || !strings.Contains(host, ".") || !canonicalHost(host) {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	suffix, _ := publicsuffix.PublicSuffix(host)
	return suffix != host
}

// otherCategory is added when an import needs CategoryOther and the
// catalogue document has none.
var otherCategory = Category{ID: CategoryOther, Name: "Other", Description: "Imported services without a matching category.", Icon: "shapes", Order: 1000}

// merge returns base with imp's services added: base wins on an ID both
// have. It returns base itself when imp is nil.
func merge(base *Catalogue, imp *Import) *Catalogue {
	if imp == nil {
		return base
	}
	c := &Catalogue{
		SchemaVersion: base.SchemaVersion, Version: base.Version, License: base.License, Homepage: base.Homepage,
		Categories: slices.Clone(base.Categories), Services: slices.Clone(base.Services), SafeSearch: slices.Clone(base.SafeSearch),
		raw: base.raw, digest: base.digest, origin: base.origin, imported: imp,
		fingerprint: digestOf([]byte(base.digest + ":" + imp.digest)),
	}
	needOther := false
	for _, s := range imp.Services {
		if base.HasService(s.ID) {
			c.shadowed++
			continue
		}
		if !base.HasCategory(s.Category) {
			s.Category = CategoryOther
			needOther = needOther || !base.HasCategory(CategoryOther)
		}
		c.Services = append(c.Services, s)
	}
	if needOther {
		c.Categories = append(c.Categories, otherCategory)
	}
	c.index()
	return c
}
