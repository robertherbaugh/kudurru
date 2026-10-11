package domainlist

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// Action is what a list does with its entries: allow (the Allowlist) or
// deny (the Denylist).
type Action string

// Actions.
const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Mode is the BlockList mode of the action's lists.
func (a Action) Mode() v1alpha1.ListMode {
	if a == Allow {
		return "allow"
	}
	return "block"
}

// ParseAction validates an action.
func ParseAction(s string) (Action, error) {
	switch Action(s) {
	case Allow, Deny:
		return Action(s), nil
	}
	return "", fmt.Errorf("%w: action %q (allow or deny)", ErrInvalid, s)
}

// Names of the lists and of the source that holds the entries.
const (
	// AllowList and DenyList hold the entries for everyone.
	AllowList = "ui-allow"
	DenyList  = "ui-deny"
	// SourceName is the source of a list that holds the entries.
	SourceName = "ui"
	// unconvertedSource keeps legacy inline lines of a ui list that are not
	// a domain, wildcard or regex rule (such as @@ exceptions or hosts
	// lines with several names), so a migration loses nothing.
	unconvertedSource = "ui-unconverted"
)

// ListName is the list that holds the entries of action for group (a
// ClientGroup name in the list's namespace), or for everyone when group is
// empty: ui-allow, ui-deny, ui-allow-<group>, ui-deny-<group>.
func ListName(a Action, group string) string {
	base := DenyList
	if a == Allow {
		base = AllowList
	}
	if group == "" {
		return base
	}
	return base + "-" + group
}

// ParseListName is the inverse of ListName; ok is false for other lists.
func ParseListName(name string) (a Action, group string, ok bool) {
	for _, act := range []Action{Allow, Deny} {
		base := ListName(act, "")
		switch {
		case name == base:
			return act, "", true
		case strings.HasPrefix(name, base+"-") && len(name) > len(base)+1:
			return act, name[len(base)+1:], true
		}
	}
	return "", "", false
}

// Errors.
var (
	// ErrInvalid is a value, type or scope the caller must fix (400/422).
	ErrInvalid = errors.New("domainlist: invalid")
	// ErrNoPolicy is a group scope no policy covers (422).
	ErrNoPolicy = errors.New("domainlist: no policy covers the client group")
	// ErrNotFound is an unknown entry (404).
	ErrNotFound = errors.New("domainlist: no such entry")
)

// maxValueBytes bounds a value.
const maxValueBytes = 1024

// ParseType validates an entry type; "" is exact.
func ParseType(s string) (v1alpha1.EntryType, error) {
	switch t := v1alpha1.EntryType(s); t {
	case "":
		return v1alpha1.EntryExact, nil
	case v1alpha1.EntryExact, v1alpha1.EntryWildcard, v1alpha1.EntryRegex:
		return t, nil
	}
	return "", fmt.Errorf("%w: type %q (exact, wildcard or regex)", ErrInvalid, s)
}

// Normalize validates value as an entry of type t and returns its
// canonical form: the lower-case name for exact, "*.name" for wildcard
// (which also accepts "name" and "||name^"), the RE2 expression without
// slashes for regex. The value must compile into exactly the rule the type
// promises, so what the list shows is what the resolver applies.
func Normalize(t v1alpha1.EntryType, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch {
	case v == "":
		return "", fmt.Errorf("%w: empty value", ErrInvalid)
	case len(v) > maxValueBytes:
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalid, maxValueBytes)
	case strings.ContainsAny(v, "\r\n"):
		return "", fmt.Errorf("%w: %q contains a line break", ErrInvalid, v)
	}
	var line string
	var kind parser.Kind
	switch t {
	case v1alpha1.EntryRegex:
		re := v
		if len(re) > 1 && strings.HasPrefix(re, "/") && strings.HasSuffix(re, "/") {
			re = re[1 : len(re)-1]
		}
		if re == "" {
			return "", fmt.Errorf("%w: empty expression", ErrInvalid)
		}
		if _, err := regexp.Compile(re); err != nil {
			return "", fmt.Errorf("%w: %q is not a valid RE2 expression: %v", ErrInvalid, re, err)
		}
		line, kind = "/"+re+"/", parser.KindRegex
	case v1alpha1.EntryWildcard:
		n := strings.TrimSuffix(strings.TrimPrefix(v, "||"), "^")
		n = strings.TrimPrefix(n, "*.")
		if strings.ContainsAny(n, "*|^/") || n == "" {
			return "", fmt.Errorf("%w: %q is not a wildcard (example: *.example.com)", ErrInvalid, v)
		}
		line, kind = "*."+n, parser.KindSuffix
	default:
		if strings.HasPrefix(v, "*.") || strings.HasPrefix(v, "||") {
			return "", fmt.Errorf("%w: %q matches subdomains; use type wildcard", ErrInvalid, v)
		}
		if strings.ContainsAny(v, "*|^/ ") {
			return "", fmt.Errorf("%w: %q is not a domain name (example: ads.example.com)", ErrInvalid, v)
		}
		line, kind = v, parser.KindExact
	}
	r, err := parseOne(line, parser.FormatAdblock)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %s", ErrInvalid, v, err.Error())
	}
	if r.Kind != kind || r.Exception {
		what := "a domain name (example: ads.example.com)"
		if t == v1alpha1.EntryRegex {
			what = "a regular expression the resolver can use"
		}
		return "", fmt.Errorf("%w: %q is not %s", ErrInvalid, v, what)
	}
	switch t {
	case v1alpha1.EntryRegex:
		return r.Pattern, nil
	case v1alpha1.EntryWildcard:
		return "*." + r.Pattern, nil
	}
	return r.Pattern, nil
}

// parseOne parses a single rule line.
func parseOne(line string, format parser.Format) (parser.Rule, error) {
	var rules []parser.Rule
	res, err := parser.Parse(strings.NewReader(line), parser.Options{Format: format, KeepInvalid: 1, KeepUnsupported: 1}, func(r parser.Rule) error {
		r.Pattern = strings.Clone(r.Pattern)
		rules = append(rules, r)
		return nil
	})
	switch {
	case err != nil:
		return parser.Rule{}, err
	case res.Invalid > 0:
		reason := "not a valid rule"
		if len(res.InvalidSamples) > 0 {
			reason = res.InvalidSamples[0].Reason
		}
		return parser.Rule{}, errors.New(reason)
	case res.Unsupported > 0:
		return parser.Rule{}, errors.New("uses a feature this version does not apply")
	case len(rules) != 1:
		return parser.Rule{}, errors.New("not exactly one rule")
	}
	return rules[0], nil
}

// FromRule maps a legacy inline line of a ui list ("example.com",
// "*.example.com", "||example.com^", "/re/") to a structured entry; ok is
// false for lines that are none of these (comments, exceptions, IP rules,
// hosts lines with several names), which a migration keeps as they are.
func FromRule(line string, format parser.Format) (t v1alpha1.EntryType, value string, ok bool) {
	if format == "" || format == parser.FormatHosts || format == parser.FormatDomains {
		format = parser.FormatAuto
	}
	r, err := parseOne(strings.TrimSpace(line), format)
	if err != nil || r.Exception {
		return "", "", false
	}
	switch r.Kind {
	case parser.KindExact:
		return v1alpha1.EntryExact, r.Pattern, true
	case parser.KindSuffix:
		return v1alpha1.EntryWildcard, "*." + r.Pattern, true
	case parser.KindRegex:
		return v1alpha1.EntryRegex, r.Pattern, true
	}
	return "", "", false
}

// RuleKey is a comparison key for a rule written as an entry or as a
// legacy line: "||x^" and "*.x" have the same key. Lines FromRule does not
// understand are their own key.
func RuleKey(rule string) string {
	if t, v, ok := FromRule(rule, ""); ok {
		return string(t) + "\x00" + v
	}
	return "line\x00" + rule
}

// ID is the opaque identifier of the logical entry (namespace, type,
// value) in the API.
func ID(namespace string, t v1alpha1.EntryType, value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(namespace + "\n" + string(t) + "\n" + value))
}

// ParseID is the inverse of ID.
func ParseID(id string) (namespace string, t v1alpha1.EntryType, value string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: id %q is not an entry id", ErrNotFound, id)
	}
	parts := strings.SplitN(string(b), "\n", 3)
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("%w: id %q is not an entry id", ErrNotFound, id)
	}
	if t, err = ParseType(parts[1]); err != nil {
		return "", "", "", fmt.Errorf("%w: id %q is not an entry id", ErrNotFound, id)
	}
	return parts[0], t, parts[2], nil
}

// MatchName is the name a cache flush and the block-list search use for
// an entry: the name of an exact entry, the base name of a wildcard; ""
// for regex.
func MatchName(t v1alpha1.EntryType, value string) string {
	switch t {
	case v1alpha1.EntryExact:
		return value
	case v1alpha1.EntryWildcard:
		return strings.TrimPrefix(value, "*.")
	}
	return ""
}
