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

// SchemaVersion is the catalogue document schema this build reads. A
// document with any other schemaVersion is refused, and the catalogue in
// service (the embedded one, or the last good download) stays.
const SchemaVersion = 1

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
	maxServices = 1000
	maxRules    = 1000 // per service
	maxEngines  = 100
	maxDomains  = 10000 // safe-search host names over all engines
	maxText     = 256
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

// IDPattern is the pattern service IDs match (the CRD items pattern of
// Policy.spec.blockedServices).
const IDPattern = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`

// Service is one blockable service.
type Service struct {
	// ID is the stable identifier Policy.spec.blockedServices names.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Category groups services in the UI: video, social, messaging,
	// gaming, music, ai, dating, ...
	Category string `json:"category"`
	// Rules are adblock-style DNS rules, "||domain^" (the domain and its
	// subdomains).
	Rules []string `json:"rules"`
	// Source is where the entry comes from: a vendor documentation URL or
	// "first-party domains of the service".
	Source string `json:"source"`
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

	addrs []netip.Addr
}

// Catalogue is one parsed, validated catalogue document. It is immutable.
type Catalogue struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
	License       string `json:"license"`
	Homepage      string `json:"homepage,omitempty"`
	// Services are sorted by ID.
	Services []Service `json:"services"`
	// SafeSearch engines are sorted by ID.
	SafeSearch []Engine `json:"safeSearch"`

	raw       []byte
	digest    string
	origin    string
	byService map[string]int
	byHost    map[string]int
	specs     []blocking.Spec
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
	case *head.SchemaVersion != SchemaVersion:
		return nil, fmt.Errorf("%w %d (this build reads %d)", ErrSchemaVersion, *head.SchemaVersion, SchemaVersion)
	}
	c := &Catalogue{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("catalogue: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	c.raw, c.digest, c.origin = bytes.Clone(b), digestOf(b), origin
	c.index()
	return c, nil
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
		if !idPattern.MatchString(s.Category) {
			add("%s: category %q must match %s", where, s.Category, IDPattern)
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
	c.byService = make(map[string]int, len(c.Services))
	c.specs = make([]blocking.Spec, 0, len(c.Services))
	for i, s := range c.Services {
		c.byService[s.ID] = i
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

// Digest is the lowercase hex SHA-256 of the document bytes.
func (c *Catalogue) Digest() string { return c.digest }

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
	return c
}()

// Embedded returns the catalogue built into the binary.
func Embedded() *Catalogue { return embedded }

// License returns the licence text of the catalogue data.
func License() string { return licenseText }

// Provenance returns the provenance file of the catalogue data.
func Provenance() string { return provenanceText }
