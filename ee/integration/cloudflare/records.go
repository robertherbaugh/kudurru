package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	cf "github.com/cloudflare/cloudflare-go/v7"
	sdk "github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

// Records returns two matching complete bounded observations, never a sample.
func (p *Provider) Records(ctx context.Context, d kinds.Destination) (host.RemoteSnapshot, error) {
	if err := p.available(ctx); err != nil {
		return host.RemoteSnapshot{}, err
	}
	z, err := p.destination(d)
	if err != nil {
		return host.RemoteSnapshot{}, err
	}
	first, err := p.read(ctx, z)
	if err != nil {
		return host.RemoteSnapshot{}, err
	}
	second, err := p.read(ctx, z)
	if err != nil {
		return host.RemoteSnapshot{}, err
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !slices.Equal(a, b) {
		return host.RemoteSnapshot{}, ErrDrift
	}
	if err := attachHeritage(second); err != nil {
		return host.RemoteSnapshot{}, err
	}
	return host.RemoteSnapshot{Records: second, Complete: true}, nil
}
func (p *Provider) read(ctx context.Context, z resolvedZone) ([]plan.RemoteRecord, error) {
	var out []plan.RemoteRecord
	seen := map[string]bool{}
	total := -1
	encoded := 0
	for page := 1; page <= maxPages; page++ {
		var env envelope[[]sdk.RecordResponse]
		_, err := p.records.List(ctx, sdk.RecordListParams{ZoneID: cf.F(z.destination.Zone), Page: cf.F(float64(page)), PerPage: cf.F(float64(pageSize)), Order: cf.F(sdk.RecordListParamsOrderName), Direction: cf.F(shared.SortDirectionAsc)}, option.WithResponseBodyInto(&env))
		if err != nil {
			return nil, safe(ctx, err)
		}
		if env.valid() != nil || env.ResultInfo.valid(page, pageSize, len(env.Result)) != nil {
			return nil, ErrInvalid
		}
		if total < 0 {
			total = *env.ResultInfo.TotalCount
		} else if total != *env.ResultInfo.TotalCount {
			return nil, ErrDrift
		}
		for _, record := range env.Result {
			encoded += len(record.JSON.RawJSON())
			if encoded > plan.MaxSnapshotBytes {
				return nil, ErrInvalid
			}
			row, err := p.convert(record, z)
			if err != nil {
				return nil, err
			}
			if seen[row.IDs[0]] {
				return nil, ErrInvalid
			}
			seen[row.IDs[0]] = true
			out = append(out, row)
		}
		if len(out) == total {
			slices.SortFunc(out, func(a, b plan.RemoteRecord) int { return strings.Compare(a.IDs[0], b.IDs[0]) })
			return out, nil
		}
	}
	return nil, ErrInvalid
}

type rawRecord struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Type           string                     `json:"type"`
	TTL            *float64                   `json:"ttl"`
	Content        *string                    `json:"content"`
	Priority       *uint16                    `json:"priority"`
	Comment        string                     `json:"comment"`
	Proxied        *bool                      `json:"proxied"`
	PrivateRouting bool                       `json:"private_routing"`
	Data           map[string]json.RawMessage `json:"data"`
	Settings       map[string]bool            `json:"settings"`
	Tags           []string                   `json:"tags"`
}

func (p *Provider) convert(record sdk.RecordResponse, z resolvedZone) (plan.RemoteRecord, error) {
	var r rawRecord
	if json.Unmarshal([]byte(record.JSON.RawJSON()), &r) != nil || !validID(r.ID) || r.Name == "" || r.Type == "" || r.TTL == nil || math.Trunc(*r.TTL) != *r.TTL || *r.TTL < 1 || *r.TTL > 86400 {
		return plan.RemoteRecord{}, ErrInvalid
	}
	name := dns.CanonicalName(r.Name)
	if !dns.IsSubDomain(z.apex, name) {
		return plan.RemoteRecord{}, ErrInvalid
	}
	target, err := r.rdata()
	if err != nil {
		return plan.RemoteRecord{}, err
	}
	ttl := int64(*r.TTL)
	proxied := false
	if r.Proxied != nil {
		proxied = *r.Proxied
	}
	if proxyType(r.Type) && r.Proxied == nil {
		return plan.RemoteRecord{}, ErrInvalid
	}
	if proxied {
		if !proxyType(r.Type) {
			return plan.RemoteRecord{}, ErrInvalid
		}
		ttl = 1
	}
	owner, comment := commentOwner(r.Comment, p.owner)
	props := []public.ProviderSpecific{{Name: "cloudflare/proxied", Value: strconv.FormatBool(proxied)}}
	if comment != "" {
		props = append(props, public.ProviderSpecific{Name: "cloudflare/comment", Value: comment})
	}
	for k, v := range r.Settings {
		if k != "ipv4_only" && k != "ipv6_only" && k != "flatten_cname" {
			return plan.RemoteRecord{}, ErrInvalid
		}
		if v {
			props = append(props, public.ProviderSpecific{Name: "cloudflare/" + strings.ReplaceAll(k, "_", "-"), Value: "true"})
		}
	}
	if r.PrivateRouting {
		props = append(props, public.ProviderSpecific{Name: "cloudflare/private-routing", Value: "true"})
	}
	if len(r.Tags) > 0 {
		if len(r.Tags) > 20 {
			return plan.RemoteRecord{}, ErrInvalid
		}
		slices.Sort(r.Tags)
		r.Tags = slices.Compact(r.Tags)
		b, _ := json.Marshal(r.Tags)
		props = append(props, public.ProviderSpecific{Name: "cloudflare/tags", Value: string(b)})
	}
	e, err := plan.Normalize(public.Endpoint{DNSName: name, RecordType: r.Type, RecordTTL: ttl, Targets: []string{target}, ProviderSpecific: props})
	if err != nil {
		return plan.RemoteRecord{}, ErrInvalid
	}
	// Canonical JSON avoids spurious version drift from object key ordering.
	var value any
	decoder := json.NewDecoder(strings.NewReader(record.JSON.RawJSON()))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return plan.RemoteRecord{}, ErrInvalid
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return plan.RemoteRecord{}, ErrInvalid
	}
	return plan.RemoteRecord{Endpoint: e, IDs: []string{r.ID}, Owner: owner, Version: kinds.Digest(canonical)}, nil
}
func (r rawRecord) rdata() (string, error) {
	content := ""
	if r.Content != nil {
		content = *r.Content
	}
	switch r.Type {
	case "TXT":
		if r.Content == nil {
			return "", ErrInvalid
		}
		if strings.HasPrefix(content, `"`) {
			return content, nil
		}
		return strconv.Quote(content), nil
	case "MX":
		if r.Priority == nil || content == "" {
			return "", ErrInvalid
		}
		return fmt.Sprintf("%d %s", *r.Priority, content), nil
	}
	if len(r.Data) == 0 {
		if content != "" {
			return content, nil
		}
		return "", ErrInvalid
	}
	valid := true
	get := func(name string) string {
		var s string
		if json.Unmarshal(r.Data[name], &s) != nil {
			valid = false
			return ""
		}
		return s
	}
	number := func(name string) string {
		var n uint32
		if json.Unmarshal(r.Data[name], &n) != nil {
			valid = false
			return ""
		}
		return strconv.FormatUint(uint64(n), 10)
	}
	var fields []string
	switch r.Type {
	case "LOC":
		decimal := func(name string) string {
			var n float64
			if json.Unmarshal(r.Data[name], &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				valid = false
				return ""
			}
			return strconv.FormatFloat(n, 'f', -1, 64)
		}
		fields = []string{decimal("lat_degrees"), decimal("lat_minutes"), decimal("lat_seconds"), get("lat_direction"), decimal("long_degrees"), decimal("long_minutes"), decimal("long_seconds"), get("long_direction"), decimal("altitude") + "m", decimal("size") + "m", decimal("precision_horz") + "m", decimal("precision_vert") + "m"}
	case "CAA":
		fields = []string{number("flags"), get("tag"), strconv.Quote(get("value"))}
	case "SRV":
		fields = []string{number("priority"), number("weight"), number("port"), get("target")}
	case "SVCB", "HTTPS":
		fields = []string{number("priority"), get("target"), get("value")}
		if !valid || fields[0] == "" || fields[1] == "" {
			return "", ErrInvalid
		}
		return strings.TrimSpace(strings.Join(fields, " ")), nil
	case "NAPTR":
		fields = []string{number("order"), number("preference"), strconv.Quote(get("flags")), strconv.Quote(get("service")), strconv.Quote(get("regex")), get("replacement")}
	case "DS":
		fields = []string{number("key_tag"), number("algorithm"), number("digest_type"), get("digest")}
	case "DNSKEY":
		fields = []string{number("flags"), number("protocol"), number("algorithm"), get("public_key")}
	case "CERT":
		fields = []string{number("type"), number("key_tag"), number("algorithm"), get("certificate")}
	case "TLSA", "SMIMEA":
		fields = []string{number("usage"), number("selector"), number("matching_type"), get("certificate")}
	case "SSHFP":
		fields = []string{number("algorithm"), number("type"), get("fingerprint")}
	case "URI":
		fields = []string{number("priority"), number("weight"), strconv.Quote(get("target"))}
	default:
		if content != "" {
			return content, nil
		}
		return "", ErrInvalid
	}
	if !valid {
		return "", ErrInvalid
	}
	for _, field := range fields {
		if field == "" {
			return "", ErrInvalid
		}
	}
	return strings.Join(fields, " "), nil
}
func proxyType(t string) bool { return t == "A" || t == "AAAA" || t == "CNAME" }

var _ host.Publisher = (*Provider)(nil)
