package cloudflare

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

type translation struct{ from, to netip.Prefix }

func translations(in map[string]string) ([]translation, error) {
	if len(in) > 64 {
		return nil, ErrInvalid
	}
	var out []translation
	for from, to := range in {
		a, err := netip.ParsePrefix(from)
		if err != nil || a != a.Masked() || a.Addr().Is4In6() {
			return nil, ErrInvalid
		}
		b, err := netip.ParsePrefix(to)
		if err != nil || b != b.Masked() || a.Bits() != b.Bits() || a.Addr().Is4() != b.Addr().Is4() || b.Addr().Is4In6() {
			return nil, ErrInvalid
		}
		for _, old := range out {
			if old.from.Overlaps(a) {
				return nil, ErrInvalid
			}
		}
		out = append(out, translation{a, b})
	}
	slices.SortFunc(out, func(a, b translation) int { return strings.Compare(a.from.String(), b.from.String()) })
	return out, nil
}
func translate(ip netip.Addr, rules []translation) netip.Addr {
	for _, rule := range rules {
		if !rule.from.Contains(ip) {
			continue
		}
		a := ip.As16()
		source := rule.from.Addr().As16()
		target := rule.to.Addr().As16()
		for i := range a {
			a[i] = target[i] | (a[i] ^ source[i])
		}
		translated := netip.AddrFrom16(a)
		if ip.Is4() {
			translated = translated.Unmap()
		}
		return translated
	}
	return ip
}

// Adjust copies selected original records and validates provider-only changes.
func (p *Provider) Adjust(ctx context.Context, pub host.Publication, endpoints []public.Endpoint) ([]public.Endpoint, error) {
	if err := p.available(ctx); err != nil {
		return nil, err
	}
	z, err := p.destination(pub.Destination)
	if err != nil {
		return nil, err
	}
	rules, err := translations(z.config.Translate)
	if err != nil {
		return nil, err
	}
	out := make([]public.Endpoint, 0, len(endpoints))
	for _, e := range endpoints {
		e = *e.DeepCopy()
		e.RecordType = strings.ToUpper(e.RecordType)
		if e.SetIdentifier != "" {
			return nil, ErrInvalid
		}
		options := map[string]string{}
		for _, v := range e.ProviderSpecific {
			if _, duplicate := options[v.Name]; duplicate {
				return nil, ErrInvalid
			}
			options[v.Name] = v.Value
		}
		if value, ok := options["cloudflare/targets"]; ok {
			if json.Unmarshal([]byte(value), &e.Targets) != nil || len(e.Targets) == 0 {
				return nil, ErrInvalid
			}
			delete(options, "cloudflare/targets")
			if len(e.Targets) == 1 && (e.RecordType == "A" || e.RecordType == "AAAA") {
				if _, err := netip.ParseAddr(e.Targets[0]); err != nil {
					e.RecordType = "CNAME"
				}
			}
		}
		for i, t := range e.Targets {
			if e.RecordType == "A" || e.RecordType == "AAAA" {
				ip, err := netip.ParseAddr(t)
				if err != nil {
					return nil, ErrInvalid
				}
				e.Targets[i] = translate(ip, rules).String()
			}
		}
		proxied := z.config.Defaults.Proxied
		if value, ok := options["cloudflare/proxied"]; ok {
			proxied, err = strconv.ParseBool(value)
			if err != nil {
				return nil, ErrInvalid
			}
		}
		if proxied && !proxyType(e.RecordType) {
			return nil, ErrInvalid
		}
		options["cloudflare/proxied"] = strconv.FormatBool(proxied)
		if value, ok := options["cloudflare/ttl"]; ok {
			e.RecordTTL, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, ErrInvalid
			}
			delete(options, "cloudflare/ttl")
		} else if e.RecordTTL == 0 {
			e.RecordTTL = z.config.Defaults.TTL
			if e.RecordTTL == 0 {
				e.RecordTTL = 1
			}
		}
		if proxied {
			e.RecordTTL = 1
		}
		if e.RecordTTL != 1 && (e.RecordTTL < z.minTTL || e.RecordTTL > 86400) {
			return nil, ErrInvalid
		}
		comment := z.config.Defaults.Comment
		if value, ok := options["cloudflare/comment"]; ok {
			comment = value
		}
		if strings.Contains(comment, markerPrefix) || len(stampComment(comment, p.owner)) > z.commentLimit || strings.ContainsFunc(comment, unicode.IsControl) {
			return nil, ErrInvalid
		}
		if comment != "" {
			options["cloudflare/comment"] = comment
		} else {
			delete(options, "cloudflare/comment")
		}
		e.ProviderSpecific = nil
		for name, value := range options {
			switch name {
			case "cloudflare/proxied", "cloudflare/comment":
			case "cloudflare/ipv4-only", "cloudflare/ipv6-only", "cloudflare/flatten-cname", "cloudflare/private-routing":
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return nil, ErrInvalid
				}
				if !enabled {
					continue
				}
				if (name == "cloudflare/ipv4-only" || name == "cloudflare/ipv6-only") && !proxied || name == "cloudflare/flatten-cname" && e.RecordType != "CNAME" || name == "cloudflare/private-routing" && !proxyType(e.RecordType) {
					return nil, ErrInvalid
				}
				value = "true"
			case "cloudflare/tags":
				var tags []string
				if json.Unmarshal([]byte(value), &tags) != nil || len(tags) > 20 || len(tags) > 0 && z.commentLimit == 100 {
					return nil, ErrInvalid
				}
				for _, tag := range tags {
					if !validTag(tag) {
						return nil, ErrInvalid
					}
				}
				slices.Sort(tags)
				tags = slices.Compact(tags)
				if len(tags) == 0 {
					continue
				}
				b, _ := json.Marshal(tags)
				value = string(b)
			default:
				return nil, ErrInvalid
			}
			e.ProviderSpecific = append(e.ProviderSpecific, public.ProviderSpecific{Name: name, Value: value})
		}
		if options["cloudflare/ipv4-only"] == "true" && options["cloudflare/ipv6-only"] == "true" {
			return nil, ErrInvalid
		}
		n, err := plan.Normalize(e)
		if err != nil {
			return nil, ErrInvalid
		}
		out = append(out, n)
	}
	return out, nil
}

// Cloudflare tag names are case-insensitive ASCII identifiers; values may be
// empty, but tags always include their name:value separator.
func validTag(tag string) bool {
	name, value, ok := strings.Cut(tag, ":")
	if !ok || name == "" || len(name) > 32 || len(value) > 100 || strings.ContainsFunc(value, unicode.IsControl) || strings.HasPrefix(strings.ToLower(name), "cf-") {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
