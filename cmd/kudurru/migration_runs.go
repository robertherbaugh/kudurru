package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

func migrationOutput(cmd *cobra.Command, o *cliOpts, value any) error {
	f, err := o.format()
	if err != nil {
		return err
	}
	if !f.Structured() {
		f = cli.FormatYAML
	}
	return cli.WriteStructured(cmd.OutOrStdout(), f, value)
}
func migrationResultOutput(cmd *cobra.Command, o *cliOpts, result api.MigrationResult) error {
	if err := migrationOutput(cmd, o, result); err != nil {
		return err
	}
	if result.SettingsError != "" || result.JournalError != "" {
		return fmt.Errorf("migration incomplete; inspect the errors above")
	}
	for _, item := range result.Items {
		if item.Result == api.ResultError {
			return fmt.Errorf("migration incomplete; inspect the errors above")
		}
	}
	return nil
}
func newMigrationRunsCmd(o *cliOpts) *cobra.Command {
	return &cobra.Command{Use: "runs [id]", Short: "List migration runs or inspect a run and its cutovers", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			var run api.MigrationRun
			if err := c.Get(cmd.Context(), "/imports/runs/"+url.PathEscape(args[0]), nil, &run); err != nil {
				return err
			}
			return migrationOutput(cmd, o, run)
		}
		runs := []api.MigrationRun{}
		q := url.Values{"limit": {"100"}}
		for {
			var page api.List[api.MigrationRun]
			if err := c.Get(cmd.Context(), "/imports/runs", q, &page); err != nil {
				return err
			}
			runs = append(runs, page.Items...)
			if page.Continue == "" {
				break
			}
			q.Set("continue", page.Continue)
		}
		return migrationOutput(cmd, o, runs)
	}}
}
func newMigrationDecommissionCmd(o *cliOpts) *cobra.Command {
	var names, addresses []string
	var apply, useVIPs bool
	var file string
	cmd := &cobra.Command{Use: "decommission <run-id>", Short: "Preview or apply kudurru nameservers for a migration", Long: "Replace apex NS and SOA primary names in zones tracked by this migration, creating A/AAAA records when serving IPs or VIPs are selected. Preview using the destination hostname or IP by default, explicit --nameserver or --nameserver-address values, or --use-vips. Save the JSON output, then apply that reviewed file with --apply --plan-file. The source server is not shut down; update parent delegations and glue separately.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		path := "/imports/runs/" + url.PathEscape(args[0]) + "/decommission"
		if apply {
			if file == "" || len(names) > 0 || len(addresses) > 0 || useVIPs {
				return fmt.Errorf("apply requires --plan-file with the reviewed JSON preview, without nameserver options")
			}
			b, err := os.ReadFile(file) //nolint:gosec // user-selected reviewed migration plan
			if err != nil {
				return err
			}
			var plan api.MigrationCutoverPlan
			if err := json.Unmarshal(b, &plan); err != nil {
				return fmt.Errorf("read JSON cutover preview: %w", err)
			}
			var result api.MigrationResult
			if err := c.Post(cmd.Context(), path, nil, plan, &result); err != nil {
				return err
			}
			return migrationResultOutput(cmd, o, result)
		}
		if file != "" {
			return fmt.Errorf("--plan-file requires --apply")
		}
		ns, ips, err := migrationTargets(cmd.Context(), o, names, addresses, useVIPs, true)
		if err != nil {
			return err
		}
		var plan api.MigrationCutoverPlan
		if err := c.Post(cmd.Context(), path+"/preview", nil, map[string]any{"nameservers": ns, "addresses": ips}, &plan); err != nil {
			return err
		}
		return migrationOutput(cmd, o, plan)
	}}
	cmd.Flags().StringSliceVar(&names, "nameserver", nil, "kudurru authoritative DNS names (repeatable; first becomes SOA primary)")
	cmd.Flags().StringSliceVar(&addresses, "nameserver-address", nil, "kudurru serving IPs or VIPs; generate in-zone NS names and A/AAAA records (repeatable)")
	cmd.Flags().BoolVar(&useVIPs, "use-vips", false, "use discovered kudurru VIPs for nameservers and address records")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply a saved and reviewed cutover preview")
	cmd.Flags().StringVar(&file, "plan-file", "", "JSON preview to apply (produce with -o json)")
	return cmd
}
func newMigrationTrackCmd(o *cliOpts) *cobra.Command {
	var apply bool
	var file string
	cmd := &cobra.Command{Use: "track-existing", Short: "Review or track previous API imports without reimporting records", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		if !apply {
			if file != "" {
				return fmt.Errorf("--plan-file requires --apply")
			}
			var candidates api.List[api.MigrationAuthorityChange]
			if err := c.Get(cmd.Context(), "/imports/existing", nil, &candidates); err != nil {
				return err
			}
			return migrationOutput(cmd, o, candidates)
		}
		if file == "" {
			return fmt.Errorf("save and review the -o json preview, then pass --plan-file with --apply")
		}
		b, err := os.ReadFile(file) //nolint:gosec // user-selected reviewed migration plan
		if err != nil {
			return err
		}
		var candidates api.List[api.MigrationAuthorityChange]
		if err := json.Unmarshal(b, &candidates); err != nil {
			return err
		}
		var result api.MigrationResult
		if err := c.Post(cmd.Context(), "/imports/adopt", nil, map[string]any{"zones": candidates.Items}, &result); err != nil {
			return err
		}
		return migrationResultOutput(cmd, o, result)
	}}
	cmd.Flags().BoolVar(&apply, "apply", false, "track the reviewed previous imports")
	cmd.Flags().StringVar(&file, "plan-file", "", "JSON candidate list to track (produce with -o json; remove unwanted items before applying)")
	return cmd
}

// migrationTargets only derives identity from the destination context. Explicit
// names/IPs always win; VIP discovery is opt-in and never trusts source settings.
func migrationTargets(ctx context.Context, o *cliOpts, names, addresses []string, useVIPs, defaults bool) ([]string, []string, error) {
	if (len(names) > 0 && len(addresses) > 0) || (useVIPs && (len(names) > 0 || len(addresses) > 0)) {
		return nil, nil, fmt.Errorf("choose --nameserver, --nameserver-address or --use-vips")
	}
	if useVIPs {
		c, _, err := o.client()
		if err != nil {
			return nil, nil, err
		}
		var edge api.EdgeInfo
		if err := c.Get(ctx, "/edge", nil, &edge); err != nil {
			return nil, nil, err
		}
		for _, vip := range edge.VIPs {
			if vip.Address != "" {
				addresses = append(addresses, vip.Address)
			}
		}
		if len(addresses) == 0 {
			return nil, nil, fmt.Errorf("no kudurru VIP addresses discovered; provide --nameserver-address or --nameserver")
		}
		// Deduplicate before imposing the target limit: a VIP can appear on every node.
		unique := map[string]bool{}
		for _, value := range addresses {
			canonical, err := importers.NameserverAddresses([]string{value})
			if err != nil {
				return nil, nil, err
			}
			unique[canonical[0]] = true
		}
		addresses = nil
		for value := range unique {
			addresses = append(addresses, value)
		}
		sort.Strings(addresses)
	}
	if defaults && len(names) == 0 && len(addresses) == 0 {
		r, err := o.resolve()
		if err != nil {
			return nil, nil, err
		}
		u, err := url.Parse(r.Server)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid destination URL")
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if _, err := netip.ParseAddr(host); err == nil {
			addresses = []string{host}
		} else {
			if host == "localhost" || strings.HasSuffix(host, ".localhost") {
				return nil, nil, fmt.Errorf("destination is localhost; provide kudurru's --nameserver or --nameserver-address")
			}
			names = []string{host}
		}
	}
	ns, err := importers.Nameservers(names)
	if err != nil {
		return nil, nil, err
	}
	ips, err := importers.NameserverAddresses(addresses)
	if err != nil {
		return nil, nil, err
	}
	if ns == nil {
		ns = []string{}
	}
	if ips == nil {
		ips = []string{}
	}
	return ns, ips, nil
}
