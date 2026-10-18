package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

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
	var names []string
	var apply bool
	var file string
	cmd := &cobra.Command{Use: "decommission <run-id>", Short: "Preview or apply kudurru nameservers for a migration", Long: "Replace apex NS and SOA primary names only in zones tracked by this migration. First preview with --nameserver and save the JSON output, then apply that reviewed file with --apply --plan-file. The source server is not shut down; update parent delegations and glue separately.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		path := "/imports/runs/" + url.PathEscape(args[0]) + "/decommission"
		if apply {
			if file == "" || len(names) > 0 {
				return fmt.Errorf("apply requires --plan-file with the reviewed JSON preview, without --nameserver")
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
		ns, err := importers.Nameservers(names)
		if err != nil {
			return err
		}
		if len(ns) == 0 {
			return fmt.Errorf("provide at least one --nameserver DNS name")
		}
		var plan api.MigrationCutoverPlan
		if err := c.Post(cmd.Context(), path+"/preview", nil, map[string]any{"nameservers": ns}, &plan); err != nil {
			return err
		}
		return migrationOutput(cmd, o, plan)
	}}
	cmd.Flags().StringSliceVar(&names, "nameserver", nil, "kudurru authoritative DNS names (repeatable; first becomes SOA primary)")
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
