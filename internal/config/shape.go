package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The typed-control metadata of a key (ux-plan contract C1): closed sets,
// bounds, formats, references and the shape of list and map values. The UI
// renders a control from it, `kudurru config schema` and the settings
// reference print it, and Validate enforces it, so the three agree.

// TypeObject is the type of a nested object with named fields; it appears
// only inside shapes (a list element or a map value), never as a key type.
const TypeObject Type = "object"

// Option is one value of a closed set.
type Option struct {
	Value string
	Label string
	Help  string
}

// Format names the syntax of a string value.
type Format string

// Formats. An empty value is never checked against its format: empty
// means unset for every key that has one.
const (
	FormatIP   Format = "ip"
	FormatIPv4 Format = "ipv4"
	FormatIPv6 Format = "ipv6"
	// FormatCIDR is a CIDR prefix or a single address (10.0.0.0/8,
	// 192.0.2.1).
	FormatCIDR Format = "cidr"
	// FormatHostPort is host:port; the host may be empty (":53").
	FormatHostPort Format = "hostport"
	// FormatURL is an absolute http or https URL.
	FormatURL    Format = "url"
	FormatDomain Format = "domain"
	// FormatUpstream is an upstream server in dnsproxy syntax (ip[:port],
	// udp://, tcp://, tls://, https://, quic://, h3://, sdns://) with an
	// optional "name=" prefix.
	FormatUpstream Format = "upstream"
	FormatEmail    Format = "email"
	// FormatDateTime is an absolute RFC3339 timestamp with a time zone.
	FormatDateTime Format = "date-time"
)

// Ref names the kind of object a value refers to.
type Ref string

// References.
const (
	RefUpstreamGroup Ref = "upstreamGroup"
	RefClientGroup   Ref = "clientGroup"
	RefBlockList     Ref = "blockList"
	RefPolicy        Ref = "policy"
	RefZone          Ref = "zone"
)

// Shape describes a value nested in a key: the element of a list (Items),
// the keys and values of a map (Keys, Values) or the members of an object
// (Fields). A Key carries the same constraints for its own value.
type Shape struct {
	// Help describes this nested value for operators; it does not affect validation.
	Help string
	Type Type
	// Enum is the closed set of a string value.
	Enum []Option
	// Min and Max bound an int value (inclusive).
	Min *int
	Max *int
	// Presets are suggested values (durations, byte sizes); any valid
	// value is accepted.
	Presets []string
	Format  Format
	Ref     Ref
	Items   *Shape
	Keys    *Shape
	Values  *Shape
	Fields  []Field
	// Default is what an omitted or empty value means; an empty value is
	// accepted when it is set. Only nested shapes use it (a key's default
	// is Key.Default).
	Default any
	// normalize maps a string value before the enum test (case-insensitive
	// names, a log level with per-role overrides).
	normalize func(string) string
}

// Field is one member of an object shape.
type Field struct {
	Name  string
	Label string
	Help  string
	// Required fields must be present and non-empty.
	Required bool
	Shape
}

// Shape returns the constraints of the key's own value.
func (k Key) Shape() Shape {
	return Shape{
		Type: k.Type, Enum: k.Enum, Min: k.Min, Max: k.Max, Presets: k.Presets, Format: k.Format, Ref: k.Ref,
		Items: k.Items, Keys: k.Keys, Values: k.Values, Fields: k.Fields, normalize: k.normalize,
	}
}

// Describe summarizes the constraints in plain text for `kudurru config
// schema` and the settings reference ("one of strip | forward | add",
// "range 0-32", "format cidr", the element, key and value shapes); it is
// empty when the value is unconstrained.
func (s Shape) Describe() string {
	parts := s.constraints(false)
	if s.Items != nil && s.Items.constrained() {
		parts = append(parts, "items: "+s.Items.nested())
	}
	if s.Keys != nil && s.Keys.constrained() {
		parts = append(parts, "keys: "+s.Keys.nested())
	}
	if s.Values != nil && s.Values.constrained() {
		parts = append(parts, "values: "+s.Values.nested())
	}
	if len(s.Fields) > 0 {
		parts = append(parts, "fields: "+s.nested())
	}
	return strings.Join(parts, "; ")
}

// constraints are the scalar constraints of s; nested shapes also show
// their default.
func (s *Shape) constraints(withDefault bool) []string {
	var out []string
	if len(s.Enum) > 0 {
		out = append(out, "one of "+strings.Join(s.EnumValues(), " | "))
	}
	switch {
	case s.Min != nil && s.Max != nil:
		out = append(out, fmt.Sprintf("range %d-%d", *s.Min, *s.Max))
	case s.Min != nil:
		out = append(out, fmt.Sprintf("at least %d", *s.Min))
	case s.Max != nil:
		out = append(out, fmt.Sprintf("at most %d", *s.Max))
	}
	if s.Format != "" {
		out = append(out, "format "+string(s.Format))
	}
	if s.Ref != "" {
		out = append(out, "ref "+string(s.Ref))
	}
	if withDefault && s.Default != nil {
		d, ok := s.Default.(string)
		if !ok {
			d = toText(s.Default)
		}
		out = append(out, "default "+d)
	}
	return out
}

func (s *Shape) constrained() bool {
	return len(s.constraints(true)) > 0 || len(s.Fields) > 0 || (s.Items != nil && s.Items.constrained()) ||
		(s.Keys != nil && s.Keys.constrained()) || (s.Values != nil && s.Values.constrained())
}

// nested describes a shape inside a key: "string (format upstream)",
// "list of string (one of udp | tcp)", "{address (required): ...; path: ...}".
func (s *Shape) nested() string {
	var base string
	switch {
	case len(s.Fields) > 0:
		fields := make([]string, len(s.Fields))
		for i := range s.Fields {
			f := &s.Fields[i]
			name := f.Name
			if f.Required {
				name += " (required)"
			}
			fields[i] = name + ": " + f.nested()
		}
		base = "{" + strings.Join(fields, "; ") + "}"
	case (s.Type == TypeList || s.Type == TypeStringList) && s.Items != nil:
		base = "list of " + s.Items.nested()
	case s.Type == TypeMap && s.Values != nil:
		key := "string"
		if s.Keys != nil {
			key = s.Keys.nested()
		}
		base = "map of " + key + " to " + s.Values.nested()
	default:
		base = string(s.Type)
	}
	if c := s.constraints(true); len(c) > 0 {
		base += " (" + strings.Join(c, ", ") + ")"
	}
	return base
}

// EnumValues returns the values of the closed set.
func (s Shape) EnumValues() []string {
	out := make([]string, len(s.Enum))
	for i, o := range s.Enum {
		out[i] = o.Value
	}
	return out
}

// FieldError is a value that violates its key's metadata. Path is the key
// with the position inside it (upstreams.groups[lan].strategy,
// listeners.dns[0].address); Allowed is the closed set when the value is
// not in it.
type FieldError struct {
	Path    string
	Message string
	Allowed []string
}

// Error implements error.
func (e *FieldError) Error() string {
	if e.Message == msgRequired {
		return e.Path + " " + msgRequired
	}
	return e.Path + ": " + e.Message
}

const msgRequired = "is required"

// FieldErrors returns every FieldError inside err (errors.Join trees).
func FieldErrors(err error) []*FieldError {
	var out []*FieldError
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		switch u := err.(type) { //nolint:errorlint // walking the tree on purpose
		case *FieldError:
			out = append(out, u)
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				walk(e)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// validateSchema checks every key of c against its metadata, on the JSON
// form of the config (the form the API and the settings file use).
func (c *Config) validateSchema() []error {
	raw, err := json.Marshal(c)
	if err != nil {
		return []error{fmt.Errorf("config: %w", err)}
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return []error{fmt.Errorf("config: %w", err)}
	}
	var errs []error
	for _, k := range Schema {
		v, ok := lookupTree(tree, k.Path)
		if !ok {
			continue
		}
		s := k.Shape()
		s.check(k.Path, v, &errs)
	}
	return errs
}

func lookupTree(tree map[string]any, path string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// Check validates v, a value in its JSON form (strings, float64 numbers,
// []any, map[string]any), against s; errors name path.
func (s *Shape) Check(path string, v any) error {
	var errs []error
	s.check(path, v, &errs)
	return errors.Join(errs...)
}

func (s *Shape) check(path string, v any, errs *[]error) {
	fail := func(format string, args ...any) {
		*errs = append(*errs, &FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if v == nil {
		return
	}
	switch s.Type {
	case TypeString:
		str, ok := v.(string)
		if !ok {
			fail("%s is not a string", toText(v))
			return
		}
		if str == "" && s.Default != nil {
			return
		}
		if len(s.Enum) > 0 {
			probe := str
			if s.normalize != nil {
				probe = s.normalize(str)
			}
			if !s.inEnum(probe) {
				allowed := s.EnumValues()
				*errs = append(*errs, &FieldError{Path: path, Message: fmt.Sprintf("unknown value %q (%s)", str, strings.Join(allowed, ", ")), Allowed: allowed})
				return
			}
		}
		if str != "" && s.Format != "" {
			if msg := checkFormat(s.Format, str); msg != "" {
				fail("%q %s", str, msg)
			}
		}
	case TypeInt:
		n, ok := toInt(v)
		if !ok {
			fail("%s is not an integer", toText(v))
			return
		}
		if msg := s.checkBounds(n); msg != "" {
			fail("%d %s", n, msg)
		}
	case TypeStringList, TypeList:
		list, ok := v.([]any)
		if !ok {
			fail("is not a list")
			return
		}
		if s.Items != nil {
			for i, e := range list {
				s.Items.check(fmt.Sprintf("%s[%d]", path, i), e, errs)
			}
		}
	case TypeMap:
		m, ok := v.(map[string]any)
		if !ok {
			fail("is not a map")
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s.Keys != nil {
				s.Keys.checkKey(path, k, errs)
			}
			if s.Values != nil {
				s.Values.check(path+"["+k+"]", m[k], errs)
			}
		}
	case TypeObject:
		m, ok := v.(map[string]any)
		if !ok {
			fail("is not an object")
			return
		}
		for i := range s.Fields {
			f := &s.Fields[i]
			fv, present := m[f.Name]
			if !present || isEmpty(fv) {
				if f.Required {
					*errs = append(*errs, &FieldError{Path: path + "." + f.Name, Message: msgRequired})
					continue
				}
				if !present {
					continue
				}
			}
			f.check(path+"."+f.Name, fv, errs)
		}
	}
}

// checkKey validates a map key (ratelimit prefix lengths).
func (s *Shape) checkKey(path, key string, errs *[]error) {
	if s.Type != TypeInt {
		s.check(path, key, errs)
		return
	}
	n, err := strconv.Atoi(key)
	if err != nil || s.checkBounds(n) != "" {
		*errs = append(*errs, &FieldError{Path: path, Message: fmt.Sprintf("key %q is not a number %s", key, s.boundsText())})
	}
}

func (s *Shape) inEnum(v string) bool {
	for _, o := range s.Enum {
		if o.Value == v {
			return true
		}
	}
	return false
}

func (s *Shape) checkBounds(n int) string {
	switch {
	case s.Min != nil && s.Max != nil && (n < *s.Min || n > *s.Max):
		return fmt.Sprintf("is outside %d-%d", *s.Min, *s.Max)
	case s.Min != nil && n < *s.Min && *s.Min == 0:
		return "is negative"
	case s.Min != nil && n < *s.Min:
		return fmt.Sprintf("is less than %d", *s.Min)
	case s.Max != nil && n > *s.Max:
		return fmt.Sprintf("is greater than %d", *s.Max)
	}
	return ""
}

func (s *Shape) boundsText() string {
	switch {
	case s.Min != nil && s.Max != nil:
		return fmt.Sprintf("in %d-%d", *s.Min, *s.Max)
	case s.Min != nil:
		return fmt.Sprintf("of at least %d", *s.Min)
	case s.Max != nil:
		return fmt.Sprintf("of at most %d", *s.Max)
	}
	return ""
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || math.Abs(n) > 1<<53 {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func toText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// checkFormat returns why v does not match f, or "".
func checkFormat(f Format, v string) string {
	switch f {
	case FormatIP:
		if a, err := netip.ParseAddr(v); err != nil || a.Zone() != "" {
			return "is not an IP address"
		}
	case FormatIPv4:
		if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
			return "is not an IPv4 address"
		}
	case FormatIPv6:
		if a, err := netip.ParseAddr(v); err != nil || !a.Is6() || a.Is4In6() {
			return "is not an IPv6 address"
		}
	case FormatCIDR:
		if _, err := ParsePrefixOrAddr(v); err != nil {
			return "is neither a CIDR nor an address"
		}
	case FormatHostPort:
		if !isHostPort(v) {
			return "is not host:port (e.g. 0.0.0.0:53, [::]:53 or :53)"
		}
	case FormatURL:
		if p, err := url.Parse(v); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return "is not an http or https URL"
		}
	case FormatDomain:
		if !IsDomain(v) {
			return "is not a domain name"
		}
	case FormatUpstream:
		if !isUpstream(v) {
			return "is not an upstream server (ip[:port], tls://host, https://host/dns-query, quic://host, h3://host or sdns://stamp, optionally name=...)"
		}
	case FormatEmail:
		if a, err := mail.ParseAddress(v); err != nil || a.Address != v {
			return "is not an email address"
		}
	case FormatDateTime:
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			return "is not an absolute RFC3339 timestamp"
		}
	}
	return ""
}

func isHostPort(v string) bool {
	host, port, err := net.SplitHostPort(v)
	if err != nil || strings.ContainsAny(host, " /") {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}

// IsDomain reports whether v is a DNS name: dot-separated labels of 1-63
// letters, digits, hyphens or underscores, 253 characters at most, with
// an optional trailing dot.
func IsDomain(v string) bool {
	v = strings.TrimSuffix(v, ".")
	if v == "" || len(v) > 253 {
		return false
	}
	for _, label := range strings.Split(v, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return false
			}
		}
	}
	return true
}

// upstreamSchemes are the dnsproxy address schemes.
var upstreamSchemes = map[string]bool{"udp": true, "tcp": true, "tls": true, "https": true, "quic": true, "h3": true, "sdns": true}

// isUpstream accepts what internal/upstream hands to dnsproxy: an
// optional display name ("cloudflare=") and an address.
func isUpstream(v string) bool {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '='); i > 0 && !strings.ContainsAny(v[:i], ":/[]") {
		v = strings.TrimSpace(v[i+1:])
	}
	if scheme, rest, ok := strings.Cut(v, "://"); ok {
		if !upstreamSchemes[scheme] || rest == "" {
			return false
		}
		if scheme == "sdns" {
			return true
		}
		p, err := url.Parse(v)
		return err == nil && p.Host != ""
	}
	if _, err := netip.ParseAddr(v); err == nil {
		return true
	}
	if isHostPort(v) {
		host, _, _ := net.SplitHostPort(v)
		return host != ""
	}
	return IsDomain(v)
}
