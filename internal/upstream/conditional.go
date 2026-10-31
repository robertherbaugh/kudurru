package upstream

import (
	"fmt"
	"io"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// suffixMap routes a query name to a group by longest matching suffix on
// label boundaries.
type suffixMap struct {
	m map[string]string // lower-case FQDN suffix → group
}

// newSuffixMap normalises the conditional map. Keys are domain suffixes or
// dnsmasq lists "[/a.example/b.example/]"; every group must exist.
func newSuffixMap(conditional map[string]string, groups map[string]GroupConfig) (*suffixMap, error) {
	sm := &suffixMap{m: make(map[string]string, len(conditional))}
	for key, group := range conditional {
		if _, ok := groups[group]; !ok {
			return nil, fmt.Errorf("upstream: conditional %q: unknown group %q", key, group)
		}
		domains, err := conditionalKey(key)
		if err != nil {
			return nil, err
		}
		for _, d := range domains {
			if prev, dup := sm.m[d]; dup && prev != group {
				return nil, fmt.Errorf("upstream: conditional suffix %q maps to both %q and %q", d, prev, group)
			}
			sm.m[d] = group
		}
	}
	return sm, nil
}

// conditionalKey expands one conditional-map key into normalised suffixes.
func conditionalKey(key string) ([]string, error) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "[/") {
		domains, rest, err := parseBracket(key)
		if err != nil {
			return nil, err
		}
		if rest != "" {
			return nil, fmt.Errorf("upstream: conditional key %q: unexpected %q after the domain list", key, rest)
		}
		return domains, nil
	}
	d, err := normaliseSuffix(key)
	if err != nil {
		return nil, err
	}
	return []string{d}, nil
}

// normaliseSuffix lower-cases a domain and makes it fully qualified.
func normaliseSuffix(d string) (string, error) {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return "", fmt.Errorf("upstream: empty conditional domain")
	}
	if strings.ContainsAny(d, " /[]#") {
		return "", fmt.Errorf("upstream: invalid conditional domain %q", d)
	}
	if !strings.HasSuffix(d, ".") {
		d += "."
	}
	if strings.Contains(d, "..") {
		return "", fmt.Errorf("upstream: invalid conditional domain %q", d)
	}
	return d, nil
}

// parseBracket reads "[/a/b/]rest" and returns the normalised domains and
// the trimmed rest.
func parseBracket(s string) ([]string, string, error) {
	end := -1
	if strings.HasPrefix(s, "[/") {
		end = strings.Index(s[1:], "/]") + 1 // index of "/]" after the opening "[/"
	}
	if end < 2 {
		return nil, "", fmt.Errorf("upstream: %q: want [/domain/.../]", s)
	}
	var out []string
	for _, part := range strings.Split(s[2:end], "/") {
		if part == "" {
			continue
		}
		d, err := normaliseSuffix(part)
		if err != nil {
			return nil, "", err
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("upstream: %q lists no domain", s)
	}
	return out, strings.TrimSpace(s[end+2:]), nil
}

// lookup returns the group of the longest suffix of qname, if any.
func (sm *suffixMap) lookup(qname string) (string, bool) {
	if sm == nil || len(sm.m) == 0 {
		return "", false
	}
	name := strings.ToLower(qname)
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	for {
		if g, ok := sm.m[name]; ok {
			return g, true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 || i == len(name)-1 {
			break
		}
		name = name[i+1:]
	}
	g, ok := sm.m["."]
	return g, ok
}

// Import is the result of ImportDnsmasq: groups and conditional entries to
// merge into a Config.
type Import struct {
	// Issues reports rules with no supported forwarding representation.
	Issues []config.DnsmasqIssue
	// Groups holds one strict group per distinct server list, named
	// "dnsmasq-1", "dnsmasq-2", ... in order of first appearance.
	Groups map[string]GroupConfig
	// Conditional maps suffixes to those groups, or to DefaultGroup for
	// "use the default servers" entries ("#").
	Conditional map[string]string
	// Default lists servers given without a domain.
	Default []string
}

// ImportDnsmasq reads conditional-forwarding rules in the two common
// dnsmasq-derived syntaxes, one per line:
//
//	server=/corp.example/10.in-addr.arpa/10.0.0.5#5353   (dnsmasq)
//	[/corp.example/10.in-addr.arpa/]10.0.0.5:5353 tls://10.0.0.6   (dnsproxy)
//
// "#" as the server means "use the default group". Plain server lines
// ("server=1.1.1.1" or "tls://1.1.1.1") become Default. Blank lines and
// lines starting with "#" are ignored. Unsupported directives, including
// local-answer rules, are reported in Issues and prevent Merge.
func ImportDnsmasq(r io.Reader) (*Import, error) {
	parsed, err := config.ParseDnsmasq(r)
	if err != nil {
		return nil, err
	}
	out := &Import{Groups: map[string]GroupConfig{}, Conditional: parsed.Conditional, Default: parsed.Default, Issues: parsed.Issues}
	for name, group := range parsed.Groups {
		out.Groups[name] = GroupConfig{Servers: group.Servers, Strategy: Strategy(group.Strategy)}
	}
	return out, nil
}

// Merge adds the import to c: groups (an existing group of the same name is
// an error), conditional entries and, when c has no default group yet, a
// parallel_best default group from Default.
func (imp *Import) Merge(c *Config) error {
	if len(imp.Issues) > 0 {
		return fmt.Errorf("upstream: import has %d unsupported directives; no rules applied", len(imp.Issues))
	}
	if c.Groups == nil {
		c.Groups = map[string]GroupConfig{}
	}
	if c.Conditional == nil {
		c.Conditional = map[string]string{}
	}
	for name, g := range imp.Groups {
		if _, ok := c.Groups[name]; ok {
			return fmt.Errorf("upstream: imported group %q already exists", name)
		}
		c.Groups[name] = g
	}
	if _, ok := c.Groups[DefaultGroup]; !ok && len(imp.Default) > 0 {
		c.Groups[DefaultGroup] = GroupConfig{Servers: imp.Default, Strategy: ParallelBest}
	}
	for d, g := range imp.Conditional {
		c.Conditional[d] = g
	}
	return nil
}
