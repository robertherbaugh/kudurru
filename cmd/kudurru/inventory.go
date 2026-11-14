package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// newInventoryCmd lists GET /inventory (D397): integration-source devices
// and this node's DHCP leases, including devices that never queried.
func newInventoryCmd(o *cliOpts) *cobra.Command {
	var source, kind, stale, query string
	var limit int
	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "List known client devices with MACs, names, staleness and address ownership",
		Long: `List the client inventory: devices reported by inventory sources (UniFi,
webhook integrations) and the bound DHCP leases of the answering node, whether
or not they have sent a DNS query. Each address shows its state: assigned
(identifies queries), expired, ambiguous (a colliding claim), superseded (a
newer claim, or an inventory MAC that wins over a lease) or withdrawn.
Requires inventory.read.`,
		Example: `  kudurru inventory
  kudurru inventory --kind dhcp --stale false
  kudurru inventory --search 02:00:5e -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch kind {
			case "", api.InventoryKindSource, api.InventoryKindDHCP:
			default:
				return errors.New("--kind must be inventory or dhcp")
			}
			switch stale {
			case "", "true", "false":
			default:
				return errors.New("--stale must be true or false")
			}
			if limit < 0 {
				return errors.New("--limit must not be negative")
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			for key, value := range map[string]string{"source": source, "kind": kind, "stale": stale, "q": query} {
				if value != "" {
					q.Set(key, value)
				}
			}
			var items []api.InventoryDevice
			err = c.Pages(cmd.Context(), "/inventory", q, 1000, func(page apiclient.ListPage) error {
				for _, raw := range page.Items {
					var d api.InventoryDevice
					if err := json.Unmarshal(raw, &d); err != nil {
						return err
					}
					items = append(items, d)
				}
				if limit > 0 && len(items) >= limit {
					items = items[:limit]
					return errStopPages
				}
				return nil
			})
			if err != nil && !errors.Is(err, errStopPages) {
				return err
			}
			return o.render(cmd, map[string]any{"items": items}, func(wide bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"SOURCE", "ID", "NAME", "MACS", "ADDRESSES", "STALE", "EXPIRES"}}
				if wide {
					t.Headers = append(t.Headers, "TAGS", "OBSERVED")
				}
				for _, d := range items {
					var addresses []string
					for _, a := range d.Addresses {
						entry := a.IP + "(" + a.State
						if a.OwnerSource != "" {
							entry += " by " + a.OwnerSource + "/" + a.OwnerID
						}
						addresses = append(addresses, entry+")")
					}
					row := []string{d.Source, d.ID, firstNonEmpty(d.Name, d.Hostname, "-"), firstNonEmpty(strings.Join(d.MACs, ","), "-"),
						firstNonEmpty(strings.Join(addresses, " "), "-"), strconv.FormatBool(d.Stale), d.ExpiresAt.UTC().Format(time.RFC3339)}
					if wide {
						observed := "-"
						if d.ObservedAt != nil {
							observed = d.ObservedAt.UTC().Format(time.RFC3339)
						}
						row = append(row, firstNonEmpty(strings.Join(d.Tags, ","), "-"), observed)
					}
					t.Add(row...)
				}
				return t, nil
			})
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "only devices of this exact source (integration-<digest> or dhcp/<scope>)")
	cmd.Flags().StringVar(&kind, "kind", "", "inventory or dhcp")
	cmd.Flags().StringVar(&stale, "stale", "", "true: only stale devices; false: only live ones")
	cmd.Flags().StringVar(&query, "search", "", "case-insensitive substring of the ID, name, hostname, a MAC or an address")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after this many devices (0: all)")
	return cmd
}

var errStopPages = errors.New("inventory: enough devices")
