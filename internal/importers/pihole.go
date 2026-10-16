package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type piGroup struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}
type piRule struct {
	ID      int    `json:"id"`
	Address string `json:"address"`
	Domain  string `json:"domain"`
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
	Groups  []int  `json:"groups"`
}
type piClient struct {
	Client  string `json:"client"`
	Comment string `json:"comment"`
	Groups  []int  `json:"groups"`
}
type piDNS struct {
	Upstreams    []string `json:"upstreams"`
	Hosts        []string `json:"hosts"`
	CNAMERecords []string `json:"cnameRecords"`
	RevServers   []string `json:"revServers"`
	HostRecord   string   `json:"hostRecord"`
	BlockTTL     int      `json:"blockTTL"`
	Blocking     struct {
		Active bool   `json:"active"`
		Mode   string `json:"mode"`
	} `json:"blocking"`
	Domain struct {
		Name string `json:"name"`
	} `json:"domain"`
}

func (c *sourceClient) pihole(ctx context.Context, req MigrationRequest, p *Plan) error {
	b, err := c.request(ctx, http.MethodPost, "/auth", nil, map[string]string{"password": req.Credential})
	if err != nil {
		return err
	}
	var auth struct {
		Session struct {
			Valid bool   `json:"valid"`
			SID   string `json:"sid"`
		} `json:"session"`
	}
	if json.Unmarshal(b, &auth) != nil || !auth.Session.Valid || auth.Session.SID == "" {
		return fmt.Errorf("source Pi-hole authentication failed; use a v6 application password")
	}
	c.headers.Set("X-FTL-SID", auth.Session.SID)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := c.request(cleanup, http.MethodDelete, "/auth", nil, nil); err != nil {
			p.Warnings = append(p.Warnings, "Could not close the temporary Pi-hole API session; it will expire on the source server.")
		}
	}()
	var groups struct {
		Groups []piGroup `json:"groups"`
	}
	var lists struct {
		Lists []piRule `json:"lists"`
	}
	var domains struct {
		Domains []piRule `json:"domains"`
	}
	var clients struct {
		Clients []piClient `json:"clients"`
	}
	var cfg struct {
		Config struct {
			DNS *piDNS `json:"dns"`
		} `json:"config"`
	}
	for _, get := range []struct {
		path string
		out  any
	}{{"/groups", &groups}, {"/lists", &lists}, {"/domains", &domains}, {"/clients", &clients}, {"/config/dns", &cfg}} {
		if err := c.get(ctx, get.path, get.out); err != nil {
			return err
		}
	}
	if cfg.Config.DNS == nil || cfg.Config.DNS.Blocking.Mode == "" {
		return fmt.Errorf("source Pi-hole settings response is missing DNS blocking configuration")
	}
	sort.Slice(groups.Groups, func(i, j int) bool { return groups.Groups[i].ID < groups.Groups[j].ID })
	if !slices.ContainsFunc(groups.Groups, func(g piGroup) bool { return g.ID == 0 }) {
		return fmt.Errorf("source Pi-hole response is missing the default group")
	}
	mode := v1alpha1.BlockResponse("zeroIP")
	switch cfg.Config.DNS.Blocking.Mode {
	case "NX":
		mode = "nxdomain"
	case "NULL":
	default:
		p.Warnings = append(p.Warnings, "Pi-hole's blocking response needs manual review; imported policies use zeroIP.")
	}
	for _, g := range groups.Groups {
		name := fmt.Sprintf("pihole-group-%d", g.ID)
		if g.ID == 0 {
			name = "default"
		}
		var members []v1alpha1.ClientMatcher
		if g.ID == 0 {
			members = []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}, {CIDR: "::/0"}}
		}
		for _, cl := range clients.Clients {
			if !slices.Contains(cl.Groups, g.ID) {
				continue
			}
			m := v1alpha1.ClientMatcher{Comment: cl.Comment}
			if ip, err := netip.ParseAddr(cl.Client); err == nil {
				m.IP = ip.String()
			} else if cidr, err := netip.ParsePrefix(cl.Client); err == nil {
				m.CIDR = cidr.Masked().String()
			} else {
				p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped client %q in group %q: reserve an IP or CIDR for MAC, hostname or interface matching.", cl.Client, g.Name))
				continue
			}
			members = append(members, m)
		}
		if len(members) > 1000 {
			return fmt.Errorf("source Pi-hole group %q exceeds the 1,000-client limit", g.Name)
		}
		tm, om := meta("ClientGroup", name, req.Namespace)
		om.Annotations = map[string]string{"kudurru.io/comment": "Pi-hole group: " + g.Name}
		p.add(&v1alpha1.ClientGroup{TypeMeta: tm, ObjectMeta: om, Spec: v1alpha1.ClientGroupSpec{Clients: members}})
		pol := v1alpha1.PolicySpec{ClientGroupRefs: []string{name}, BlockMode: mode, BlockTTL: metav1.Duration{Duration: time.Duration(cfg.Config.DNS.BlockTTL) * time.Second}}
		for _, listMode := range []string{"block", "allow"} {
			var sources []v1alpha1.ListSource
			var entries []v1alpha1.ListEntry
			for _, l := range lists.Lists {
				if l.Type != listMode || !slices.Contains(l.Groups, g.ID) {
					continue
				}
				enabled := l.Enabled
				sources = append(sources, v1alpha1.ListSource{Name: fmt.Sprintf("adlist-%d", l.ID), URL: l.Address, Format: "auto", Enabled: &enabled})
			}
			for _, d := range domains.Domains {
				want := "deny"
				if listMode == "allow" {
					want = "allow"
				}
				if d.Type != want || !slices.Contains(d.Groups, g.ID) {
					continue
				}
				typ := v1alpha1.EntryExact
				if d.Kind == "regex" {
					typ = v1alpha1.EntryRegex
				} else if d.Kind != "exact" {
					p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped unknown Pi-hole domain kind %q.", d.Kind))
					continue
				}
				if e, ok := p.entry(d.Domain, typ, d.Enabled, d.Comment); ok {
					entries = append(entries, e)
				}
			}
			// A disabled group still matches its clients with an empty policy,
			// preventing them from falling back to the blocking default policy.
			sources = append(sources, entrySources(entries)...)
			refs := p.lists(req.Namespace, fmt.Sprintf("pihole-%d-%s", g.ID, listMode), listMode, sources)
			if !g.Enabled || !cfg.Config.DNS.Blocking.Active {
				continue
			}
			if listMode == "block" {
				pol.BlockListRefs = refs
			} else {
				pol.AllowListRefs = refs
			}
		}
		tm, om = meta("Policy", name, req.Namespace)
		p.add(&v1alpha1.Policy{TypeMeta: tm, ObjectMeta: om, Spec: pol})
	}
	if len(groups.Groups) > 1 {
		p.Warnings = append(p.Warnings, "Additional Pi-hole groups require a kudurru license that permits multiple policies; client matching uses kudurru's most-specific IP/CIDR tier.")
	}
	for _, cl := range clients.Clients {
		if len(cl.Groups) == 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Client %q has no Pi-hole groups; assign an unfiltered policy before cutover to prevent default-policy filtering.", cl.Client))
		}
	}
	if len(cfg.Config.DNS.RevServers) > 0 {
		p.Warnings = append(p.Warnings, "Pi-hole conditional reverse forwarding needs manual conversion to upstream suffix routes.")
	}
	if cfg.Config.DNS.HostRecord != "" {
		p.Warnings = append(p.Warnings, "Pi-hole's custom hostRecord setting needs manual conversion.")
	}
	if err := p.piUpstreams(cfg.Config.DNS.Upstreams); err != nil {
		return err
	}
	return p.piLocal(req, *cfg.Config.DNS)
}

func (p *Plan) piUpstreams(servers []string) error {
	var lines []string
	for _, s := range servers {
		lines = append(lines, "server="+s)
	}
	im, err := upstream.ImportDnsmasq(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		return fmt.Errorf("source Pi-hole upstreams could not be converted")
	}
	groups := map[string]any{}
	if len(im.Default) > 0 {
		groups["default"] = map[string]any{"servers": im.Default, "strategy": "strict"}
	}
	for n, g := range im.Groups {
		groups["pihole-"+n] = map[string]any{"servers": g.Servers, "strategy": "strict"}
	}
	conditional := map[string]string{}
	for suffix, n := range im.Conditional {
		if n != "default" {
			n = "pihole-" + n
		}
		conditional[suffix] = n
	}
	if len(groups) > 0 {
		p.Settings["upstreams"] = map[string]any{"groups": groups, "conditional": conditional}
	}
	return nil
}

func (p *Plan) piLocal(req MigrationRequest, cfg piDNS) error {
	suffixes := slices.Clone(req.LocalZones)
	if len(suffixes) == 0 && cfg.Domain.Name != "" {
		suffixes = append(suffixes, cfg.Domain.Name)
	}
	sort.Slice(suffixes, func(i, j int) bool { return len(suffixes[i]) > len(suffixes[j]) })
	zones := map[string][]dns.RR{}
	add := func(owner, typ, target string, ttl int) error {
		owner = dns.Fqdn(owner)
		var suffix string
		for _, s := range suffixes {
			s = dns.Fqdn(strings.ToLower(strings.TrimSpace(s)))
			if s == "." {
				return fmt.Errorf("the root cannot be used as a Pi-hole local zone")
			}
			if dns.IsSubDomain(s, strings.ToLower(owner)) {
				suffix = s
				break
			}
		}
		if suffix == "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Local record %q is outside the selected local zones; select its local suffix to import it.", owner))
			return nil
		}
		rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", owner, ttl, typ, target))
		if err != nil {
			return fmt.Errorf("invalid Pi-hole local %s record", typ)
		}
		zones[suffix] = append(zones[suffix], rr)
		return nil
	}
	for _, line := range cfg.Hosts {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return fmt.Errorf("invalid Pi-hole hosts entry")
		}
		ip, err := netip.ParseAddr(parts[0])
		if err != nil {
			return fmt.Errorf("invalid Pi-hole hosts address")
		}
		typ := "A"
		if ip.Is6() {
			typ = "AAAA"
		}
		for _, owner := range parts[1:] {
			if strings.HasPrefix(owner, "#") {
				break
			}
			if err := add(owner, typ, ip.String(), 300); err != nil {
				return err
			}
		}
	}
	for _, line := range cfg.CNAMERecords {
		parts := strings.Split(line, ",")
		if len(parts) < 2 || len(parts) > 3 {
			return fmt.Errorf("invalid Pi-hole CNAME entry")
		}
		ttl := 300
		if len(parts) == 3 {
			parsedTTL, err := strconv.Atoi(strings.TrimSpace(parts[2]))
			if err != nil || parsedTTL < 0 {
				return fmt.Errorf("invalid Pi-hole CNAME TTL")
			}
			ttl = parsedTTL
		}
		if err := add(strings.TrimSpace(parts[0]), "CNAME", dns.Fqdn(strings.TrimSpace(parts[1])), ttl); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(zones))
	for k := range zones {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, suffix := range keys {
		var text strings.Builder
		for _, rr := range zones[suffix] {
			text.WriteString(rr.String() + "\n")
		}
		im, err := zone.ImportBIND(strings.NewReader(text.String()), suffix)
		if err != nil {
			return fmt.Errorf("source Pi-hole local zone %s: %w", suffix, err)
		}
		p.zone("pihole", req.Namespace, suffix, im)
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s becomes authoritative: missing names under this suffix will no longer forward upstream.", suffix))
	}
	return nil
}
