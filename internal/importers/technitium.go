package importers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func (c *sourceClient) tech(ctx context.Context, path string, q url.Values, out any) error {
	b, err := c.request(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	var env struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(b, &env) != nil || env.Status != "ok" {
		return fmt.Errorf("source Technitium %s: request failed (check the token and its permissions)", path)
	}
	if out != nil && (len(env.Response) == 0 || bytes.Equal(bytes.TrimSpace(env.Response), []byte("null")) || json.Unmarshal(env.Response, out) != nil) {
		return fmt.Errorf("source Technitium %s: invalid response", path)
	}
	return nil
}

// techText recognizes HTTP-200 API errors before treating an export as data.
func (c *sourceClient) techText(ctx context.Context, path string, q url.Values) ([]byte, error) {
	b, err := c.request(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		return nil, fmt.Errorf("source Technitium %s: export failed (check the token and its permissions)", path)
	}
	return b, nil
}

type techZone struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Internal     bool   `json:"internal"`
	Disabled     bool   `json:"disabled"`
	DNSSECStatus string `json:"dnssecStatus"`
}

type techZoneOptions struct {
	QueryAccess                string   `json:"queryAccess"`
	QueryACL                   []string `json:"queryAccessNetworkACL"`
	Catalog                    string   `json:"catalog"`
	OverrideCatalogQueryAccess *bool    `json:"overrideCatalogQueryAccess"`
}

func (c *sourceClient) technitium(ctx context.Context, req MigrationRequest, p *Plan) error {
	c.headers.Set("Authorization", "Bearer "+req.Credential)
	var zones struct {
		Zones []techZone `json:"zones"`
	}
	// Omitting pageNumber explicitly requests every zone in Technitium.
	if err := c.tech(ctx, "/zones/list", nil, &zones); err != nil {
		return err
	}
	var settings struct {
		BlockListURLs      []string        `json:"blockListUrls"`
		EnableBlocking     *bool           `json:"enableBlocking"`
		BlockingType       string          `json:"blockingType"`
		BlockingAnswerTTL  int             `json:"blockingAnswerTtl"`
		Forwarders         []string        `json:"forwarders"`
		ForwarderProtocol  string          `json:"forwarderProtocol"`
		BlockingBypassList []string        `json:"blockingBypassList"`
		Proxy              json.RawMessage `json:"proxy"`
	}
	if err := c.tech(ctx, "/settings/get", nil, &settings); err != nil {
		return err
	}
	if settings.EnableBlocking == nil || settings.BlockingType == "" {
		return fmt.Errorf("source Technitium settings response is missing blocking configuration")
	}
	mode := v1alpha1.BlockResponse("zeroIP")
	switch settings.BlockingType {
	case "NxDomain":
		mode = "nxdomain"
	case "AnyAddress":
	default:
		p.Warnings = append(p.Warnings, "Technitium's custom blocking response needs manual review; imported policies use zeroIP.")
	}
	pol := v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockMode: mode, BlockTTL: metav1.Duration{Duration: time.Duration(settings.BlockingAnswerTTL) * time.Second}}
	subscriptions := map[string][]v1alpha1.ListSource{}
	for i, u := range settings.BlockListURLs {
		u = strings.TrimSpace(u)
		listMode := "block"
		if strings.HasPrefix(u, "!") {
			listMode = "allow"
			u = strings.TrimSpace(strings.TrimPrefix(u, "!"))
		}
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		subscriptions[listMode] = append(subscriptions[listMode], v1alpha1.ListSource{Name: fmt.Sprintf("subscription-%d", i+1), URL: u, Format: "auto", Subdomains: true})
	}
	for _, listMode := range []string{"block", "allow"} {
		path := "/blocked/export"
		if listMode == "allow" {
			path = "/allowed/export"
		}
		b, err := c.techText(ctx, path, nil)
		if err != nil {
			return err
		}
		var entries []v1alpha1.ListEntry
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if e, ok := p.entry(line, v1alpha1.EntryWildcard, true, ""); ok {
				entries = append(entries, e)
			}
		}
		sources := append(entrySources(entries), subscriptions[listMode]...)
		refs := p.lists(req.Namespace, "technitium-"+listMode, listMode, sources)
		if *settings.EnableBlocking {
			if listMode == "block" {
				pol.BlockListRefs = refs
			} else {
				pol.AllowListRefs = refs
			}
		}
	}
	tm, om := meta("Policy", "default", req.Namespace)
	p.add(&v1alpha1.Policy{TypeMeta: tm, ObjectMeta: om, Spec: pol})
	if len(settings.BlockingBypassList) > 0 {
		p.Warnings = append(p.Warnings, "Technitium blocking bypass networks need an unfiltered client policy before cutover.")
	}
	if len(settings.Proxy) > 0 && string(settings.Proxy) != "null" {
		p.Warnings = append(p.Warnings, "Technitium uses an outbound proxy; its forwarders were not imported. Configure the destination's upstream connectivity before cutover.")
	} else if len(settings.Forwarders) > 0 {
		servers, ok := techServers(settings.Forwarders, settings.ForwarderProtocol)
		if ok {
			p.Settings["upstreams"] = map[string]any{"groups": map[string]any{"default": map[string]any{"servers": servers, "strategy": "parallel_best"}}}
		} else {
			p.Warnings = append(p.Warnings, "Technitium forwarder addresses or protocol need manual conversion.")
		}
	} else {
		p.Warnings = append(p.Warnings, "Technitium used full recursion; kudurru will use its configured upstream resolvers.")
	}
	// A nil entry caches a failed read, so one inaccessible catalog does not
	// cause another request for every member. Never fall back to local defaults.
	catalogs := map[string]*techZoneOptions{}
	for _, z := range zones.Zones {
		if z.Internal {
			continue
		}
		if z.Disabled {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped disabled zone %s.", z.Name))
			continue
		}
		if z.Type != "Primary" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped %s zone %s: only primary zones are imported automatically.", z.Type, z.Name))
			continue
		}
		if z.DNSSECStatus != "" && z.DNSSECStatus != "Unsigned" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped signed zone %s: prepare a DNSSEC transition before moving authority.", z.Name))
			continue
		}
		if err := c.techPrimary(ctx, req, p, z, catalogs); err != nil {
			return err
		}
	}
	p.Warnings = append(p.Warnings, "Technitium DNS Apps, Advanced Blocking profiles, conditional forwarders, zone transfers, NOTIFY and dynamic updates need separate review.")
	return nil
}

func techServers(in []string, protocol string) ([]string, bool) {
	var out []string
	for _, s := range in {
		// Pinned bootstrap addresses, proxy syntax and custom forwarder
		// directives have no equivalent in a simple upstream group.
		if strings.ContainsAny(s, "() \t\r\n") {
			return nil, false
		}
		switch protocol {
		case "Udp":
		case "Tcp":
			s = "tcp://" + s
		case "Tls":
			s = "tls://" + s
		case "Https":
			if !strings.HasPrefix(s, "https://") {
				return nil, false
			}
		case "Quic":
			s = "quic://" + s
		default:
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func (c *sourceClient) techPrimary(ctx context.Context, req MigrationRequest, p *Plan, z techZone, catalogs map[string]*techZoneOptions) error {
	var opts techZoneOptions
	if err := c.tech(ctx, "/zones/options/get", url.Values{"zone": {z.Name}}, &opts); err != nil {
		return err
	}
	query := &opts
	if opts.Catalog != "" {
		if opts.OverrideCatalogQueryAccess == nil {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped zone %s: catalog query-access inheritance is unknown.", z.Name))
			return nil
		}
		if !*opts.OverrideCatalogQueryAccess {
			catalog, seen := catalogs[opts.Catalog]
			if !seen {
				catalog = &techZoneOptions{}
				if err := c.tech(ctx, "/zones/options/get", url.Values{"zone": {opts.Catalog}}, catalog); err != nil {
					catalog = nil
				}
				catalogs[opts.Catalog] = catalog
			}
			if catalog == nil {
				p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped zone %s: could not read query access from catalog %s; the source token needs access to its zone options.", z.Name, opts.Catalog))
				return nil
			}
			// Member options report the local policy, even when the catalog's
			// policy is effective. Copy the inherited ACL in its original order.
			query = catalog
		}
	}
	access := map[string]v1alpha1.AccessMode{"Deny": "deny", "Allow": "allow", "AllowOnlyPrivateNetworks": "allowOnlyPrivate", "AllowOnlyForPrivateNetworks": "allowOnlyPrivate", "AllowOnlyPrivate": "allowOnlyPrivate", "AllowOnlyZoneNameServers": "allowOnlyZoneNameServers", "UseSpecifiedNetworkACL": "useNetworkACL", "AllowZoneNameServersAndUseSpecifiedNetworkACL": "zoneNameServersAndACL", "UseZoneNameServersAndSpecifiedNetworkACL": "zoneNameServersAndACL"}
	a, ok := access[query.QueryAccess]
	if !ok {
		p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped zone %s: its query access policy needs manual conversion.", z.Name))
		return nil
	}
	q := url.Values{"zone": {z.Name}, "domain": {z.Name}, "listZone": {"true"}}
	var records struct {
		Records []struct {
			Name      string `json:"name"`
			Type      string `json:"type"`
			Disabled  bool   `json:"disabled"`
			ExpiryTTL int    `json:"expiryTtl"`
		} `json:"records"`
	}
	if err := c.tech(ctx, "/zones/records/get", q, &records); err != nil {
		return err
	}
	for _, r := range records.Records {
		if r.Disabled || r.ExpiryTTL > 0 || r.Type == "APP" || r.Type == "ANAME" || r.Type == "FWD" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Skipped zone %s: record %s %s has disabled, expiring or app-specific behavior that a zone export cannot preserve.", z.Name, r.Name, r.Type))
			return nil
		}
	}
	b, err := c.techText(ctx, "/zones/export", url.Values{"zone": {z.Name}})
	if err != nil {
		return err
	}
	im, err := zone.ImportBIND(strings.NewReader(string(b)), z.Name)
	if err != nil {
		return fmt.Errorf("source Technitium zone %s: could not parse the standard zone export", z.Name)
	}
	if opts.Catalog != "" {
		const warning = "Catalog member zones are imported as independent primary zones with their effective query access rules. Catalog membership and replication are not migrated."
		if !slices.Contains(p.Warnings, warning) {
			p.Warnings = append(p.Warnings, warning)
		}
	}
	im.Spec.Policy = v1alpha1.ZonePolicy{QueryAccess: a, QueryACL: query.QueryACL, ZoneTransfer: "deny", Notify: "none"}
	p.zone("technitium", req.Namespace, z.Name, im)
	return nil
}
