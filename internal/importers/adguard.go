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

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"go.yaml.in/yaml/v3"
)

// PreviewAdGuard converts a documented subset of AdGuardHome.yaml. It never
// reads the working directory, filter bodies, authentication or TLS material.
func PreviewAdGuard(ctx context.Context, req OfflineRequest, r io.Reader) (*Plan, error) {
	return previewOffline(ctx, "adguard", req, r, adguardOffline)
}

func adguardOffline(ctx context.Context, req OfflineRequest, root *offlineMap, p *Plan) error {
	if root.values["dns"] == nil && root.values["filtering"] == nil && root.values["filters"] == nil && root.values["clients"] == nil {
		return errors.New("offline source does not contain recognized AdGuard settings")
	}
	dnsSettings, err := root.mapping("dns")
	if err != nil {
		return err
	}
	filterNode := root.take("filtering")
	filtering := dnsSettings
	if filterNode != nil {
		filtering, err = offlineMapping(filterNode)
		if err != nil {
			return err
		}
		for _, key := range []string{"protection_enabled", "filtering_enabled", "blocking_mode", "blocked_response_ttl", "rewrites"} {
			if dnsSettings.values[key] != nil {
				return errors.New("ambiguous AdGuard filtering settings in both dns and filtering")
			}
		}
	}
	protection, err := offlineBool(filtering.take("protection_enabled"), true)
	if err != nil {
		return err
	}
	enabled, err := offlineBool(filtering.take("filtering_enabled"), true)
	if err != nil {
		return err
	}
	mode, err := offlineString(filtering.take("blocking_mode"))
	if err != nil {
		return err
	}
	blockMode := public.BlockResponse("")
	switch mode {
	case "null_ip":
		blockMode = "zeroIP"
	case "nxdomain":
		blockMode = "nxdomain"
	case "refused":
		blockMode = "refused"
	default:
		p.loss("filtering.blocking_mode: source response semantics are not converted; policies inherit the destination block response. Review before applying.")
	}
	ttl, err := offlineSeconds(filtering.take("blocked_response_ttl"), 10*time.Second)
	if err != nil {
		return err
	}
	if ttl == 0 {
		p.loss("filtering.blocked_response_ttl: zero cannot be represented as a policy override; destination TTL is inherited.")
	}
	var blockSources, allowSources []public.ListSource
	for _, section := range []string{"filters", "whitelist_filters"} {
		sources, err := adguardFilters(root.take(section), p, section)
		if err != nil {
			return err
		}
		if section == "filters" {
			blockSources = sources
		} else {
			allowSources = sources
		}
	}
	rules, err := offlineStrings(root.take("user_rules"))
	if err != nil {
		return err
	}
	block, allow := adguardRules(p, rules)
	blockSources = append(blockSources, entrySources(block)...)
	allowSources = append(allowSources, entrySources(allow)...)
	blockRefs := p.lists(req.Namespace, "adguard-block", "block", blockSources)
	allowRefs := p.lists(req.Namespace, "adguard-allow", "allow", allowSources)
	if len(blockSources)+len(allowSources) > 0 {
		p.loss("Filter URL bodies are not inspected offline. AdGuard modifiers, hosts-file response addresses and cross-list exceptions may differ; inspect destination unsupported/invalid rule counts after fetching.")
	}
	if len(blockRefs) > 1 {
		p.loss("Block filters exceed one object: inline exceptions apply only within their own BlockList; review cross-object exception scope.")
	}
	pol := public.PolicySpec{ClientGroupRefs: []string{"default"}, BlockMode: blockMode, BlockTTL: offlineTTL(ttl)}
	if protection && enabled {
		pol.BlockListRefs = blockRefs
		pol.AllowListRefs = allowRefs
	}
	if filtering.values["protection_disabled_until"] != nil {
		p.loss("filtering.protection_disabled_until: temporary protection deadline is omitted; review the imported enabled state.")
	}
	offlinePolicy(p, req.Namespace, "default", pol)
	if err := adguardUpstreams(dnsSettings, p); err != nil {
		return err
	}
	if err := adguardClients(ctx, root.take("clients"), req, p, pol, blockRefs, allowRefs, protection); err != nil {
		return err
	}
	if err := adguardLocal(filtering.take("rewrites"), req, p); err != nil {
		return err
	}
	if filterNode != nil {
		filtering.finish(p, "filtering")
	}
	dnsSettings.finish(p, "dns")
	return nil
}

func adguardFilters(n *yaml.Node, p *Plan, path string) ([]public.ListSource, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New("AdGuard filters must be a sequence")
	}
	sources := []public.ListSource{}
	seen := map[string]bool{}
	for _, item := range n.Content {
		m, err := offlineMapping(item)
		if err != nil {
			return nil, err
		}
		id := ""
		if n := m.take("id"); n != nil {
			if n.Tag != "!!int" {
				return nil, errors.New("AdGuard filter id must be an integer")
			}
			i, err := strconv.ParseInt(n.Value, 10, 64)
			if err != nil || i < 0 {
				return nil, errors.New("invalid AdGuard filter id")
			}
			id = strconv.FormatInt(i, 10)
		}
		address, err := offlineString(m.take("url"))
		if err != nil {
			return nil, err
		}
		if id == "" {
			id = address
		}
		if seen[id] {
			return nil, errors.New("duplicate AdGuard filter identity")
		}
		seen[id] = true
		enabled, err := offlineBool(m.take("enabled"), true)
		if err != nil {
			return nil, err
		}
		m.take("name")
		m.take("last_updated")
		m.finish(p, path)
		u, ok := offlineURL(address)
		if !ok {
			p.loss(path + ": local path or credential-bearing/unsupported URL omitted (value redacted).")
			continue
		}
		sources = append(sources, public.ListSource{Name: offlineName("adguard", "filter", id), URL: u, Format: "auto", Enabled: &enabled})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return sources, nil
}

func adguardRules(p *Plan, rules []string) (block, allow []public.ListEntry) {
	seen := map[string]bool{}
	for _, rule := range rules {
		s := strings.TrimSpace(rule)
		if s == "" || strings.HasPrefix(s, "!") || strings.HasPrefix(s, "#") {
			continue
		}
		exception := strings.HasPrefix(s, "@@")
		s = strings.TrimPrefix(s, "@@")
		var typ public.EntryType
		if strings.HasPrefix(s, "||") && strings.HasSuffix(s, "^") && !strings.Contains(s, "$") {
			s = strings.TrimSuffix(strings.TrimPrefix(s, "||"), "^")
			typ = public.EntryWildcard
		} else if strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
			typ = public.EntryRegex
		} else {
			p.loss("user_rules: unsupported or ambiguous AdGuard rule omitted (value redacted).")
			continue
		}
		entries := offlineEntries(p, []string{map[bool]string{true: "*.", false: ""}[typ == public.EntryWildcard] + s}, "user_rules")
		if len(entries) != 1 {
			continue
		}
		entry := entries[0]
		id := fmt.Sprintf("%t/%s/%s", exception, typ, entry.Value)
		if seen[id] {
			continue
		}
		seen[id] = true
		if exception {
			allow = append(allow, entry)
		} else {
			block = append(block, entry)
		}
	}
	return block, allow
}

func adguardUpstreams(m *offlineMap, p *Plan) error {
	file, err := offlineString(m.take("upstream_dns_file"))
	if err != nil {
		return err
	}
	servers, err := offlineStrings(m.take("upstream_dns"))
	if err != nil {
		return err
	}
	mode, err := offlineString(m.take("upstream_mode"))
	if err != nil {
		return err
	}
	bootstrap, err := offlineStrings(m.take("bootstrap_dns"))
	if err != nil {
		return err
	}
	patch := map[string]any{}
	if file != "" {
		p.loss("dns.upstream_dns_file: referenced file is not read; inline upstreams are ignored as on the source.")
	} else if len(servers) > 0 {
		converted := []string{}
		valid := true
		for _, s := range servers {
			v, ok := offlineServer(s)
			if !ok {
				valid = false
				break
			}
			converted = append(converted, v)
		}
		strategy := "strict"
		if len(converted) > 1 {
			if mode == "parallel" && len(converted) == 2 {
				strategy = "parallel_best"
			} else {
				valid = false
			}
		}
		if valid {
			patch["groups"] = map[string]any{"default": map[string]any{"mode": "forward", "servers": converted, "strategy": strategy}}
		} else {
			p.loss("dns.upstream_dns: conditional syntax, credentials, resolver stamps or source selection strategy require manual conversion; the entire upstream group is omitted.")
		}
	}
	if len(bootstrap) > 0 {
		safe := []string{}
		valid := true
		for _, s := range bootstrap {
			v, ok := offlineServer(s)
			if !ok || strings.Contains(v, "://") {
				valid = false
				break
			}
			safe = append(safe, v)
		}
		if valid {
			patch["bootstrap"] = safe
		} else {
			p.loss("dns.bootstrap_dns: unsupported bootstrap addresses omitted as a unit (values redacted).")
		}
	}
	if len(patch) > 0 {
		p.Settings["upstreams"] = patch
	}
	return nil
}

func adguardClients(ctx context.Context, n *yaml.Node, req OfflineRequest, p *Plan, global public.PolicySpec, blocks, allows []string, protection bool) error {
	if n == nil {
		return nil
	}
	clients := n
	if n.Kind == yaml.MappingNode {
		m, err := offlineMapping(n)
		if err != nil {
			return err
		}
		clients = m.take("persistent")
		m.finish(p, "clients")
	}
	if clients == nil {
		return nil
	}
	if clients.Kind != yaml.SequenceNode {
		return errors.New("AdGuard persistent clients must be a sequence")
	}
	seenNames, seenMatchers := map[string]bool{}, map[string]bool{}
	for _, item := range clients.Content {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := offlineMapping(item)
		if err != nil {
			return err
		}
		name, err := offlineString(m.take("name"))
		if err != nil || name == "" {
			return errors.New("AdGuard client name is required")
		}
		if seenNames[name] {
			return errors.New("duplicate AdGuard client identity")
		}
		seenNames[name] = true
		ids, err := offlineStrings(m.take("ids"))
		if err != nil {
			return err
		}
		members := []public.ClientMatcher{}
		for _, id := range ids {
			matcher, ok := offlineClient(id)
			if !ok {
				p.loss("clients.persistent: non-IP/CIDR identifier omitted; reserve an address before migration (value redacted).")
				continue
			}
			key := matcher.IP + matcher.CIDR
			if seenMatchers[key] {
				return errors.New("duplicate AdGuard client IP/CIDR identity")
			}
			seenMatchers[key] = true
			members = append(members, matcher)
		}
		useGlobal, err := offlineBool(m.take("use_global_settings"), true)
		if err != nil {
			return err
		}
		filtering, err := offlineBool(m.take("filtering_enabled"), true)
		if err != nil {
			return err
		}
		ignoreLog, err := offlineBool(m.take("ignore_querylog"), false)
		if err != nil {
			return err
		}
		upstreams, err := offlineStrings(m.take("upstreams"))
		if err != nil {
			return err
		}
		m.take("uid")
		m.finish(p, "clients.persistent")
		if len(members) == 0 {
			p.loss("clients.persistent: client policy omitted because no supported identifiers remain.")
			continue
		}
		group := offlineName("adguard", "client", name)
		if err := offlineGroup(p, req.Namespace, group, members); err != nil {
			return err
		}
		pol := global
		pol.ClientGroupRefs = []string{group}
		queryLog := !ignoreLog
		pol.QueryLog = &queryLog
		if !useGlobal {
			pol.BlockListRefs = nil
			pol.AllowListRefs = nil
			if protection && filtering {
				pol.BlockListRefs = blocks
				pol.AllowListRefs = allows
			}
		}
		if len(upstreams) > 0 {
			server, ok := offlineServer(upstreams[0])
			if len(upstreams) == 1 && ok {
				patch, ok := p.Settings["upstreams"].(map[string]any)
				if !ok {
					patch = map[string]any{}
					p.Settings["upstreams"] = patch
				}
				groups, ok := patch["groups"].(map[string]any)
				if !ok {
					groups = map[string]any{}
					patch["groups"] = groups
				}
				groups[group] = map[string]any{"mode": "forward", "servers": []string{server}, "strategy": "strict"}
				pol.UpstreamGroup = group
			} else {
				p.loss("clients.persistent.upstreams: unsupported addresses or selection semantics omitted; destination resolver routing is inherited.")
			}
		}
		offlinePolicy(p, req.Namespace, group, pol)
	}
	if len(seenNames) > 0 {
		p.loss("Client policies use kudurru's most-specific IP/CIDR tier; overlapping source identifiers and additional-policy license limits require review.")
	}
	return nil
}

func adguardLocal(n *yaml.Node, req OfflineRequest, p *Plan) error {
	if n == nil {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return errors.New("AdGuard rewrites must be a sequence")
	}
	zones := map[string][]string{}
	seen := map[string]bool{}
	for _, item := range n.Content {
		m, err := offlineMapping(item)
		if err != nil {
			return err
		}
		domain, err := offlineString(m.take("domain"))
		if err != nil {
			return err
		}
		answer, err := offlineString(m.take("answer"))
		if err != nil {
			return err
		}
		enabled, err := offlineBool(m.take("enabled"), true)
		if err != nil {
			return err
		}
		m.finish(p, "filtering.rewrites")
		owner, ok := offlineDomain(domain)
		if !ok {
			p.loss("filtering.rewrites: wildcard or invalid owner omitted (value redacted).")
			continue
		}
		typ, target := "CNAME", ""
		if ip, err := netip.ParseAddr(answer); err == nil && ip.Zone() == "" {
			typ = "A"
			if ip.Is6() {
				typ = "AAAA"
			}
			target = ip.String()
		} else {
			target, ok = offlineDomain(answer)
			if !ok || answer == "A" || answer == "AAAA" {
				p.loss("filtering.rewrites: special passthrough or unsupported answer omitted (value redacted).")
				continue
			}
		}
		key := owner + "/" + typ
		if seen[key] {
			return errors.New("duplicate or ambiguous AdGuard local record identity")
		}
		seen[key] = true
		if !enabled {
			p.loss("filtering.rewrites: disabled rewrite omitted.")
			continue
		}
		suffix := ""
		for _, s := range req.LocalZones {
			if owner == s || strings.HasSuffix(owner, "."+s) {
				suffix = s
				break
			}
		}
		if suffix == "" {
			p.loss("filtering.rewrites: exact record omitted; explicitly select its authoritative suffix with --local-zone.")
			continue
		}
		zones[suffix] = append(zones[suffix], fmt.Sprintf("%s 300 IN %s %s", owner, typ, target))
	}
	keys := make([]string, 0, len(zones))
	for k := range zones {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		sort.Strings(zones[name])
		if err := offlineZone(p, "adguard", req.Namespace, name, zones[name]); err != nil {
			return err
		}
	}
	if len(zones) > 0 {
		p.loss("Selected AdGuard local suffixes become authoritative with a 300-second TTL and generated SOA/NS. Missing names no longer forward upstream; local answers bypass filtering. Validate this explicitly before cutover.")
	}
	return nil
}
