package upstream

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
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
// lines starting with "#" are ignored; dnsmasq's "local=/x/" and
// "server=/x/" (answer locally) have no forwarding meaning and are skipped.
func ImportDnsmasq(r io.Reader) (*Import, error) {
	imp := &Import{Groups: map[string]GroupConfig{}, Conditional: map[string]string{}}
	byServers := map[string]string{}
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "local=") {
			continue
		}
		var domains, servers []string
		switch {
		case strings.HasPrefix(line, "server="):
			v := strings.TrimPrefix(line, "server=")
			if !strings.HasPrefix(v, "/") {
				imp.Default = append(imp.Default, dnsmasqServer(v))
				continue
			}
			last := strings.LastIndexByte(v, '/')
			if last == 0 {
				return nil, fmt.Errorf("upstream: dnsmasq line %d: unterminated domain list", lineNo)
			}
			ds, _, err := parseBracket("[" + v[:last+1] + "]")
			if err != nil {
				return nil, fmt.Errorf("upstream: dnsmasq line %d: %w", lineNo, err)
			}
			domains = ds
			if s := strings.TrimSpace(v[last+1:]); s != "" {
				servers = []string{dnsmasqServer(s)}
			}
		case strings.HasPrefix(line, "[/"):
			ds, rest, err := parseBracket(line)
			if err != nil {
				return nil, fmt.Errorf("upstream: dnsmasq line %d: %w", lineNo, err)
			}
			domains, servers = ds, strings.Fields(rest)
		default:
			imp.Default = append(imp.Default, strings.Fields(line)...)
			continue
		}
		if len(servers) == 0 {
			continue // answer-locally rule; nothing to forward
		}
		group := DefaultGroup
		if len(servers) != 1 || servers[0] != "#" {
			key := strings.Join(servers, " ")
			g, ok := byServers[key]
			if !ok {
				g = fmt.Sprintf("dnsmasq-%d", len(byServers)+1)
				byServers[key] = g
				imp.Groups[g] = GroupConfig{Servers: servers, Strategy: Strict}
			}
			group = g
		}
		for _, d := range domains {
			imp.Conditional[d] = group
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return imp, nil
}

// dnsmasqServer turns dnsmasq's ip#port into ip:port; "#" alone is kept.
func dnsmasqServer(s string) string {
	s = strings.TrimSpace(s)
	if s == "#" || strings.Contains(s, "://") {
		return s
	}
	if host, port, ok := strings.Cut(s, "#"); ok && host != "" {
		return net.JoinHostPort(host, port)
	}
	return s
}

// Merge adds the import to c: groups (an existing group of the same name is
// an error), conditional entries and, when c has no default group yet, a
// parallel_best default group from Default.
func (imp *Import) Merge(c *Config) error {
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
