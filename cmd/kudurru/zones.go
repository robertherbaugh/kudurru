package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// zoneSummary is one item of GET /zones (and the body of GET
// /zones/{zone}): "type, serial, recordCount, ready, managedBy". The
// object name is "name"; the zone apex is read from "zoneName" or "zone".
type zoneSummary struct {
	Name        string `json:"name"`
	ZoneName    string `json:"zoneName,omitempty"`
	Zone        string `json:"zone,omitempty"`
	Type        string `json:"type"`
	Serial      uint32 `json:"serial"`
	RecordCount int    `json:"recordCount"`
	Ready       any    `json:"ready"`
	ManagedBy   string `json:"managedBy,omitempty"`
}

func (z zoneSummary) apex() string { return firstNonEmpty(z.ZoneName, z.Zone) }

// readyString renders a ready field that may be a bool or a condition
// status string.
func readyString(v any) string {
	switch r := v.(type) {
	case bool:
		if r {
			return "True"
		}
		return "False"
	case string:
		return r
	case nil:
		return "Unknown"
	}
	return fmt.Sprint(v)
}

func zoneTable(zs []zoneSummary) *cli.Table {
	t := &cli.Table{Headers: []string{"NAME", "ZONE", "TYPE", "SERIAL", "RECORDS", "READY", "MANAGED-BY"}}
	for _, z := range zs {
		t.Add(z.Name, firstNonEmpty(z.apex(), "-"), z.Type, strconv.FormatUint(uint64(z.Serial), 10), strconv.Itoa(z.RecordCount), readyString(z.Ready), firstNonEmpty(z.ManagedBy, "-"))
	}
	return t
}

// newZonesCmd is `kudurru zones list|get|import|export|notify|delete`.
func newZonesCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "zones", Aliases: []string{"zone"}, Short: "List, import, export, notify and delete zones"}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List zones with serial, record count and readiness",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			zs, raws, err := listItems[zoneSummary](cmd.Context(), c, "/zones", nil)
			if err != nil {
				return err
			}
			return o.render(cmd, raws, func(bool) (*cli.Table, error) { return zoneTable(zs), nil })
		},
	}
	get := &cobra.Command{
		Use:               "get <zone>",
		Short:             "Show one zone",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.Get(cmd.Context(), "/zones/"+apiclient.Seg(args[0]), nil, &raw); err != nil {
				return err
			}
			var z zoneSummary
			_ = json.Unmarshal(raw, &z)
			return o.render(cmd, raw, func(bool) (*cli.Table, error) { return zoneTable([]zoneSummary{z}), nil })
		},
	}
	cmd.AddCommand(list, get, newZoneImportCmd(o, "import <zone> -f file.zone"), newZoneExportCmd(o), newZoneNotifyCmd(o), newZoneDeleteCmd(o))
	return cmd
}

// newZoneDeleteCmd is `kudurru zones delete <zone>`: DELETE /zones/{zone},
// the zone with every record set that references it (D148).
func newZoneDeleteCmd(o *cliOpts) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:               "delete <zone>",
		Aliases:           []string{"rm"},
		Short:             "Delete a zone and every record set that references it",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if dryRun {
				q.Set("dryRun", "true")
			}
			var raw json.RawMessage
			if err := c.Do(cmd.Context(), http.MethodDelete, "/zones/"+apiclient.Seg(args[0]), q, nil, &raw); err != nil {
				return err
			}
			f, _ := o.format()
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, raw)
			}
			var res struct {
				RecordSetCount int `json:"recordSetCount"`
				EndpointCount  int `json:"endpointCount"`
			}
			_ = json.Unmarshal(raw, &res)
			verb := "deleted"
			if dryRun {
				verb = "would be deleted (dry run)"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "zone %s %s with %d record sets (%d records)\n", args[0], verb, res.RecordSetCount, res.EndpointCount)
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only report what would be deleted")
	return cmd
}

// completeZones completes zone object names.
func (o *cliOpts) completeZones(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c, _, err := o.client()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	zs, _, err := listItems[zoneSummary](ctx, c, "/zones", nil)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(zs))
	for _, z := range zs {
		names = append(names, z.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// newZoneImportCmd posts BIND text to /zones/{zone}/import. It is shared
// by `zones import` and `import bind`.
func newZoneImportCmd(o *cliOpts, use string) *cobra.Command {
	var file, origin string
	cmd := &cobra.Command{
		Use:               use,
		Short:             "Import a BIND zone file into a zone's record sets",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readFileOrStdin(file, o.stdin)
			if err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var res json.RawMessage
			q := url.Values{}
			if origin != "" {
				q.Set("origin", origin)
			}
			if o.namespace != "" {
				q.Set("namespace", o.namespace)
			}
			req := apiclient.Request{Method: http.MethodPost, Path: "/zones/" + apiclient.Seg(args[0]) + "/import", Query: q, Body: bytes.NewReader(body), ContentType: "text/dns"}
			resp, err := c.Raw(cmd.Context(), req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if err := apiclient.CheckResponse(resp); err != nil {
				return err
			}
			_ = json.NewDecoder(resp.Body).Decode(&res)
			f, err := o.format()
			if err != nil {
				return err
			}
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, res)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "zone %s imported", args[0])
			if s := importSummary(res); s != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), ": %s", s)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout())
			var warn struct {
				Warnings []string `json:"warnings"`
			}
			_ = json.Unmarshal(res, &warn)
			for _, w := range warn.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "filename", "f", "", "zone file in BIND presentation format, or - for stdin")
	cmd.Flags().StringVar(&origin, "origin", "", "zone apex, to create the DNSZone when it does not exist yet")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// importSummary renders an import result: numbers as is, lists by their
// length, created when the zone was made ("created, endpoints 3,
// recordSets 1, serial 2026101101").
func importSummary(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var parts []string
	if m["created"] == true {
		parts = append(parts, "zone created")
	}
	for _, k := range sortedKeys(m) {
		switch v := m[k].(type) {
		case float64:
			if k != "took_ms" {
				parts = append(parts, fmt.Sprintf("%s %s", k, strconv.FormatFloat(v, 'f', -1, 64)))
			}
		case []any:
			if k != "warnings" {
				parts = append(parts, fmt.Sprintf("%s %d", k, len(v)))
			}
		case string:
			if k == "recordSet" || k == "result" {
				parts = append(parts, fmt.Sprintf("%s %s", k, v))
			}
		}
	}
	return strings.Join(parts, ", ")
}

func newZoneExportCmd(o *cliOpts) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:               "export <zone>",
		Short:             "Print a zone in BIND presentation format",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			resp, err := c.Raw(cmd.Context(), apiclient.Request{Path: "/zones/" + apiclient.Seg(args[0]) + "/export", Accept: "text/dns"})
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if err := apiclient.CheckResponse(resp); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if out != "" && out != "-" {
				fh, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec // zone data is public by nature
				if err != nil {
					return err
				}
				defer func() { _ = fh.Close() }()
				w = fh
			}
			_, err = io.Copy(w, resp.Body)
			return err
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	return cmd
}

func newZoneNotifyCmd(o *cliOpts) *cobra.Command {
	return &cobra.Command{
		Use:               "notify <zone>",
		Short:             "Send NOTIFY for a zone to its secondaries",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var res json.RawMessage
			if err := c.Post(cmd.Context(), "/zones/"+apiclient.Seg(args[0])+"/notify", nil, nil, &res); err != nil {
				return err
			}
			f, _ := o.format()
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, res)
			}
			var nr struct {
				Targets []string `json:"targets"`
			}
			_ = json.Unmarshal(res, &nr)
			msg := "NOTIFY queued for zone " + args[0]
			if len(nr.Targets) > 0 {
				msg += " to " + strings.Join(nr.Targets, ", ")
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), msg)
			return err
		},
	}
}

// recordItem is one item of GET /zones/{zone}/records.
type recordItem struct {
	RecordSet     string                `json:"recordSet"`
	Namespace     string                `json:"namespace,omitempty"`
	DNSName       string                `json:"dnsName"`
	RecordType    string                `json:"recordType"`
	Targets       []string              `json:"targets"`
	RecordTTL     int64                 `json:"recordTTL,omitempty"`
	SetIdentifier string                `json:"setIdentifier,omitempty"`
	Kudurru       *v1alpha1.EndpointExt `json:"kudurru,omitempty"`
}

// newRecordsCmd is `kudurru records list|add|rm`.
func newRecordsCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "records", Aliases: []string{"record", "rr"}, Short: "List, add and remove records in a zone"}
	list := &cobra.Command{
		Use:               "list <zone>",
		Aliases:           []string{"ls"},
		Short:             "List a zone's records across its record sets",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			items, raws, err := listItems[recordItem](cmd.Context(), c, "/zones/"+apiclient.Seg(args[0])+"/records", nil)
			if err != nil {
				return err
			}
			return o.render(cmd, raws, func(wide bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"NAME", "TYPE", "TTL", "TARGETS", "RECORDSET"}}
				if wide {
					t.Headers = append(t.Headers, "SET-ID", "DISABLED", "COMMENT")
				}
				for _, r := range items {
					ttl := "-"
					if r.RecordTTL > 0 {
						ttl = strconv.FormatInt(r.RecordTTL, 10)
					}
					row := []string{r.DNSName, r.RecordType, ttl, strings.Join(r.Targets, ","), firstNonEmpty(r.RecordSet, "-")}
					if wide {
						disabled, comment := "false", "-"
						if r.Kudurru != nil {
							disabled = strconv.FormatBool(r.Kudurru.Disabled)
							comment = firstNonEmpty(r.Kudurru.Comment, "-")
						}
						row = append(row, firstNonEmpty(r.SetIdentifier, "-"), disabled, comment)
					}
					t.Add(row...)
				}
				return t, nil
			})
		},
	}
	var (
		ttl       int64
		recordSet string
		comment   string
		setID     string
	)
	add := &cobra.Command{
		Use:   "add <zone> <name> <type> <target...>",
		Short: "Create or replace one name/type in a record set",
		Long: `Create or replace one endpoint (name, type and its targets) in a record set of
the zone. The record set defaults to "api-<zone>" and is created on demand.
Targets are in presentation format: an address for A/AAAA, a name for CNAME,
"10 mail.example.com." for MX, a quoted string for TXT.`,
		Example: `  kudurru records add home-example-com nas.home.example.com A 192.168.1.10 --ttl 300
  kudurru records add home-example-com home.example.com MX "10 mail.home.example.com."`,
		Args:              cobra.MinimumNArgs(4),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			ep := v1alpha1.Endpoint{DNSName: args[1], RecordType: strings.ToUpper(args[2]), Targets: args[3:], RecordTTL: ttl, SetIdentifier: setID}
			if comment != "" {
				ep.Kudurru = &v1alpha1.EndpointExt{Comment: comment}
			}
			q := url.Values{}
			if recordSet != "" {
				q.Set("recordSet", recordSet)
			}
			if o.namespace != "" {
				q.Set("namespace", o.namespace)
			}
			var res json.RawMessage
			if err := c.Post(cmd.Context(), "/zones/"+apiclient.Seg(args[0])+"/records", q, ep, &res); err != nil {
				return err
			}
			f, _ := o.format()
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, res)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s set in zone %s (record set %s)\n", ep.DNSName, ep.RecordType, args[0], firstNonEmpty(recordSet, "api-"+args[0]))
			return err
		},
	}
	add.Flags().Int64Var(&ttl, "ttl", 0, "record TTL in seconds (default: the zone's defaultTTL)")
	add.Flags().StringVar(&recordSet, "record-set", "", "record set to write (default api-<zone>)")
	add.Flags().StringVar(&comment, "comment", "", "comment stored with the record")
	add.Flags().StringVar(&setID, "set-identifier", "", "external-dns set identifier")
	var rmSet, rmSetID string
	rm := &cobra.Command{
		Use:               "rm <zone> <name> <type>",
		Aliases:           []string{"remove", "delete"},
		Short:             "Remove one name/type from a zone",
		Args:              cobra.ExactArgs(3),
		ValidArgsFunction: o.completeZones,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if rmSet != "" {
				q.Set("recordSet", rmSet)
			}
			if rmSetID != "" {
				q.Set("setIdentifier", rmSetID)
			}
			path := "/zones/" + apiclient.Seg(args[0]) + "/records/" + apiclient.Seg(args[1]) + "/" + apiclient.Seg(strings.ToUpper(args[2]))
			if err := c.Delete(cmd.Context(), path, q); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s removed from zone %s\n", args[1], strings.ToUpper(args[2]), args[0])
			return err
		},
	}
	rm.Flags().StringVar(&rmSet, "record-set", "", "only this record set (namespace/name or name)")
	rm.Flags().StringVar(&rmSetID, "set-identifier", "", "only the endpoint with this set identifier")
	cmd.AddCommand(list, add, rm)
	return cmd
}

// newImportCmd includes file-based BIND imports and live API migrations.
func newImportCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "import", Short: "Import data from other DNS servers"}
	cmd.AddCommand(newZoneImportCmd(o, "bind <zone> -f file.zone"))
	cmd.AddCommand(newMigrationCmd(o, "pihole"), newMigrationCmd(o, "technitium"))
	return cmd
}
