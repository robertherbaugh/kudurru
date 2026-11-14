package unifi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

// Action names (D401). Each is advertised only when its push field enables
// it, and each invocation is one reviewed, audited operator command.
const (
	ActionDHCPDNSServers = "dhcp-dns-servers"
	ActionBlockClient    = "block-client"
	ActionUnblockClient  = "unblock-client"
)

const (
	dhcpParams   = `{"type":"object","required":["mode"],"additionalProperties":false,"properties":{"mode":{"type":"string","enum":["preview","apply"]},"digest":{"type":"string","pattern":"^[a-f0-9]{64}$"}}}`
	clientParams = `{"type":"object","required":["mac"],"additionalProperties":false,"properties":{"mac":{"type":"string","maxLength":17,"pattern":"^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$"}}}`
)

// Actions advertises only the writes this instance's spec enables.
func (p *Provider) Actions() []integration.ActionSpec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []integration.ActionSpec{}
	if p.host == nil {
		return out
	}
	if p.config.Push.DHCPDNSServers {
		out = append(out, integration.ActionSpec{Name: ActionDHCPDNSServers, Params: json.RawMessage(dhcpParams), Verb: "integrations.act"})
	}
	if p.config.Push.ClientActions && p.config.Legacy {
		out = append(out,
			integration.ActionSpec{Name: ActionBlockClient, Params: json.RawMessage(clientParams), Verb: "integrations.act"},
			integration.ActionSpec{Name: ActionUnblockClient, Params: json.RawMessage(clientParams), Verb: "integrations.act"})
	}
	return out
}

// AuditSubject names the client a block-client or unblock-client action
// affects, in the canonical lowercase form the controller is sent, so that
// audit rows of one client match whatever letter case was asked (D384).
// The DHCP action is described by its digest alone.
func (p *Provider) AuditSubject(name string, params json.RawMessage) (string, bool) {
	if name != ActionBlockClient && name != ActionUnblockClient {
		return "", false
	}
	var in struct {
		MAC string `json:"mac"`
	}
	if decode(params, &in, true) != nil {
		return "", false
	}
	mac, err := normalizeMAC(in.MAC)
	if err != nil {
		return "", false
	}
	return "client " + mac, true
}

// Invoke runs one advertised action. Nothing here retries a write.
func (p *Provider) Invoke(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error) {
	if !slices.ContainsFunc(p.Actions(), func(a integration.ActionSpec) bool { return a.Name == name }) {
		return nil, host.ErrUnsupported
	}
	p.observeMu.Lock()
	defer p.observeMu.Unlock()
	switch name {
	case ActionDHCPDNSServers:
		var in struct {
			Mode   string `json:"mode"`
			Digest string `json:"digest"`
		}
		if decode(params, &in, true) != nil || in.Mode != "preview" && in.Mode != "apply" || in.Mode == "apply" && !kinds.ValidDigest(in.Digest) || in.Mode == "preview" && in.Digest != "" {
			return nil, host.ErrInvalid
		}
		out, err := p.dhcp(ctx, in.Mode == "apply", in.Digest)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	default:
		var in struct {
			MAC string `json:"mac"`
		}
		if decode(params, &in, true) != nil {
			return nil, host.ErrInvalid
		}
		mac, err := normalizeMAC(in.MAC)
		if err != nil {
			return nil, host.ErrInvalid
		}
		return p.station(ctx, name, mac)
	}
}

// station sends one legacy station command for a client MAC. The controller's
// answer is the only acknowledgment; a lost answer is uncertain.
func (p *Provider) station(ctx context.Context, name, mac string) (json.RawMessage, error) {
	budget := &pollBudget{}
	site, _, err := p.identity(ctx, budget)
	if err != nil {
		return nil, err
	}
	command := "block-sta"
	if name == ActionUnblockClient {
		command = "unblock-sta"
	}
	payload, _ := json.Marshal(map[string]string{"cmd": command, "mac": mac})
	body, err := p.write(ctx, budget, http.MethodPost, "/proxy/network/api/s/"+site.InternalReference+"/cmd/stamgr", payload)
	if err != nil {
		return nil, err
	}
	var envelope struct{ Meta struct{ RC string } }
	if decode(body, &envelope, false) != nil || envelope.Meta.RC != "ok" {
		return nil, ErrInvalid
	}
	return json.Marshal(struct {
		Action    string `json:"action"`
		MAC       string `json:"mac"`
		Completed bool   `json:"completed"`
	}{name, mac, true})
}

// DHCPChange is one configured network's reviewed DHCP DNS server change.
// Action is update, unchanged, missing (no network has the name) or
// unsupported (the network has no gateway DHCP server).
type DHCPChange struct {
	Network string   `json:"network"`
	Action  string   `json:"action"`
	Current []string `json:"current"`
	Desired []string `json:"desired"`
}

// DHCPPlan is the answer of dhcp-dns-servers. Digest binds the complete
// network bodies read; apply runs only with the digest of an unchanged plan.
type DHCPPlan struct {
	Mode      string       `json:"mode"`
	Digest    string       `json:"digest"`
	Changes   []DHCPChange `json:"changes"`
	Applied   int          `json:"applied"`
	Unchanged int          `json:"unchanged"`
}

type dhcpTarget struct {
	id     string
	body   map[string]json.RawMessage
	change DHCPChange
}

func (p *Provider) dhcpPlan(ctx context.Context, budget *pollBudget) (siteRow, []dhcpTarget, string, error) {
	p.mu.RLock()
	names, desired := slices.Clone(p.config.Push.Networks), slices.Clone(p.config.Push.DNSServers)
	p.mu.RUnlock()
	site, _, err := p.identity(ctx, budget)
	if err != nil {
		return siteRow{}, nil, "", err
	}
	path := "/sites/" + site.ID + "/networks"
	rows, err := list(ctx, p, budget, path, maxNetworks, func(n networkRow) string { return n.ID })
	if err != nil {
		return siteRow{}, nil, "", err
	}
	byName := map[string]string{}
	for _, row := range rows {
		if _, duplicate := byName[row.Name]; duplicate {
			byName[row.Name] = ""
			continue
		}
		byName[row.Name] = row.ID
	}
	type digestRow struct{ Network, ID, Body, Action string }
	var bound []digestRow
	var out []dhcpTarget
	for _, name := range names {
		t := dhcpTarget{change: DHCPChange{Network: name, Current: []string{}, Desired: slices.Clone(desired)}}
		id, ok := byName[name]
		if !ok || id == "" {
			// A duplicated name is as unusable as a missing one.
			t.change.Action = "missing"
			out = append(out, t)
			bound = append(bound, digestRow{Network: name, Action: t.change.Action})
			continue
		}
		raw, err := p.get(ctx, budget, path+"/"+id)
		if err != nil {
			return siteRow{}, nil, "", err
		}
		var detail map[string]json.RawMessage
		if decode(raw, &detail, false) != nil {
			return siteRow{}, nil, "", ErrInvalid
		}
		var identity struct{ ID, Name, Management string }
		if json.Unmarshal(raw, &identity) != nil || identity.ID != id || identity.Name != name {
			return siteRow{}, nil, "", ErrDrift
		}
		var v4 struct {
			DHCPConfiguration *struct {
				Mode                         string   `json:"mode"`
				DNSServerIPAddressesOverride []string `json:"dnsServerIpAddressesOverride"`
			} `json:"dhcpConfiguration"`
		}
		if c, ok := detail["ipv4Configuration"]; ok && json.Unmarshal(c, &v4) != nil {
			return siteRow{}, nil, "", ErrInvalid
		}
		t.id, t.body = id, detail
		switch {
		case identity.Management != "GATEWAY" || v4.DHCPConfiguration == nil || v4.DHCPConfiguration.Mode != "SERVER":
			t.change.Action = "unsupported"
		default:
			if v4.DHCPConfiguration.DNSServerIPAddressesOverride != nil {
				if len(v4.DHCPConfiguration.DNSServerIPAddressesOverride) > 16 {
					return siteRow{}, nil, "", ErrInvalid
				}
				t.change.Current = slices.Clone(v4.DHCPConfiguration.DNSServerIPAddressesOverride)
			}
			t.change.Action = "update"
			if slices.Equal(t.change.Current, desired) {
				t.change.Action = "unchanged"
			}
		}
		out = append(out, t)
		bound = append(bound, digestRow{Network: name, ID: id, Body: kinds.Digest(raw), Action: t.change.Action})
	}
	encoded, _ := json.Marshal(struct {
		Site     string
		Desired  []string
		Networks []digestRow
	}{site.ID, desired, bound})
	return site, out, kinds.Digest(encoded), nil
}

func (p *Provider) dhcp(ctx context.Context, apply bool, expected string) (*DHCPPlan, error) {
	budget := &pollBudget{}
	site, targets, digest, err := p.dhcpPlan(ctx, budget)
	if err != nil {
		return nil, err
	}
	out := &DHCPPlan{Mode: "preview", Digest: digest, Changes: []DHCPChange{}}
	for _, t := range targets {
		out.Changes = append(out.Changes, t.change)
		if t.change.Action == "unchanged" {
			out.Unchanged++
		}
	}
	if !apply {
		return out, nil
	}
	out.Mode = "apply"
	if expected != digest {
		return nil, host.ErrStalePreview
	}
	for _, t := range targets {
		if t.change.Action == "missing" || t.change.Action == "unsupported" {
			// Refused before any write; the preview names the network.
			return nil, host.ErrUnsupported
		}
	}
	path := p.prefix + "/v1/sites/" + site.ID + "/networks/"
	for _, t := range targets {
		if t.change.Action != "update" {
			continue
		}
		payload, err := dhcpBody(t.body, t.change.Desired)
		if err != nil {
			return nil, err
		}
		if _, err := p.write(ctx, budget, http.MethodPut, path+t.id, payload); err != nil {
			return nil, err
		}
		out.Applied++
	}
	// One reread confirms the written values; anything else is drift.
	_, after, _, err := p.dhcpPlan(ctx, &pollBudget{})
	if err != nil {
		return nil, err
	}
	for _, t := range after {
		if t.change.Action != "unchanged" {
			return nil, ErrDrift
		}
	}
	return out, nil
}

// dhcpBody is the network as read, without its identity and metadata, with
// only the IPv4 DHCP DNS server override replaced.
func dhcpBody(detail map[string]json.RawMessage, servers []string) ([]byte, error) {
	body := map[string]json.RawMessage{}
	for k, v := range detail {
		if k != "id" && k != "metadata" {
			body[k] = v
		}
	}
	var v4 map[string]json.RawMessage
	if json.Unmarshal(body["ipv4Configuration"], &v4) != nil || v4 == nil {
		return nil, ErrInvalid
	}
	var dhcp map[string]json.RawMessage
	if json.Unmarshal(v4["dhcpConfiguration"], &dhcp) != nil || dhcp == nil {
		return nil, ErrInvalid
	}
	encoded, _ := json.Marshal(servers)
	dhcp["dnsServerIpAddressesOverride"] = encoded
	v4["dhcpConfiguration"], _ = json.Marshal(dhcp)
	body["ipv4Configuration"], _ = json.Marshal(v4)
	return json.Marshal(body)
}
