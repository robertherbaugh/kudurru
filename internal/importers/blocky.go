package importers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"go.yaml.in/yaml/v3"
)

// PreviewBlocky reads the documented groups-based configuration format. It
// never reads local list paths, resolves client hostnames, or fetches sources.
func PreviewBlocky(ctx context.Context, req OfflineRequest, r io.Reader) (*Plan, error) {
	return previewOffline(ctx, "blocky", req, r, blockyOffline)
}

type blockyLists struct {
	block, allow      []string
	hasDeny, hasAllow bool
	incomplete        bool
}

func blockyOffline(ctx context.Context, req OfflineRequest, root *offlineMap, p *Plan) error {
	if root.values["upstreams"] == nil && root.values["blocking"] == nil && root.values["customDNS"] == nil && root.values["conditional"] == nil {
		return errors.New("offline source does not contain recognized Blocky settings")
	}
	upstreams, err := root.mapping("upstreams")
	if err != nil {
		return err
	}
	groups, err := upstreams.mapping("groups")
	if err != nil {
		return err
	}
	strategy, err := offlineString(upstreams.take("strategy"))
	if err != nil {
		return err
	}
	if strategy == "" {
		strategy = "parallel_best"
	}
	switch strategy {
	case "parallel_best", "strict", "random":
	default:
		return errors.New("unsupported Blocky upstream strategy")
	}
	timeout, err := offlineDuration(upstreams.take("timeout"), 2*time.Second)
	if err != nil {
		return err
	}
	routing := map[string]string{}
	groupPatch := map[string]any{}
	for _, selector := range offlineKeys(groups) {
		values, err := offlineStrings(groups.take(selector))
		if err != nil {
			return err
		}
		name := offlineName("blocky", "upstream", selector)
		if selector == "default" {
			name = "default"
		}
		servers := []string{}
		valid := len(values) > 0
		for _, s := range values {
			server, ok := blockyServer(s)
			if !ok {
				valid = false
				break
			}
			servers = append(servers, server)
		}
		if !valid {
			p.loss("upstreams.groups: group with unsupported addresses, credentials, stamps or TLS name override omitted as a unit (values redacted).")
			continue
		}
		if selector != "default" {
			if _, ok := offlineClient(selector); !ok {
				p.loss("upstreams.groups: client hostname/glob routing omitted; no hostname resolution is performed.")
				continue
			}
		}
		routing[selector] = name
		groupPatch[name] = map[string]any{"mode": "forward", "servers": servers, "strategy": strategy, "timeout": timeout.String()}
	}
	upstreams.finish(p, "upstreams")
	patch := map[string]any{}
	if len(groupPatch) > 0 {
		patch["groups"] = groupPatch
	}
	if err := blockyConditional(root.take("conditional"), p, patch, strategy, timeout); err != nil {
		return err
	}
	if len(patch) > 0 {
		p.Settings["upstreams"] = patch
	}
	blocking, err := root.mapping("blocking")
	if err != nil {
		return err
	}
	mode, err := offlineString(blocking.take("blockType"))
	if err != nil {
		return err
	}
	if mode == "" {
		mode = "zeroIp"
	}
	blockMode := public.BlockResponse("")
	switch strings.ToLower(mode) {
	case "zeroip":
		blockMode = "zeroIP"
	case "nxdomain":
		blockMode = "nxdomain"
	case "refused":
		blockMode = "refused"
	default:
		p.loss("blocking.blockType: custom response addresses are omitted; destination block response is inherited.")
	}
	ttl, err := offlineDuration(blocking.take("blockTTL"), 6*time.Hour)
	if err != nil {
		return err
	}
	lists := map[string]*blockyLists{}
	for _, section := range []string{"denylists", "allowlists"} {
		defined, err := blocking.mapping(section)
		if err != nil {
			return err
		}
		for _, name := range offlineKeys(defined) {
			n := defined.take(name)
			sources, incomplete, err := blockySources(n, p, section)
			if err != nil {
				return err
			}
			l := lists[name]
			if l == nil {
				l = &blockyLists{}
				lists[name] = l
			}
			refs := p.lists(req.Namespace, offlineName("blocky", section, name), map[bool]string{true: "allow", false: "block"}[section == "allowlists"], sources)
			l.incomplete = l.incomplete || incomplete
			if section == "denylists" {
				l.block = refs
				l.hasDeny = n != nil && len(n.Content) > 0
			} else {
				l.allow = refs
				l.hasAllow = n != nil && len(n.Content) > 0
			}
		}
	}
	schedules, err := blocking.mapping("listSchedules")
	if err != nil {
		return err
	}
	for _, name := range offlineKeys(schedules) {
		if l := lists[name]; l != nil {
			l.incomplete = true
		}
	}
	if len(schedules.values) > 0 {
		p.loss("blocking.listSchedules: scheduled list groups are excluded from policies; source schedule combinations are not approximated.")
	}
	assignments, err := blocking.mapping("clientGroupsBlock")
	if err != nil {
		return err
	}
	allSelectors := map[string]bool{"default": true}
	for key := range assignments.values {
		allSelectors[key] = true
	}
	for key := range routing {
		allSelectors[key] = true
	}
	selectors := make([]string, 0, len(allSelectors))
	for key := range allSelectors {
		selectors = append(selectors, key)
	}
	sort.Strings(selectors)
	seen := map[string]bool{}
	for _, selector := range selectors {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := "default"
		var member public.ClientMatcher
		if selector != "default" {
			var ok bool
			member, ok = offlineClient(selector)
			if !ok {
				p.loss("blocking.clientGroupsBlock: client hostname, FQDN or glob policy omitted; reserve an IP/CIDR before migration.")
				continue
			}
			key := member.IP + member.CIDR
			if seen[key] {
				return errors.New("duplicate Blocky client IP/CIDR identity")
			}
			seen[key] = true
			name = offlineName("blocky", "client", key)
		}
		n := assignments.take(selector)
		if n == nil && selector != "default" {
			n = assignments.values["default"]
		}
		selected, err := offlineStrings(n)
		if err != nil {
			return err
		}
		pol := public.PolicySpec{ClientGroupRefs: []string{name}, BlockMode: blockMode, BlockTTL: offlineTTL(ttl), UpstreamGroup: routing[selector]}
		hasDeny, hasAllow, incomplete := false, false, false
		selectedSeen := map[string]bool{}
		for _, group := range selected {
			if selectedSeen[group] {
				return errors.New("duplicate Blocky list group assignment")
			}
			selectedSeen[group] = true
			l, ok := lists[group]
			if !ok {
				return errors.New("blocky client assignment references an undefined list group")
			}
			if l.incomplete {
				incomplete = true
				continue
			}
			pol.BlockListRefs = append(pol.BlockListRefs, l.block...)
			pol.AllowListRefs = append(pol.AllowListRefs, l.allow...)
			hasDeny = hasDeny || l.hasDeny
			hasAllow = hasAllow || l.hasAllow
		}
		if incomplete {
			p.loss("blocking.clientGroupsBlock: policy filtering omitted because a selected group has skipped sources or schedules; rebuild it before applying.")
			pol.BlockListRefs = nil
			pol.AllowListRefs = nil
		} else {
			pol.AllowOnly = hasAllow && !hasDeny
		}
		if selector != "default" {
			if err := offlineGroup(p, req.Namespace, name, []public.ClientMatcher{member}); err != nil {
				return err
			}
		}
		offlinePolicy(p, req.Namespace, name, pol)
	}
	blocking.finish(p, "blocking")
	if len(selectors) > 1 {
		p.loss("Client policies use kudurru's most-specific IP/CIDR tier and require the corresponding multi-policy license. Review overlapping source client assignments and routing before cutover.")
	}
	if len(lists) > 0 {
		p.loss("Blocky response-IP and CNAME-target list checks, loading/failure behavior and regex dialect are not guaranteed equivalent. URL bodies are not inspected offline; review destination fetch/unsupported/invalid status.")
	}
	return blockyCustomDNS(root.take("customDNS"), req, p)
}

func blockyServer(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// tcp+udp uses UDP with TCP retry on truncation on both sides.
	s = strings.TrimPrefix(s, "tcp+udp:")
	for _, pair := range [][2]string{{"tcp-tls:", "tls://"}, {"quic:", "quic://"}} {
		if strings.HasPrefix(s, pair[0]) && !strings.HasPrefix(s, pair[0]+"//") {
			s = pair[1] + strings.TrimPrefix(s, pair[0])
		}
	}
	return offlineServer(s)
}
func blockySources(n *yaml.Node, p *Plan, path string) ([]public.ListSource, bool, error) {
	if n == nil {
		return nil, false, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, false, errors.New("blocky list sources must be a sequence")
	}
	sources := []public.ListSource{}
	seen := map[string]bool{}
	incomplete := false
	for _, item := range n.Content {
		value, err := offlineString(item)
		if err != nil {
			return nil, false, err
		}
		if seen[value] {
			return nil, false, errors.New("duplicate Blocky list source identity")
		}
		seen[value] = true
		name := offlineName("blocky", "source", value)
		if u, ok := offlineURL(value); ok {
			sources = append(sources, public.ListSource{Name: name, URL: u, Format: "auto"})
			continue
		}
		if item.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			entries, complete := blockyInline(value)
			if !complete {
				incomplete = true
				p.loss("blocking." + path + ": unsupported inline rules omitted (values redacted).")
			}

			for _, source := range entrySources(entries) {
				source.Name = name + "-" + source.Name
				sources = append(sources, source)
			}
		} else {
			incomplete = true
			p.loss("blocking." + path + ": local file or credential-bearing/unsupported URL omitted (value redacted).")
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return sources, incomplete, nil
}

func blockyConditional(n *yaml.Node, p *Plan, patch map[string]any, strategy string, timeout time.Duration) error {
	m, err := offlineMapping(n)
	if err != nil {
		return err
	}
	mapping, err := m.mapping("mapping")
	if err != nil {
		return err
	}
	fallback, err := offlineBool(m.take("fallbackUpstream"), false)
	if err != nil {
		return err
	}
	rewrite := m.take("rewrite")
	if fallback || (rewrite != nil && len(rewrite.Content) > 0) {
		p.loss("conditional: rewrite or fallback semantics require manual conversion; all conditional routes omitted.")
		m.finish(p, "conditional")
		return nil
	}
	groups, ok := patch["groups"].(map[string]any)
	if !ok {
		groups = map[string]any{}
	}
	routes := map[string]string{}
	seen := map[string]bool{}
	for _, suffix := range offlineKeys(mapping) {
		domain, ok := offlineDomain(suffix)
		if !ok {
			return errors.New("blocky conditional suffix is invalid")
		}
		if seen[domain] {
			return errors.New("duplicate Blocky conditional suffix identity")
		}
		seen[domain] = true
		value, err := offlineString(mapping.take(suffix))
		if err != nil {
			return err
		}
		servers := []string{}
		valid := true
		for _, s := range strings.Split(value, ",") {
			server, ok := blockyServer(s)
			if !ok {
				valid = false
				break
			}
			servers = append(servers, server)
		}
		if !valid {
			p.loss("conditional.mapping: route with unsupported/credential-bearing resolver omitted (values redacted).")
			continue
		}
		name := offlineName("blocky", "route", domain)
		groups[name] = map[string]any{"mode": "forward", "servers": servers, "strategy": strategy, "timeout": timeout.String()}
		routes[domain] = name
	}
	if len(routes) > 0 {
		patch["groups"] = groups
		patch["conditional"] = routes
	}
	m.finish(p, "conditional")
	return nil
}

func blockyCustomDNS(n *yaml.Node, req OfflineRequest, p *Plan) error {
	m, err := offlineMapping(n)
	if err != nil {
		return err
	}
	mapping, err := m.mapping("mapping")
	if err != nil {
		return err
	}
	ttlNode := m.take("customTTL")
	ttl, err := blockyCustomTTL(ttlNode)
	if err != nil {
		return err
	}
	filter, err := offlineBool(m.take("filterUnmappedTypes"), true)
	if err != nil {
		return err
	}
	rewrite := m.take("rewrite")
	if !filter || (rewrite != nil && len(rewrite.Content) > 0) {
		p.loss("customDNS: unmapped-type forwarding or query rewriting requires manual conversion; all simple mappings omitted.")
		m.finish(p, "customDNS")
		return nil
	}
	seen := map[string]bool{}
	for _, name := range offlineKeys(mapping) {
		domain, ok := offlineDomain(name)
		if !ok {
			return errors.New("blocky customDNS owner is invalid")
		}
		if seen[domain] {
			return errors.New("duplicate Blocky customDNS suffix identity")
		}
		seen[domain] = true
		value, err := offlineString(mapping.take(name))
		if err != nil {
			return err
		}
		lines := []string{}
		valid := true
		ips := map[string]bool{}
		for _, s := range strings.Split(value, ",") {
			ip, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil || ip.Zone() != "" {
				valid = false
				break
			}
			if ips[ip.String()] {
				return errors.New("duplicate Blocky customDNS address")
			}
			ips[ip.String()] = true
			typ := "A"
			if ip.Is6() {
				typ = "AAAA"
			}
			for _, owner := range []string{domain, "*." + domain} {
				lines = append(lines, fmt.Sprintf("%s %d IN %s %s", owner, int64(ttl/time.Second), typ, ip.String()))
			}
		}
		if !valid {
			p.loss("customDNS.mapping: non-IP or malformed address mapping omitted as a unit (value redacted).")
			continue
		}
		if err := offlineZone(p, "blocky", req.Namespace, domain, lines); err != nil {
			return err
		}
	}
	if len(mapping.values) > 0 {
		p.loss("Blocky IP mappings become authoritative suffix zones with apex and wildcard records; generated SOA/NS are additional answers. Automatic reverse records are omitted and local answers bypass filtering. Review before cutover.")
	}
	m.finish(p, "customDNS")
	return nil
}

// Reuse kudurru's clean-room rule parser while admitting only Blocky's
// documented hosts/domain/wildcard/regex syntax, without adblock extensions.
func blockyInline(value string) ([]public.ListEntry, bool) {
	entries := []public.ListEntry{}
	complete := true
	seen := map[string]bool{}
	result, err := parser.Parse(strings.NewReader(value), parser.Options{Format: parser.FormatAdblock, KeepInvalid: -1, KeepUnsupported: -1}, func(rule parser.Rule) error {
		switch rule.Syntax {
		case parser.SyntaxDomain, parser.SyntaxHosts, parser.SyntaxWildcard, parser.SyntaxRegex:
		default:
			complete = false
			return nil
		}
		if rule.Exception || rule.Kind == parser.KindIP {
			complete = false
			return nil
		}
		typ := public.EntryExact
		v := strings.Clone(rule.Pattern)
		switch rule.Kind {
		case parser.KindSuffix:
			typ = public.EntryWildcard
			v = "*." + v
		case parser.KindRegex:
			typ = public.EntryRegex
		}
		// ListEntry.value is bounded more tightly than parser input. Match
		// the destination schema's character count, excluding regex slashes.
		if utf8.RuneCountInString(v) > 1024 {
			complete = false
			return nil
		}
		key := string(typ) + "/" + v
		if !seen[key] {
			entries = append(entries, public.ListEntry{Type: typ, Value: v, Origin: public.OriginMigrated})
			seen[key] = true
		}
		return nil
	})
	if err != nil || result == nil {
		return nil, false
	}
	return entries, complete && result.Unsupported == 0 && result.Invalid == 0
}
func blockyCustomTTL(n *yaml.Node) (time.Duration, error) {
	if n != nil && n.Tag == "!!int" {
		d, err := offlineSeconds(n, 0)
		if err != nil || d <= 0 || d > time.Duration(2147483647)*time.Second/60 {
			return 0, errors.New("blocky numeric customTTL must be positive minutes within the DNS TTL range")
		}
		return d * 60, nil
	}
	return offlineDuration(n, time.Hour)
}
