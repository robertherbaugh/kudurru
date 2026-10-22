package unifi

import (
	"context"
	"errors"
	"strings"

	uni "github.com/unpoller/unifi/v6"
)

type legacyRow struct {
	MAC            string `json:"mac"`
	IP             string `json:"ip"`
	Name           string `json:"name"`
	Hostname       string `json:"hostname"`
	LocalDNSRecord string `json:"local_dns_record"`
	NetworkID      string `json:"network_id"`
	Note           string `json:"note"`
	LastSeen       int64  `json:"last_seen"`
}

func (p *Provider) enrich(ctx context.Context, budget *pollBudget, site uni.IntegrationSite, clients []Device, networks []Network, diag *Diagnostics) error {
	byMAC := map[string]int{}
	for i, c := range clients {
		if c.MAC != "" {
			byMAC[c.MAC] = i
		}
	}
	paths := []struct{ name, path string }{
		{"users", "/api/s/" + site.InternalReference + "/rest/user"},
		{"stations", "/api/s/" + site.InternalReference + "/stat/sta"},
		{"leases", "/v2/api/site/" + site.InternalReference + "/active-leases"},
	}
	for _, endpoint := range paths {
		// Legacy uses the explicit same-origin /proxy/network paths, not v1.
		body, err := p.getPath(ctx, budget, "/proxy/network"+endpoint.path)
		var response *responseError
		if errors.As(err, &response) && response.code == 404 {
			diag.LegacyUnavailable = append(diag.LegacyUnavailable, endpoint.name)
			continue
		}
		if err != nil {
			return err
		}
		var envelope struct {
			Meta   struct{ RC string }
			Data   *[]legacyRow
			Leases *[]legacyRow `json:"dhcp_lease_info"`
		}
		if err := decode(body, &envelope, false); err != nil {
			return err
		}
		if endpoint.name == "leases" {
			envelope.Data = envelope.Leases
		} else if envelope.Meta.RC != "ok" {
			return ErrInvalid
		}
		if envelope.Data == nil {
			return ErrInvalid
		}
		if len(*envelope.Data) > maxClients {
			return ErrCapacity
		}
		seen := map[string]bool{}
		for _, row := range *envelope.Data {
			mac, err := normalizeMAC(row.MAC)
			if err != nil {
				return err
			}
			if seen[mac] {
				return ErrDrift
			}
			seen[mac] = true
			i, ok := byMAC[mac]
			if !ok {
				if row.LastSeen == 0 {
					diag.HistoricalWithoutTime++
				}
				continue
			}
			d := &clients[i]
			if row.IP != "" {
				matching := false
				for _, ip := range d.IPs {
					matching = matching || ip.String() == row.IP
				}
				if !matching {
					continue
				}
			}
			if len(row.Name) > 253 || len(row.Hostname) > 253 || len(row.LocalDNSRecord) > 253 || len(row.Note) > 4096 {
				return ErrInvalid
			}
			// Prefer explicit saved-user metadata over live station/lease
			// display fields, which may only fill missing enrichment.
			if row.Name != "" && d.Alias == "" {
				d.Alias = row.Name
			}
			if row.Hostname != "" && d.Hostname == "" {
				d.Hostname = row.Hostname
			}
			if row.LocalDNSRecord != "" && d.LocalDNSName == "" {
				d.LocalDNSName = row.LocalDNSRecord
			}
			if row.NetworkID != "" {
				if len(row.NetworkID) > 128 || strings.ContainsAny(row.NetworkID, "\x00\r\n\t ") {
					return ErrInvalid
				}
				// Legacy Mongo IDs are not official UUIDs. Only an exact official
				// ID authorizes an explicit join; otherwise infer from IP prefixes.
				for _, network := range networks {
					if network.ID == row.NetworkID {
						if d.NetworkID != "" && d.NetworkID != row.NetworkID {
							return ErrDrift
						}
						d.NetworkID = row.NetworkID
					}
				}
			}
			if row.Note != "" && d.Note == "" {
				d.Note = row.Note
			}
			// Never copy legacy LastSeen/fixed_ip into assignment evidence.
		}
	}
	return nil
}
