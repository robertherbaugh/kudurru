package importers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// CorefileSyntax names the grammar the CoreDNS importer reads (D392).
const CorefileSyntax = "CoreDNS 1.12 Corefile"

// PreviewCoreDNS converts a documented subset of a CoreDNS Corefile. It never
// reads zone or hosts files, expands import globs, resolves environment
// placeholders or contacts upstream servers.
func PreviewCoreDNS(ctx context.Context, req OfflineRequest, r io.Reader) (*Plan, error) {
	b, err := readOffline(ctx, r)
	if err != nil {
		return nil, err
	}
	cf, err := parseCorefile(ctx, string(b))
	if err != nil {
		return nil, err
	}
	req, p, err := newOfflinePlan("coredns", req)
	if err != nil {
		return nil, err
	}
	p.Warnings = append(p.Warnings, "Read as "+CorefileSyntax+" syntax: server blocks, comments, quoted tokens and snippet imports. Converted plugins: forward, hosts (inline entries), rewrite (exact name), cache and acl; file and auto zones are listed for a separate BIND import. Every other plugin is named below and not converted.")
	c := &corednsConverter{ctx: ctx, req: req, p: p, groups: map[string]any{}, conditional: map[string]string{}, rewrites: map[string]public.Rewrite{}, served: map[string]bool{}}
	if err := c.convert(cf); err != nil {
		return nil, err
	}
	return finishOfflinePlan(ctx, p)
}

// corednsPlugins lists documented CoreDNS plugin names. Only these names are
// echoed in the loss report; any other directive is counted with its name
// redacted.
var corednsPlugins = map[string]bool{
	"acl": true, "any": true, "auto": true, "autopath": true, "azure": true, "bind": true, "bufsize": true, "cache": true,
	"cancel": true, "chaos": true, "clouddns": true, "debug": true, "dns64": true, "dnssec": true, "dnstap": true,
	"erratic": true, "errors": true, "etcd": true, "file": true, "forward": true, "geoip": true, "grpc": true,
	"grpc_server": true, "header": true, "health": true, "hosts": true, "https": true, "https3": true, "k8s_external": true, "kubernetes": true,
	"loadbalance": true, "local": true, "log": true, "loop": true, "metadata": true, "minimal": true,
	"multisocket": true, "nomad": true, "nsid": true, "on": true, "pprof": true, "prometheus": true, "quic": true, "ready": true,
	"reload": true, "rewrite": true, "root": true, "route53": true, "secondary": true, "sign": true, "template": true,
	"timeouts": true, "tls": true, "trace": true, "transfer": true, "tsig": true, "view": true, "whoami": true,
}

type corednsZone struct {
	name string // canonical with trailing dot; "." for the root
	port int
}

type corednsConverter struct {
	ctx         context.Context
	req         OfflineRequest
	p           *Plan
	groups      map[string]any
	conditional map[string]string
	rewrites    map[string]public.Rewrite
	served      map[string]bool
	cache       map[string]any
	control     map[string]any
	rootSeen    bool
	notes       map[string]bool
	// owner maps each zone and port of a plain DNS server block to the
	// block's index, and current is the index of the block being converted.
	// CoreDNS routes a query to the block with the longest matching zone on
	// the port the query arrived on. Blocks that are not converted take part
	// too: a classless reverse prefix routes the zones CoreDNS expands it to.
	owner   map[corednsZone]int
	ports   []int
	current int
	// curPorts are the ports of the block being converted.
	curPorts []int
	// opaque counts server blocks whose keys cannot be read (environment
	// placeholders, unsupported forms): CoreDNS may route any name to them.
	opaque int
	// blockPorts are the ports of each converted block and rewriteBlock the
	// block that defined each local answer, by name. portDrops counts the
	// answers omitted because a block on a lower port answers the same name,
	// by the omitted block's lowest port.
	blockPorts   map[int][]int
	rewriteBlock map[string]int
	portDrops    map[int]int
}

// route returns the zone of the plain DNS server block that CoreDNS sends a
// query for name on port to, and that block's index. The zone is "" and the
// index -1 when no block serves the name.
func (c *corednsConverter) route(name string, port int) (string, int) {
	for n := name; ; {
		if i, ok := c.owner[corednsZone{name: n, port: port}]; ok {
			return n, i
		}
		if n == "." {
			return "", -1
		}
		if dot := strings.IndexByte(n, '.'); dot >= 0 && dot+1 < len(n) {
			n = n[dot+1:]
		} else {
			n = "."
		}
	}
}

// unserved counts local answers a block defines for names CoreDNS never
// sends to that block: shadowed names fall inside a more specific block
// (named by zone), and other names reach no block or a different one.
// opaque counts names omitted because a block with unreadable keys may
// take them; ports counts imported names that another block answers on a
// different port (named in otherPorts).
type unserved struct {
	zones      map[string]bool
	n          int
	other      int
	opaque     int
	ports      int
	otherPorts map[int]bool
}

// mine reports whether CoreDNS sends a query for name to the block being
// converted on any of its ports.
func (c *corednsConverter) mine(name string) bool {
	for _, port := range c.curPorts {
		if _, i := c.route(name, port); i == c.current {
			return true
		}
	}
	return false
}

// serves reports whether CoreDNS sends queries for name to the block being
// converted, whose zones are blockZones, and counts the name in u if not.
// While a block with unreadable keys exists the routing is unknown, so no
// local answer counts as served.
func (c *corednsConverter) serves(name string, blockZones []string, u *unserved) bool {
	if c.opaque > 0 {
		u.opaque++
		return false
	}
	if c.mine(name) {
		// kudurru answers every port alike: name a port where another
		// block takes the query.
		for _, port := range c.ports {
			if _, i := c.route(name, port); i >= 0 && i != c.current && !containsInt(c.curPorts, port) {
				if u.otherPorts == nil {
					u.otherPorts = map[int]bool{}
				}
				u.otherPorts[port] = true
				u.ports++
				break
			}
		}
		return true
	}
	z := ""
	for _, port := range c.curPorts {
		if found, i := c.route(name, port); i >= 0 {
			z = found
			break
		}
	}
	if z != "" && within(z, blockZones) {
		if u.zones == nil {
			u.zones = map[string]bool{}
		}
		u.zones[z] = true
		u.n++
	} else {
		u.other++
	}
	return false
}

func containsInt(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

func (c *corednsConverter) reportUnserved(path, what string, u *unserved) {
	if u.n > 0 {
		zones := make([]string, 0, len(u.zones))
		for z := range u.zones {
			zones = append(zones, displayZone(z))
		}
		sort.Strings(zones)
		c.p.loss(fmt.Sprintf("%s: %d %s fall inside the more specific server block for %s, which CoreDNS sends those queries to; omitted (names redacted).", path, u.n, what, strings.Join(zones, ", ")))
	}
	if u.other > 0 {
		c.p.loss(fmt.Sprintf("%s: %d %s omitted; CoreDNS sends those queries to no server block or to another one.", path, u.other, what))
	}
	if u.opaque > 0 {
		c.p.loss(fmt.Sprintf("%s: %d %s omitted; a server block whose keys cannot be read may receive those queries in CoreDNS (names redacted).", path, u.opaque, what))
	}
	if u.ports > 0 {
		ports := make([]int, 0, len(u.otherPorts))
		for p := range u.otherPorts {
			ports = append(ports, p)
		}
		sort.Ints(ports)
		list := make([]string, len(ports))
		for i, p := range ports {
			list[i] = strconv.Itoa(p)
		}
		c.p.loss(fmt.Sprintf("%s: %d %s are imported, but on port %s CoreDNS sends those queries to another server block; kudurru gives them this block's answer on every listener (names redacted).", path, u.ports, what, strings.Join(list, ", ")))
	}
}

// keyZones splits a server block's keys into plain DNS zones and labels of
// encrypted or gRPC keys. A non-empty reason means the block is omitted;
// routes then holds the zones CoreDNS still routes to it (the expansion of
// classless reverse prefixes), and opaque is true when they are unknown.
func keyZones(blk *cfServerBlock) (zones []corednsZone, encrypted []string, reason string) {
	zones, encrypted, reason, _, _ = keyRoutes(blk)
	return zones, encrypted, reason
}

func keyRoutes(blk *cfServerBlock) (zones []corednsZone, encrypted []string, reason string, routes []corednsZone, opaque bool) {
	for _, key := range blk.keys {
		for _, part := range strings.Split(key.text, ",") {
			if part == "" {
				continue
			}
			if key.placeholder() {
				return nil, nil, "Server block: key with an environment placeholder; block omitted (value redacted).", nil, true
			}
			scheme, z, ok := corednsKey(part)
			if !ok {
				if scheme, classless, ok := classlessKey(part); ok {
					if scheme != "dns" {
						// An encrypted listener: omitted like any other
						// encrypted key, with the prefix redacted.
						encrypted = append(encrypted, scheme+"://(classless prefix):"+strconv.Itoa(classless[0].port))
						continue
					}
					routes = append(routes, classless...)
					reason = "Server block: classless reverse prefix key; block omitted, and local answers of other blocks for the reverse zones it covers are not imported (value redacted)."
					continue
				}
				return nil, nil, "Server block: unsupported key; block omitted (value redacted).", nil, true
			}
			if scheme != "dns" {
				encrypted = append(encrypted, scheme+"://"+displayZone(z.name)+":"+strconv.Itoa(z.port))
				continue
			}
			zones = append(zones, z)
		}
	}
	if reason != "" {
		return nil, nil, reason, append(routes, zones...), false
	}
	return zones, encrypted, "", nil, false
}

// classlessKey reads [scheme://]prefix[:port] for a reverse prefix that is
// not on an octet (IPv4) or nibble (IPv6) boundary and returns the zones
// CoreDNS expands it to: every reverse zone of the next boundary inside the
// prefix.
func classlessKey(s string) (string, []corednsZone, bool) {
	scheme := "dns"
	if i := strings.Index(s, "://"); i >= 0 {
		scheme, s = strings.ToLower(s[:i]), s[i+3:]
	}
	// Reuse the key reader for the scheme and port with a stand-in zone.
	host, rest := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 && strings.Count(s, ":") == 1 {
		host, rest = s[:i], s[i:]
	}
	prefix, err := netip.ParsePrefix(host)
	if err != nil {
		return "", nil, false
	}
	stand := "."
	if scheme != "dns" {
		stand = scheme + "://."
	}
	scheme, z, ok := corednsKey(stand + rest)
	if !ok {
		return "", nil, false
	}
	prefix = prefix.Masked()
	step := 8
	if prefix.Addr().Is6() {
		step = 4
	}
	bits := prefix.Bits()
	next := (bits + step - 1) / step * step
	if next-bits > 8 {
		return "", nil, false
	}
	var out []corednsZone
	for i := 0; i < 1<<(next-bits); i++ {
		sub, ok := subPrefix(prefix, next, i)
		if !ok {
			return "", nil, false
		}
		name, ok := reverseZone(sub)
		if !ok {
			return "", nil, false
		}
		out = append(out, corednsZone{name: name, port: z.port})
	}
	return scheme, out, true
}

// subPrefix returns the i-th prefix of length bits inside p.
func subPrefix(p netip.Prefix, bits, i int) (netip.Prefix, bool) {
	b := p.Addr().AsSlice()
	for j := 0; j < bits-p.Bits(); j++ {
		if i>>(bits-p.Bits()-1-j)&1 == 1 {
			pos := p.Bits() + j
			b[pos/8] |= 0x80 >> (pos % 8)
		}
	}
	a, ok := netip.AddrFromSlice(b)
	if !ok {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, bits), true
}

func (c *corednsConverter) note(message string) {
	if c.notes == nil {
		c.notes = map[string]bool{}
	}
	if !c.notes[message] {
		c.notes[message] = true
		c.p.loss(message)
	}
}

func (c *corednsConverter) convert(cf *corefile) error {
	if cf.topImports > 0 {
		c.p.loss(fmt.Sprintf("Corefile: %d top-level import lines omitted; imported files are not read.", cf.topImports))
	}
	if cf.unresolved > 0 {
		c.p.loss(fmt.Sprintf("Corefile: %d import lines omitted; only argument-free imports of snippets defined in this file are expanded, and files are not read.", cf.unresolved))
	}
	// Collect every plain DNS zone first: whether a block's local answer was
	// ever served depends on the blocks that follow it too. A zone listed by
	// two blocks is refused when the second block is converted.
	c.owner = map[corednsZone]int{}
	for i, blk := range cf.blocks {
		zones, _, _, routes, opaque := keyRoutes(blk)
		if opaque {
			c.opaque++
			continue
		}
		for _, z := range append(zones, routes...) {
			if _, dup := c.owner[z]; !dup {
				c.owner[z] = i
			}
			if !containsInt(c.ports, z.port) {
				c.ports = append(c.ports, z.port)
			}
		}
	}
	sort.Ints(c.ports)
	c.blockPorts, c.rewriteBlock, c.portDrops = map[int][]int{}, map[string]int{}, map[int]int{}
	for i, blk := range cf.blocks {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		c.current = i
		if err := c.block(blk); err != nil {
			return err
		}
	}
	if c.opaque > 0 && len(c.conditional) > 0 {
		c.p.loss(fmt.Sprintf("Corefile: %d conditional forwarding routes are imported, but a server block whose keys cannot be read may receive those queries in CoreDNS (names redacted).", len(c.conditional)))
	}
	if len(c.portDrops) > 0 {
		ports := make([]int, 0, len(c.portDrops))
		n := 0
		for port, count := range c.portDrops {
			ports = append(ports, port)
			n += count
		}
		sort.Ints(ports)
		list := make([]string, len(ports))
		for i, port := range ports {
			list[i] = strconv.Itoa(port)
		}
		c.p.loss(fmt.Sprintf("hosts and rewrite: %d names have another local answer in a server block on a different port; kudurru keeps the answer of the block on the lowest port and omits the answers on port %s (names redacted).", n, strings.Join(list, ", ")))
	}
	if !c.rootSeen {
		c.note("Corefile: no plain DNS server block serves the root zone; destination upstreams.groups.default, cache and access settings are kept.")
	}
	patch := map[string]any{}
	if len(c.groups) > 0 {
		patch["groups"] = c.groups
	}
	if len(c.conditional) > 0 {
		patch["conditional"] = c.conditional
	}
	if len(patch) > 0 {
		c.p.Settings["upstreams"] = patch
	}
	if len(c.cache) > 0 {
		c.p.Settings["cache"] = c.cache
	}
	if len(c.control) > 0 {
		c.p.Settings["control"] = c.control
	}
	if len(c.rewrites) > 128 {
		c.p.loss(fmt.Sprintf("hosts and rewrite: %d local answers exceed the 128 rewrites of one policy; all omitted. Serve these names from a zone instead (kudurru import bind).", len(c.rewrites)))
		return nil
	}
	if len(c.rewrites) > 0 {
		keys := make([]string, 0, len(c.rewrites))
		for k := range c.rewrites {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		spec := public.PolicySpec{ClientGroupRefs: []string{"default"}}
		for _, k := range keys {
			spec.Rewrites = append(spec.Rewrites, c.rewrites[k])
		}
		offlinePolicy(c.p, c.req.Namespace, "default", spec)
		c.p.loss("hosts and rewrite answers become rewrites of Policy/default, which replaces a destination Policy/default and its lists. Rewritten answers use the response.ttl setting.")
	}
	return nil
}

func (c *corednsConverter) block(blk *cfServerBlock) error {
	zones, encrypted, reason := keyZones(blk)
	if reason != "" {
		c.p.loss(reason)
		return nil
	}
	label := append([]string{}, encrypted...)
	for _, z := range zones {
		label = append(label, displayZone(z.name)+":"+strconv.Itoa(z.port))
	}
	path := "Server block " + strings.Join(label, " ")
	switch {
	case len(zones) == 0 && len(encrypted) == 0:
		return errors.New("corefile server block has no keys")
	case len(zones) == 0:
		c.p.loss(path + ": encrypted or gRPC listener block omitted with its certificates and directives; configure listeners.dot, listeners.doh or listeners.doq instead.")
		return nil
	case len(encrypted) > 0:
		c.p.loss(path + ": encrypted or gRPC keys " + strings.Join(encrypted, " ") + " are omitted with their certificates; the plain DNS keys are converted. Configure listeners.dot, listeners.doh or listeners.doq instead.")
	}
	names := []string{}
	c.curPorts = c.curPorts[:0]
	for _, z := range zones {
		if !containsInt(c.curPorts, z.port) {
			c.curPorts = append(c.curPorts, z.port)
		}
		if c.served[z.name] {
			return errors.New("corefile serves the same zone in more than one plain DNS server block; convert one listener at a time")
		}
		c.served[z.name] = true
		if z.port != 53 {
			c.note("Server block ports are not imported; kudurru answers on its listeners.dns addresses.")
		}
		names = append(names, z.name)
	}
	c.blockPorts[c.current] = append([]int{}, c.curPorts...)
	root := false
	for _, n := range names {
		root = root || n == "."
	}
	c.rootSeen = c.rootSeen || root
	unsupported := map[string]bool{}
	unknown := 0
	seen := map[string]int{}
	hasCache := false
	local := unserved{}
	for _, d := range blk.directives {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		name := d.name.text
		if d.name.quoted || !corednsPlugins[name] {
			unknown++
			continue
		}
		seen[name]++
		if placeholders(d) {
			c.p.loss(path + ": " + name + " uses an environment placeholder; directive omitted (value redacted).")
			continue
		}
		var err error
		switch name {
		case "forward":
			err = c.forward(path, names, root, d)
		case "hosts":
			if seen[name] > 1 {
				return errors.New("corefile server block uses hosts more than once")
			}
			err = c.hosts(path, names, d, &local)
		case "rewrite":
			err = c.rewrite(path, names, d, &local)
		case "cache":
			if seen[name] > 1 {
				return errors.New("corefile server block uses cache more than once")
			}
			hasCache = true
			c.cacheDirective(path, root, d)
		case "acl":
			c.acl(path, root, seen[name], d)
		case "file", "auto", "secondary":
			c.zoneSource(path, names, d)
		default:
			unsupported[name] = true
		}
		if err != nil {
			return err
		}
	}
	c.reportUnserved(path, "hosts or rewrite names", &local)
	if root && !hasCache {
		c.note(path + ": no cache plugin; destination cache settings are kept and kudurru caches answers by default.")
	}
	if len(unsupported) > 0 {
		list := make([]string, 0, len(unsupported))
		for k := range unsupported {
			list = append(list, k)
		}
		sort.Strings(list)
		c.p.loss(path + ": plugins not converted: " + strings.Join(list, ", ") + ".")
	}
	if unknown > 0 {
		c.p.loss(fmt.Sprintf("%s: %d unknown directives omitted (names redacted).", path, unknown))
	}
	return nil
}

func placeholders(d *cfDirective) bool {
	for _, a := range d.args {
		if a.placeholder() {
			return true
		}
	}
	for _, sub := range d.block {
		if sub.name.placeholder() || placeholders(sub) {
			return true
		}
	}
	return false
}

// corednsKey reads [scheme://]zone[:port]. A zone may be a domain, "." or an
// IPv4 prefix on an octet boundary or IPv6 prefix on a nibble boundary.
func corednsKey(s string) (string, corednsZone, bool) {
	scheme := "dns"
	if i := strings.Index(s, "://"); i >= 0 {
		scheme, s = strings.ToLower(s[:i]), s[i+3:]
		switch scheme {
		case "dns", "tls", "https", "https3", "grpc", "quic":
		default:
			return "", corednsZone{}, false
		}
	}
	port := 53
	switch scheme {
	case "tls":
		port = 853
	case "https", "https3":
		port = 443
	case "quic":
		port = 853
	case "grpc":
		port = 443
	}
	host := s
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", corednsZone{}, false
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", corednsZone{}, false
			}
			n, err := strconv.Atoi(rest[1:])
			if err != nil || n < 1 || n > 65535 {
				return "", corednsZone{}, false
			}
			port = n
		}
	} else if i := strings.LastIndex(s, ":"); i >= 0 && strings.Count(s, ":") == 1 {
		n, err := strconv.Atoi(s[i+1:])
		if err != nil || n < 1 || n > 65535 {
			return "", corednsZone{}, false
		}
		host, port = s[:i], n
	}
	zone, ok := corednsZoneName(host)
	return scheme, corednsZone{name: zone, port: port}, ok
}

// corednsZoneName canonicalizes "." , a domain, or a reverse prefix.
func corednsZoneName(s string) (string, bool) {
	if s == "." || s == "" {
		return ".", true
	}
	if prefix, err := netip.ParsePrefix(s); err == nil {
		return reverseZone(prefix)
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return "", false
	}
	return offlineDomain(s)
}

func reverseZone(prefix netip.Prefix) (string, bool) {
	prefix = prefix.Masked()
	a := prefix.Addr().Unmap()
	bits := prefix.Bits()
	labels := []string{}
	if a.Is4() {
		if bits%8 != 0 {
			return "", false
		}
		b := a.As4()
		for i := bits/8 - 1; i >= 0; i-- {
			labels = append(labels, strconv.Itoa(int(b[i])))
		}
		return strings.Join(append(labels, "in-addr", "arpa"), ".") + ".", true
	}
	if bits%4 != 0 {
		return "", false
	}
	b := a.As16()
	for i := bits/4 - 1; i >= 0; i-- {
		nibble := b[i/2] >> 4
		if i%2 == 1 {
			nibble = b[i/2] & 0x0f
		}
		labels = append(labels, strconv.FormatUint(uint64(nibble), 16))
	}
	return strings.Join(append(labels, "ip6", "arpa"), ".") + ".", true
}

func displayZone(z string) string {
	if z == "." {
		return "."
	}
	return strings.TrimSuffix(z, ".")
}

// within reports whether name equals or descends from one of zones.
func within(name string, zones []string) bool {
	for _, z := range zones {
		if z == "." || name == z || strings.HasSuffix(name, "."+z) {
			return true
		}
	}
	return false
}

// forward FROM TO... maps to an upstream group: the root becomes the
// default group and other suffixes become conditional routes.
func (c *corednsConverter) forward(path string, zones []string, root bool, d *cfDirective) error {
	if len(d.args) < 2 {
		c.p.loss(path + ": forward without FROM and TO omitted.")
		return nil
	}
	from, ok := corednsZoneName(d.args[0].text)
	if !ok {
		c.p.loss(path + ": forward with an unsupported FROM zone omitted (value redacted).")
		return nil
	}
	suffixes := []string{}
	if from == "." {
		suffixes = append(suffixes, zones...)
	} else if within(from, zones) {
		if !c.mine(from) {
			z := ""
			for _, port := range c.curPorts {
				if found, i := c.route(from, port); i >= 0 {
					z = found
					break
				}
			}
			c.p.loss(path + ": forward FROM inside the more specific server block for " + displayZone(z) + " never matches; omitted.")
			return nil
		}
		suffixes = append(suffixes, from)
	} else {
		c.p.loss(path + ": forward FROM outside the server block zones never matches; omitted.")
		return nil
	}
	strategy, forceTCP := "random", false
	tuning := false
	for _, opt := range d.block {
		switch opt.name.text {
		case "policy":
			switch firstText(opt.args) {
			case "random":
				strategy = "random"
			case "sequential":
				strategy = "strict"
			case "round_robin":
				strategy = "random"
				c.note("forward policy round_robin becomes random server selection.")
			default:
				c.p.loss(path + ": forward with an unsupported policy omitted as a unit.")
				return nil
			}
		case "force_tcp":
			forceTCP = true
		case "prefer_udp", "max_fails", "health_check", "expire", "max_concurrent", "max_idle_conns", "next", "failfast_all_unhealthy_upstreams", "failover":
			tuning = true
		case "tls_servername", "tls":
			c.p.loss(path + ": forward with a TLS server name override or client certificate omitted as a unit; use tls://NAME servers manually (values redacted).")
			return nil
		case "except":
			c.p.loss(path + ": forward with except omitted as a unit; excluded names would be forwarded by kudurru (values redacted).")
			return nil
		default:
			c.p.loss(path + ": forward with an unsupported option omitted as a unit (name redacted).")
			return nil
		}
	}
	if tuning {
		c.note("forward health-check, failure, connection and next options are not converted; kudurru applies its own upstream health tracking.")
	}
	servers := []string{}
	for _, to := range d.args[1:] {
		s := to.text
		if strings.HasPrefix(s, "/") || !strings.Contains(s, ".") && !strings.Contains(s, ":") {
			c.p.loss(path + ": forward to a resolv.conf-style file omitted as a unit; the file is not read.")
			return nil
		}
		plain := true
		if strings.HasPrefix(strings.ToLower(s), "dns://") {
			s = s[len("dns://"):]
		} else if strings.Contains(s, "://") {
			plain = false
		}
		server, ok := offlineServer(s)
		if !ok {
			c.p.loss(path + ": forward with an unsupported or credential-bearing upstream omitted as a unit (values redacted).")
			return nil
		}
		if plain && forceTCP {
			server = "tcp://" + server
		}
		servers = append(servers, server)
	}
	name := offlineName("coredns", "forward", strings.Join(suffixes, ","))
	if root && from == "." {
		name = "default"
	}
	if _, dup := c.groups[name]; dup {
		return errors.New("corefile forwards the same zone more than once")
	}
	c.groups[name] = map[string]any{"mode": "forward", "servers": servers, "strategy": strategy, "timeout": "2s"}
	for _, s := range suffixes {
		if s == "." {
			continue
		}
		if _, dup := c.conditional[s]; dup {
			return errors.New("corefile forwards the same zone more than once")
		}
		c.conditional[s] = name
	}
	return nil
}

// hosts converts inline entries into exact-name policy rewrites, with PTR
// answers unless no_reverse is set. The hosts file itself is never read.
func (c *corednsConverter) hosts(path string, blockZones []string, d *cfDirective, u *unserved) error {
	zones := blockZones
	c.note("hosts: the hosts file (by default the system hosts file) is not read; only inline entries are converted.")
	if len(d.args) > 1 {
		zones = zones[:0:0]
		for _, a := range d.args[1:] {
			z, ok := corednsZoneName(a.text)
			if !ok {
				c.p.loss(path + ": hosts with an unsupported zone omitted as a unit (value redacted).")
				return nil
			}
			zones = append(zones, z)
		}
	}
	reverse, fall := true, false
	addresses := map[string][]string{}
	pointers := map[string][]string{}
	skipped := 0
	for _, line := range d.block {
		ip, err := netip.ParseAddr(line.name.text)
		if err == nil && !line.name.quoted && ip.Zone() == "" && !line.hasBlock {
			ip = ip.Unmap()
			for _, a := range line.args {
				name, ok := offlineDomain(a.text)
				if !ok || !within(name, zones) {
					skipped++
					continue
				}
				// Lists stop growing past the 16-target limit, which bounds
				// every duplicate check below; an over-limit name is omitted.
				if len(addresses[name]) <= 16 && !containsString(addresses[name], ip.String()) {
					addresses[name] = append(addresses[name], ip.String())
				}
				if rev, err := dns.ReverseAddr(ip.String()); err == nil && len(pointers[rev]) <= 16 && !containsString(pointers[rev], name) {
					pointers[rev] = append(pointers[rev], name)
				}
			}
			continue
		}
		switch line.name.text {
		case "no_reverse":
			reverse = false
		case "fallthrough":
			fall = true
			if len(line.args) > 0 {
				c.note("hosts fallthrough limited to zones becomes forwarding for every name without a local answer.")
			}
		case "ttl":
			c.note("hosts ttl is not converted; rewritten answers use the response.ttl setting.")
		case "reload":
		default:
			skipped++
		}
	}
	if skipped > 0 {
		c.p.loss(fmt.Sprintf("%s: %d hosts entries or options omitted (values redacted).", path, skipped))
	}
	if !fall {
		c.note("hosts without fallthrough answered SERVFAIL for other names in its zones; kudurru forwards them.")
	}
	names := make([]string, 0, len(addresses))
	for name := range addresses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ips := addresses[name]
		if !c.serves(name, blockZones, u) {
			continue
		}
		if len(ips) > 16 {
			c.p.loss(path + ": hosts name with more than 16 addresses omitted (value redacted).")
			continue
		}
		if err := c.addRewrite(public.Rewrite{Name: strings.TrimSuffix(name, "."), Targets: ips}); err != nil {
			return err
		}
	}
	if reverse {
		// CoreDNS hosts answers PTR queries outside its zones too, but only
		// those that CoreDNS sends to this server block.
		revs := make([]string, 0, len(pointers))
		for rev := range pointers {
			revs = append(revs, rev)
		}
		sort.Strings(revs)
		ptr := unserved{}
		for _, rev := range revs {
			targets := pointers[rev]
			if !c.serves(rev, blockZones, &ptr) {
				continue
			}
			if len(targets) > 16 {
				c.p.loss(path + ": hosts address with more than 16 names has no PTR answer (value redacted).")
				continue
			}
			sort.Strings(targets)
			if err := c.addRewrite(public.Rewrite{Name: strings.TrimSuffix(rev, "."), Type: "PTR", Targets: targets}); err != nil {
				return err
			}
		}
		c.reportUnserved(path, "hosts PTR answers", &ptr)
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// addRewrite records a local answer. A second answer for the same name is
// refused, unless it comes from a block on other ports than the first one:
// kudurru answers every listener alike, so the block on the lowest port
// wins and the other answer is counted in portDrops.
func (c *corednsConverter) addRewrite(r public.Rewrite) error {
	for _, typ := range []string{"", "PTR", "CNAME"} {
		key := r.Name + "/" + typ
		if _, dup := c.rewrites[key]; !dup {
			continue
		}
		prev, ok := c.rewriteBlock[r.Name]
		if !ok || prev == c.current || sharesPort(c.blockPorts[prev], c.curPorts) {
			return errors.New("corefile gives the same name more than one local answer")
		}
		if minPort(c.blockPorts[prev]) <= minPort(c.curPorts) {
			c.portDrops[minPort(c.curPorts)]++
			return nil
		}
		c.portDrops[minPort(c.blockPorts[prev])]++
		delete(c.rewrites, key)
		break
	}
	sort.Strings(r.Targets)
	c.rewrites[r.Name+"/"+r.Type] = r
	c.rewriteBlock[r.Name] = c.current
	return nil
}

func sharesPort(a, b []int) bool {
	for _, p := range a {
		if containsInt(b, p) {
			return true
		}
	}
	return false
}

func minPort(ports []int) int {
	m := 0
	for i, p := range ports {
		if i == 0 || p < m {
			m = p
		}
	}
	return m
}

// rewrite maps only "rewrite [stop] name [exact] FROM TO [answer auto]" to a
// CNAME rewrite; every other form changes semantics kudurru cannot express.
func (c *corednsConverter) rewrite(path string, zones []string, d *cfDirective, u *unserved) error {
	args := append([]cfToken{}, d.args...)
	if len(args) > 0 && args[0].text == "stop" {
		args = args[1:]
	}
	kind := firstText(args)
	if kind != "name" || d.hasBlock {
		switch kind {
		case "continue", "type", "class", "edns0", "ttl", "cname", "rcode", "name":
			c.p.loss(path + ": rewrite " + kind + " is not converted (values redacted).")
		default:
			c.p.loss(path + ": rewrite of an unsupported form is not converted (values redacted).")
		}
		return nil
	}
	args = args[1:]
	if len(args) > 0 && args[0].text == "exact" {
		args = args[1:]
	}
	if len(args) == 4 && args[2].text == "answer" && args[3].text == "auto" {
		args = args[:2]
	}
	if len(args) != 2 {
		c.p.loss(path + ": rewrite name with prefix, suffix, substring, regex or answer rules is not converted (values redacted).")
		return nil
	}
	from, ok1 := offlineDomain(args[0].text)
	to, ok2 := offlineDomain(args[1].text)
	if !ok1 || !ok2 || from == to || !within(from, zones) {
		c.p.loss(path + ": rewrite name with an invalid or out-of-zone name is not converted (values redacted).")
		return nil
	}
	if !c.serves(from, zones, u) {
		return nil
	}
	c.note("rewrite name becomes a CNAME answer to the target name; CoreDNS answered under the original name.")
	return c.addRewrite(public.Rewrite{Name: strings.TrimSuffix(from, "."), CNAME: to})
}

// Cache bounds: TTLs are DNS TTLs (RFC 2181 limits them to 2^31-1 seconds)
// and each capacity stays far below an int overflow when summed.
const (
	maxCacheSeconds  = 1<<31 - 1
	maxCacheCapacity = 1 << 30
)

// cacheDirective maps the root block's cache plugin to the global cache. A
// value outside the bounds, a zero TTL (kudurru reads cache.maxTTL 0 as no
// limit) or a zero capacity (0 cache.maxEntries turns the cache off) omits
// the directive as a unit.
func (c *corednsConverter) cacheDirective(path string, root bool, d *cfDirective) {
	invalid := func() { c.p.loss(path + ": cache with an invalid or out-of-range value omitted as a unit.") }
	seconds := func(n, lowest int) (time.Duration, bool) {
		if n < lowest || n > maxCacheSeconds {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	args := d.args
	maxTTL, negTTL := time.Hour, 30*time.Minute
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0].text); err == nil || errors.Is(err, strconv.ErrRange) {
			ttl, ok := seconds(n, 1)
			if err != nil || !ok {
				invalid()
				return
			}
			maxTTL, negTTL = ttl, ttl
			args = args[1:]
		}
	}
	if !root || len(args) > 1 || len(args) == 1 && args[0].text != "." {
		c.p.loss(path + ": cache scoped to zones other than the root is not converted; the kudurru cache is global.")
		return
	}
	out := map[string]any{}
	// CoreDNS keeps a success answer at least MINTTL (5 s by default) but
	// never past TTL: TTL wins when MINTTL exceeds it. cache.minTTL is
	// always emitted, so a destination minTTL cannot exceed the imported
	// maxTTL and fail settings validation.
	minTTL := 5 * time.Second
	capacity, explicit := [2]int{9984, 9984}, false
	for _, opt := range d.block {
		switch opt.name.text {
		case "success", "denial":
			if len(opt.args) < 1 || len(opt.args) > 3 {
				invalid()
				return
			}
			nums := make([]int, 0, len(opt.args))
			for _, a := range opt.args {
				n, err := strconv.Atoi(a.text)
				if err != nil {
					invalid()
					return
				}
				nums = append(nums, n)
			}
			if nums[0] < 1 || nums[0] > maxCacheCapacity {
				invalid()
				return
			}
			i := 0
			if opt.name.text == "denial" {
				i = 1
			}
			capacity[i], explicit = nums[0], true
			if len(nums) > 1 {
				ttl, ok := seconds(nums[1], 1)
				if !ok {
					invalid()
					return
				}
				if opt.name.text == "success" {
					maxTTL = ttl
				} else {
					negTTL = ttl
				}
			}
			if len(nums) > 2 {
				least, ok := seconds(nums[2], 0)
				if !ok {
					invalid()
					return
				}
				if opt.name.text == "success" {
					minTTL = least
				} else {
					c.note("cache denial MINTTL is not converted; kudurru applies cache.minTTL to every answer.")
				}
			}
		case "prefetch":
			if len(opt.args) < 1 || len(opt.args) > 3 {
				c.p.loss(path + ": cache prefetch with invalid or out-of-range values omitted.")
				continue
			}
			n, err := strconv.Atoi(opt.args[0].text)
			if err != nil || n < 1 || n > maxCacheCapacity {
				c.p.loss(path + ": cache prefetch with invalid or out-of-range values omitted.")
				continue
			}
			window := time.Minute
			if len(opt.args) > 1 {
				w, err := time.ParseDuration(opt.args[1].text)
				if err != nil || w <= 0 || w > maxCacheSeconds*time.Second {
					c.p.loss(path + ": cache prefetch with invalid or out-of-range values omitted.")
					continue
				}
				window = w
			}
			if len(opt.args) > 2 {
				c.note("cache prefetch percentage is not converted; kudurru refreshes popular answers shortly before they expire.")
			}
			out["prefetch"] = map[string]any{"threshold": n, "window": window.String()}
		case "serve_stale":
			stale := time.Hour
			if len(opt.args) > 0 {
				s, err := time.ParseDuration(opt.args[0].text)
				if err != nil || s < 0 || s > maxCacheSeconds*time.Second {
					c.p.loss(path + ": cache serve_stale with invalid or out-of-range values omitted.")
					continue
				}
				stale = s
			}
			if stale == 0 {
				// CoreDNS serves no stale answer with a zero duration, and
				// kudurru reads cache.serveStale.ttl 0 the same way.
				c.note("cache serve_stale 0 becomes cache.serveStale.ttl 0s, which turns serving expired answers off.")
			}
			if len(opt.args) > 1 {
				c.note("cache serve_stale refresh mode is not converted; kudurru waits up to cache.serveStale.maxWait for a fresh answer.")
			}
			out["serveStale"] = map[string]any{"ttl": stale.String()}
		default:
			c.p.loss(path + ": cache option not converted (name redacted).")
		}
	}
	if explicit {
		out["maxEntries"] = capacity[0] + capacity[1]
	}
	if minTTL > maxTTL {
		c.note(path + ": cache MINTTL above the success TTL becomes cache.minTTL equal to cache.maxTTL; CoreDNS lets TTL win the same way.")
		minTTL = maxTTL
	}
	out["minTTL"], out["maxTTL"], out["negativeTTL"] = minTTL.String(), maxTTL.String(), negTTL.String()
	c.cache = out
}

type aclRule struct {
	action string
	types  []string
	nets   []string
}

// acl maps the root block's rules when they take one of these shapes, in
// order: block type T... (every network), then allow net N... (every type),
// then one final block for everything. Other orders are omitted as a unit.
func (c *corednsConverter) acl(path string, root bool, count int, d *cfDirective) {
	if !root || len(d.args) > 1 || len(d.args) == 1 && d.args[0].text != "." {
		c.p.loss(path + ": acl scoped to zones other than the root is not converted; kudurru access control is global.")
		return
	}
	if count > 1 {
		delete(c.control, "allowedNetworks")
		delete(c.control, "blockedQTypes")
		c.p.loss(path + ": more than one acl directive; all acl rules omitted.")
		return
	}
	var rules []aclRule
	for _, line := range d.block {
		r := aclRule{action: line.name.text}
		mode := ""
		for _, a := range line.args {
			switch a.text {
			case "type", "net":
				mode = a.text
			default:
				switch mode {
				case "type":
					r.types = append(r.types, a.text)
				case "net":
					r.nets = append(r.nets, a.text)
				default:
					c.p.loss(path + ": acl with an unsupported rule omitted as a unit (values redacted).")
					return
				}
			}
		}
		rules = append(rules, r)
	}
	all := func(v []string) bool { return len(v) == 0 || len(v) == 1 && v[0] == "*" }
	var blocked, allowed []string
	phase, closed := 0, false
	for _, r := range rules {
		switch {
		case phase == 0 && r.action == "block" && !all(r.types) && all(r.nets):
			for _, t := range r.types {
				code, ok := dns.StringToType[strings.ToUpper(t)]
				if !ok || code == 0 {
					c.p.loss(path + ": acl with an unknown query type omitted as a unit (value redacted).")
					return
				}
				name := dns.TypeToString[code]
				if len(blocked) >= 256 {
					c.p.loss(path + ": acl blocks more than 256 query types, the control.blockedQTypes limit; all acl rules omitted.")
					return
				}
				if !containsString(blocked, name) {
					blocked = append(blocked, name)
				}
			}
		case phase <= 1 && r.action == "allow" && all(r.types) && !all(r.nets):
			phase = 1
			for _, n := range r.nets {
				m, ok := offlineClient(n)
				if !ok {
					c.p.loss(path + ": acl with an unsupported network omitted as a unit (value redacted).")
					return
				}
				cidr := m.CIDR
				if cidr == "" {
					ip := netip.MustParseAddr(m.IP)
					cidr = netip.PrefixFrom(ip, ip.BitLen()).String()
				}
				if len(allowed) >= 1024 {
					c.p.loss(path + ": acl allows more than 1,024 networks, the control.allowedNetworks limit; all acl rules omitted.")
					return
				}
				if !containsString(allowed, cidr) {
					allowed = append(allowed, cidr)
				}
			}
		case phase == 1 && !closed && r.action == "block" && all(r.types) && all(r.nets):
			closed = true
			phase = 2
		default:
			c.p.loss(path + ": acl rule order, filter or drop actions, or per-type allow rules cannot be expressed; all acl rules omitted.")
			return
		}
	}
	if c.control == nil {
		c.control = map[string]any{}
	}
	if len(blocked) > 0 {
		c.control["blockedQTypes"] = blocked
	}
	if closed {
		sort.Strings(allowed)
		c.control["allowedNetworks"] = allowed
		c.note("acl allow networks become control.allowedNetworks; kudurru always answers loopback clients.")
	}
	if len(c.control) == 0 {
		c.control = nil
	}
}

// zoneSource names zones served from files or transfers without reading them.
func (c *corednsConverter) zoneSource(path string, zones []string, d *cfDirective) {
	name := d.name.text
	listed := zones
	start := 0
	if name == "file" {
		start = 1
	}
	if len(d.args) > start {
		listed = nil
		for _, a := range d.args[start:] {
			z, ok := corednsZoneName(a.text)
			if !ok {
				c.p.loss(path + ": " + name + " lists an unsupported zone (value redacted).")
				continue
			}
			listed = append(listed, z)
		}
	}
	display := make([]string, 0, len(listed))
	for _, z := range listed {
		display = append(display, displayZone(z))
	}
	switch name {
	case "secondary":
		c.p.loss(path + ": secondary zones " + strings.Join(display, ", ") + " are not converted; create DNSZone objects of type secondary.")
	default:
		c.p.loss(path + ": " + name + " zones " + strings.Join(display, ", ") + " are loaded from files that are not read; import each with kudurru import bind ZONE -f FILE.")
	}
}
