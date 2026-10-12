// Package catalogue is kudurru's blocked-services and safe-search catalogue
// (docs/architecture.md §7, decision D60): which domains make up a service
// a Policy can block by ID (spec.blockedServices: youtube, tiktok, ...),
// and which search engines answer their own safe hostname when a Policy
// sets spec.safeSearch.
//
// The data is kudurru-authored, Apache-2.0, from each vendor's own public
// documentation and the services' first-party domains; PROVENANCE.md
// records the source of every entry. AdGuard's blocked-services and
// safe-search tables are GPL-3.0 and are neither copied nor consulted
// (§14 risk 16). TestProvenance and TestEmbedded keep both promises.
//
// The embedded catalogue.json ships in the binary. blocking.catalogue.url
// replaces it at runtime with a newer document of the same schema version
// (Refresher, refresh.go); the format and URL contract are in
// docs/deploy/blocked-services.md.
//
// Services compile like block lists: every service is a block-mode
// blocking.Spec named "service:<id>" whose rules are adblock rules
// ("||youtube.com^"), so the query log names the service as the list.
package catalogue

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// SchemaVersion is the catalogue document schema this build writes and
// reads. Version 2 added categories (with names, descriptions, icons and a
// sort order) and the requires, related and note hints of a service;
// MinSchemaVersion documents are still read, their categories filled in
// from the embedded catalogue's. A document with any other schemaVersion
// is refused, and the catalogue in service (the embedded one, or the last
// good download) stays.
const (
	SchemaVersion    = 2
	MinSchemaVersion = 1
)

// Name prefixes of the catalogue's entries in the query log (Response.List)
// and in the compiled list snapshot. BlockList names are DNS labels, so
// neither can collide with a BlockList.
const (
	ServiceListPrefix = "service:"
	SafeSearchPrefix  = "safesearch:"
)

// OriginEmbedded is the Origin of the catalogue built into the binary.
const OriginEmbedded = "embedded"

// MaxBytes caps a catalogue document.
const MaxBytes = 4 << 20

// Document limits.
const (
	maxServices   = 1000
	maxRules      = 1000 // per service
	maxEngines    = 100
	maxDomains    = 10000 // safe-search host names over all engines
	maxCategories = 100
	maxHints      = 20 // requires and related entries per service
	maxText       = 256
)

// Safe-search modes (Policy.spec.safeSearchMode).
const (
	ModeStrict   = "strict"
	ModeModerate = "moderate"
)

// ErrSchemaVersion is returned by Parse for a document of another schema
// version.
var ErrSchemaVersion = errors.New("catalogue: unsupported schemaVersion")

//go:embed catalogue.json
var embeddedJSON []byte

//go:embed LICENSE
var licenseText string

//go:embed PROVENANCE.md
var provenanceText string

// idPattern is the form of service, engine and category IDs.
var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// IDPattern is the pattern service and category IDs match (the CRD items
// pattern of Policy.spec.blockedServices and blockedCategories).
const IDPattern = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`

// CategoryOther is the category of imported services whose upstream group
// has no kudurru counterpart.
const CategoryOther = "other"

// Category groups services: the dashboard's tabs and section headers, and
// what Policy.spec.blockedCategories names.
type Category struct {
	// ID is the stable identifier blockedCategories names.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Description says what the category holds; optional.
	Description string `json:"description,omitempty"`
	// Icon is a Lucide icon name (kebab-case, "gamepad-2") the dashboard
	// draws next to the name; optional.
	Icon string `json:"icon,omitempty"`
	// Order sorts the categories (ascending, then by ID).
	Order int `json:"order"`
}

// Origins of a service in the catalogue in service.
const (
	// OriginKudurru: the catalogue document (embedded or
	// blocking.catalogue.url).
	OriginKudurru = "kudurru"
	// OriginAdGuard: the opt-in import of AdGuard's HostlistsRegistry
	// services (blocking.catalogue.adguardServicesURL, adguard.go).
	OriginAdGuard = "adguard"
)

// Service is one blockable service.
type Service struct {
	// ID is the stable identifier Policy.spec.blockedServices names.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Category is the ID of the service's category (ai, social, video, ...).
	Category string `json:"category"`
	// Rules are adblock-style DNS rules, "||domain^" (the domain and its
	// subdomains).
	Rules []string `json:"rules"`
	// Source is where the entry comes from: a vendor documentation URL or
	// "first-party domains of the service".
	Source string `json:"source"`
	// Icon optionally names the embedded logo (icons/<slug>.svg) when its
	// slug differs from ID; IconSlug resolves it.
	Icon string `json:"icon,omitempty"`
	// Requires names services this one also depends on, so blocking it
	// alone does not stop it (Fortnite signs in through Epic Games).
	Requires []string `json:"requires,omitempty"`
	// Related names services of the same vendor or closely tied to it.
	Related []string `json:"related,omitempty"`
	// Note is an optional caveat shown with the service.
	Note string `json:"note,omitempty"`

	// origin is OriginKudurru ("") or OriginAdGuard (Origin).
	origin string
}

// Origin is where the service comes from: OriginKudurru or OriginAdGuard.
func (s *Service) Origin() string {
	if s.origin == "" {
		return OriginKudurru
	}
	return s.origin
}

// Engine is one safe-search rewrite: the engine's host names are answered
// with a CNAME to Target (or ModerateTarget in moderate mode) and the
// target's own addresses, or, for engines whose documentation publishes an
// address instead of a host name, with Addresses directly.
type Engine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
	// ModerateTarget is the target in moderate mode; empty when the engine
	// has a single level.
	ModerateTarget string `json:"moderateTarget,omitempty"`
	// Addresses are answered as A/AAAA records when the engine has no
	// target host name.
	Addresses []string `json:"addresses,omitempty"`
	// Domains are the exact host names that are rewritten.
	Domains []string `json:"domains"`
	Source  string   `json:"source"`
	// Icon optionally names the embedded logo, as for Service.
	Icon string `json:"icon,omitempty"`

	addrs []netip.Addr
}

// Catalogue is one parsed, validated catalogue document. It is immutable.
type Catalogue struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
	License       string `json:"license"`
	Homepage      string `json:"homepage,omitempty"`
	// Categories are sorted by Order, then ID. A version 1 document has
	// none; Parse fills them in.
	Categories []Category `json:"categories,omitempty"`
	// Services are sorted by ID.
	Services []Service `json:"services"`
	// SafeSearch engines are sorted by ID.
	SafeSearch []Engine `json:"safeSearch"`

	raw        []byte
	digest     string
	origin     string
	byService  map[string]int
	byCategory map[string]int
	members    map[string][]string // category -> service IDs, sorted
	byHost     map[string]int
	specs      []blocking.Spec
	// imported is the AdGuard import merged in; nil without one.
	imported *Import
	// shadowed counts imported services dropped for a kudurru ID.
	shadowed int
	// fingerprint changes with the document and the import.
	fingerprint string
}

// Parse decodes and validates a catalogue document. origin is recorded for
// diagnostics: OriginEmbedded or the URL the document came from. Unknown
// fields are ignored, so a document may add fields within a schema
// version; anything this build relies on is checked strictly.
func Parse(b []byte, origin string) (*Catalogue, error) {
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("catalogue: document is %d bytes, over the %d byte cap", len(b), MaxBytes)
	}
	var head struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, fmt.Errorf("catalogue: %w", err)
	}
	switch {
	case head.SchemaVersion == nil:
		return nil, fmt.Errorf("%w: the document has no schemaVersion", ErrSchemaVersion)
	case *head.SchemaVersion < MinSchemaVersion || *head.SchemaVersion > SchemaVersion:
		return nil, fmt.Errorf("%w %d (this build reads %d to %d)", ErrSchemaVersion, *head.SchemaVersion, MinSchemaVersion, SchemaVersion)
	}
	c := &Catalogue{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("catalogue: %w", err)
	}
	if c.SchemaVersion == 1 {
		// Version 1 has no categories and no hints: its services' category
		// IDs get the embedded catalogue's names, or a name made from the ID.
		c.Categories = deriveCategories(c.Services, defaultCategories())
		for i := range c.Services {
			c.Services[i].Requires, c.Services[i].Related, c.Services[i].Note = nil, nil, ""
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	c.raw, c.digest, c.origin = bytes.Clone(b), digestOf(b), origin
	c.fingerprint = c.digest
	c.index()
	return c, nil
}

// defaultCategories are the embedded catalogue's categories; nil while
// the embedded catalogue itself is parsed.
func defaultCategories() []Category { return knownCategories }

// deriveCategories returns a category for every category ID services use:
// the one of known with that ID, else one named after the ID and sorted
// after the known ones.
func deriveCategories(services []Service, known []Category) []Category {
	byID := map[string]Category{}
	last := 0
	for _, k := range known {
		byID[k.ID] = k
		last = max(last, k.Order)
	}
	seen := map[string]bool{}
	var out []Category
	for _, s := range services {
		if seen[s.Category] || !idPattern.MatchString(s.Category) {
			continue
		}
		seen[s.Category] = true
		if k, ok := byID[s.Category]; ok {
			out = append(out, k)
			continue
		}
		out = append(out, Category{ID: s.Category, Name: titleCase(s.Category), Order: last + 1})
	}
	return out
}

// titleCase makes a display name from an ID: "social-media" -> "Social media".
func titleCase(id string) string {
	s := strings.ReplaceAll(id, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func checkText(field, s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len(s) > maxText {
		return fmt.Errorf("%s is longer than %d bytes", field, maxText)
	}
	return nil
}

// validate checks the whole document and reports every problem.
func (c *Catalogue) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if err := checkText("version", c.Version); err != nil {
		add("%v", err)
	}
	if err := checkText("license", c.License); err != nil {
		add("%v", err)
	}
	if len(c.Services) > maxServices {
		add("%d services, over the limit of %d", len(c.Services), maxServices)
	}
	if len(c.SafeSearch) > maxEngines {
		add("%d safe-search engines, over the limit of %d", len(c.SafeSearch), maxEngines)
	}
	if len(c.Categories) > maxCategories {
		add("%d categories, over the limit of %d", len(c.Categories), maxCategories)
	}
	cats := map[string]bool{}
	for i := range c.Categories {
		k := &c.Categories[i]
		where := fmt.Sprintf("categories[%d] %q", i, k.ID)
		if !idPattern.MatchString(k.ID) {
			add("%s: id must match %s", where, IDPattern)
		} else if cats[k.ID] {
			add("%s: duplicate id", where)
		}
		cats[k.ID] = true
		if err := checkText("name", k.Name); err != nil {
			add("%s: %v", where, err)
		}
		if len(k.Description) > maxText {
			add("%s: description is longer than %d bytes", where, maxText)
		}
		if k.Icon != "" && !idPattern.MatchString(k.Icon) {
			add("%s: icon %q must match %s", where, k.Icon, IDPattern)
		}
	}
	ids := make(map[string]bool, len(c.Services))
	for i := range c.Services {
		ids[c.Services[i].ID] = true
	}
	seen := map[string]bool{}
	for i := range c.Services {
		s := &c.Services[i]
		where := fmt.Sprintf("services[%d] %q", i, s.ID)
		if !idPattern.MatchString(s.ID) {
			add("%s: id must match %s", where, IDPattern)
		} else if seen[s.ID] {
			add("%s: duplicate id", where)
		}
		seen[s.ID] = true
		for _, f := range []struct{ name, v string }{{"name", s.Name}, {"source", s.Source}} {
			if err := checkText(f.name, f.v); err != nil {
				add("%s: %v", where, err)
			}
		}
		switch {
		case !idPattern.MatchString(s.Category):
			add("%s: category %q must match %s", where, s.Category, IDPattern)
		case !cats[s.Category]:
			add("%s: category %q is not in categories", where, s.Category)
		}
		if len(s.Note) > maxText {
			add("%s: note is longer than %d bytes", where, maxText)
		}
		for _, h := range []struct {
			field string
			ids   []string
		}{{"requires", s.Requires}, {"related", s.Related}} {
			if len(h.ids) > maxHints {
				add("%s: %d %s entries, over the limit of %d", where, len(h.ids), h.field, maxHints)
			}
			for _, id := range h.ids {
				switch {
				case id == s.ID:
					add("%s: %s names the service itself", where, h.field)
				case !ids[id]:
					add("%s: %s %q is not a service of the document", where, h.field, id)
				}
			}
		}
		if s.Icon != "" && !idPattern.MatchString(s.Icon) {
			add("%s: icon %q must match %s", where, s.Icon, IDPattern)
		}
		if len(s.Rules) == 0 || len(s.Rules) > maxRules {
			add("%s: %d rules; a service needs 1 to %d", where, len(s.Rules), maxRules)
			continue
		}
		if err := checkRules(s.Rules); err != nil {
			add("%s: %v", where, err)
		}
	}
	seen = map[string]bool{}
	hosts := map[string]string{}
	domains := 0
	for i := range c.SafeSearch {
		e := &c.SafeSearch[i]
		where := fmt.Sprintf("safeSearch[%d] %q", i, e.ID)
		if !idPattern.MatchString(e.ID) {
			add("%s: id must match %s", where, IDPattern)
		} else if seen[e.ID] {
			add("%s: duplicate id", where)
		}
		seen[e.ID] = true
		for _, f := range []struct{ name, v string }{{"name", e.Name}, {"source", e.Source}} {
			if err := checkText(f.name, f.v); err != nil {
				add("%s: %v", where, err)
			}
		}
		if e.Icon != "" && !idPattern.MatchString(e.Icon) {
			add("%s: icon %q must match %s", where, e.Icon, IDPattern)
		}
		switch {
		case e.Target == "" && len(e.Addresses) == 0:
			add("%s: needs a target or addresses", where)
		case e.Target != "" && len(e.Addresses) > 0:
			add("%s: sets both target and addresses", where)
		case e.Target == "" && e.ModerateTarget != "":
			add("%s: moderateTarget needs a target", where)
		}
		for _, t := range []string{e.Target, e.ModerateTarget} {
			if t != "" && !canonicalHost(t) {
				add("%s: target %q is not a lower-case host name", where, t)
			}
		}
		e.addrs = nil
		for _, a := range e.Addresses {
			ip, err := netip.ParseAddr(a)
			if err != nil || ip.Zone() != "" || ip.Is4In6() || ip.IsUnspecified() {
				add("%s: address %q is not a unicast IPv4 or IPv6 address", where, a)
				continue
			}
			e.addrs = append(e.addrs, ip)
		}
		if len(e.Domains) == 0 {
			add("%s: no domains", where)
		}
		domains += len(e.Domains)
		for _, d := range e.Domains {
			switch {
			case !canonicalHost(d):
				add("%s: domain %q is not a lower-case host name", where, d)
			case hosts[d] != "":
				add("%s: domain %q is already rewritten by %q", where, d, hosts[d])
			case d == e.Target || d == e.ModerateTarget:
				add("%s: domain %q is its own target", where, d)
			}
			hosts[d] = e.ID
		}
	}
	if domains > maxDomains {
		add("%d safe-search domains, over the limit of %d", domains, maxDomains)
	}
	if len(errs) > 0 {
		return fmt.Errorf("catalogue: invalid document: %w", errors.Join(errs...))
	}
	return nil
}

// checkRules requires every rule to compile to a block entry: no
// exceptions, no unsupported or invalid lines, no comments.
func checkRules(rules []string) error {
	for _, r := range rules {
		if strings.ContainsAny(r, "\n\r") {
			return fmt.Errorf("rule %q contains a line break", r)
		}
	}
	res, err := parser.Parse(strings.NewReader(strings.Join(rules, "\n")), parser.Options{Format: parser.FormatAdblock, KeepInvalid: 1, KeepUnsupported: 1}, func(r parser.Rule) error {
		if r.Exception {
			return fmt.Errorf("rule %q is an exception; services only block", r.String())
		}
		return nil
	})
	switch {
	case err != nil:
		return err
	case res.Invalid > 0:
		return fmt.Errorf("invalid rule %q: %s", res.InvalidSamples[0].Text, res.InvalidSamples[0].Reason)
	case res.Unsupported > 0:
		return fmt.Errorf("unsupported rule %q: %s", res.UnsupportedRules[0].Text, res.UnsupportedRules[0].Reason)
	case res.Rules != len(rules):
		return fmt.Errorf("%d of %d lines are rules (no comments or blank lines)", res.Rules, len(rules))
	}
	return nil
}

// canonicalHost reports whether s is a host name in the form query names
// are matched in: lower-case A-labels without a trailing dot.
func canonicalHost(s string) bool {
	n, err := parser.NormalizeDomain(s)
	return err == nil && n == s && strings.Contains(s, ".")
}

func (c *Catalogue) index() {
	slices.SortFunc(c.Services, func(a, b Service) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(c.SafeSearch, func(a, b Engine) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(c.Categories, func(a, b Category) int { return cmp.Or(cmp.Compare(a.Order, b.Order), strings.Compare(a.ID, b.ID)) })
	c.byCategory = make(map[string]int, len(c.Categories))
	for i, k := range c.Categories {
		c.byCategory[k.ID] = i
	}
	c.byService = make(map[string]int, len(c.Services))
	c.members = map[string][]string{}
	c.specs = make([]blocking.Spec, 0, len(c.Services))
	for i, s := range c.Services {
		c.byService[s.ID] = i
		c.members[s.Category] = append(c.members[s.Category], s.ID)
		c.specs = append(c.specs, blocking.Spec{
			Name: ListName(s.ID), Mode: blocking.ModeBlock,
			Sources: []blocking.Source{blocking.LinesSource("catalogue", parser.FormatAdblock, s.Rules)},
		})
	}
	c.byHost = map[string]int{}
	for i, e := range c.SafeSearch {
		for _, d := range e.Domains {
			c.byHost[d] = i
		}
	}
}

// digestOf is the lowercase hex SHA-256 of b.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ListName is the compiled list name of service id: "service:<id>".
func ListName(id string) string { return ServiceListPrefix + id }

// Digest is the lowercase hex SHA-256 of the document bytes (of the
// catalogue document alone when an import is merged in).
func (c *Catalogue) Digest() string { return c.digest }

// Fingerprint changes whenever the services in service change: the
// document digest, combined with the import's when one is merged in.
func (c *Catalogue) Fingerprint() string { return c.fingerprint }

// Imported is the AdGuard import merged into c, and how many of its
// services were dropped because the catalogue document has their ID; nil
// and 0 without an import.
func (c *Catalogue) Imported() (*Import, int) { return c.imported, c.shadowed }

// Origin is OriginEmbedded or the URL the document was fetched from.
func (c *Catalogue) Origin() string { return c.origin }

// Raw returns the document bytes.
func (c *Catalogue) Raw() []byte { return bytes.Clone(c.raw) }

// Service returns the service with id.
func (c *Catalogue) Service(id string) (Service, bool) {
	i, ok := c.byService[id]
	if !ok {
		return Service{}, false
	}
	return c.Services[i], true
}

// HasService reports whether id names a service.
func (c *Catalogue) HasService(id string) bool {
	_, ok := c.byService[id]
	return ok
}

// Category returns the category with id.
func (c *Catalogue) Category(id string) (Category, bool) {
	i, ok := c.byCategory[id]
	if !ok {
		return Category{}, false
	}
	return c.Categories[i], true
}

// HasCategory reports whether id names a category.
func (c *Catalogue) HasCategory(id string) bool {
	_, ok := c.byCategory[id]
	return ok
}

// CategoryServices returns the IDs of the services in category id, sorted,
// and whether the category exists. The slice is shared: do not modify it.
func (c *Catalogue) CategoryServices(id string) ([]string, bool) {
	if _, ok := c.byCategory[id]; !ok {
		return nil, false
	}
	return c.members[id], true
}

// Specs returns the compile specs of every service, one block-mode list
// "service:<id>" each, sorted by ID. The specs are shared: do not modify
// them.
func (c *Catalogue) Specs() []blocking.Spec { return slices.Clone(c.specs) }

// Rewrite is the safe-search answer for one question name.
type Rewrite struct {
	// Engine is the engine ID; List renders it for the query log.
	Engine string
	// Target is the CNAME target; empty when Addrs answer directly.
	Target string
	// Addrs are the addresses answered when there is no target.
	Addrs []netip.Addr
}

// List is the query-log list name of the rewrite: "safesearch:<engine>".
func (r Rewrite) List() string { return SafeSearchPrefix + r.Engine }

// RewriteFor returns the safe-search rewrite for qname (any case, with or without the
// trailing dot) in mode (ModeStrict or ModeModerate; anything else is
// strict). It does not allocate for an already lower-case name.
func (c *Catalogue) RewriteFor(qname, mode string) (Rewrite, bool) {
	if c == nil || len(c.byHost) == 0 {
		return Rewrite{}, false
	}
	i, ok := c.byHost[blocking.NormalizeName(qname)]
	if !ok {
		return Rewrite{}, false
	}
	e := &c.SafeSearch[i]
	r := Rewrite{Engine: e.ID, Target: e.Target, Addrs: e.addrs}
	if mode == ModeModerate && e.ModerateTarget != "" {
		r.Target = e.ModerateTarget
	}
	return r, true
}

var embedded = func() *Catalogue {
	c, err := Parse(embeddedJSON, OriginEmbedded)
	if err != nil {
		panic(fmt.Sprintf("catalogue: the embedded catalogue.json is invalid: %v", err))
	}
	knownCategories = c.Categories
	return c
}()

// knownCategories are the embedded catalogue's categories, the names a
// version 1 document's category IDs get; set once at start-up.
var knownCategories []Category

// Embedded returns the catalogue built into the binary.
func Embedded() *Catalogue { return embedded }

// License returns the licence text of the catalogue data.
func License() string { return licenseText }

// Provenance returns the provenance file of the catalogue data.
func Provenance() string { return provenanceText }
