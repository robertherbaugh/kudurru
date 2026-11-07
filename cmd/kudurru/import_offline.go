package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/spf13/cobra"
)

type offlinePreview func(context.Context, importers.OfflineRequest, io.Reader) (*importers.Plan, error)

func newOfflineMigrationCmd(o *cliOpts, source string, preview offlinePreview) *cobra.Command {
	var file string
	var apply, dryRun, wait, skipSettings, useVIPs, updateNameservers bool
	var objects, settings, names, addresses, zones []string
	cmd := &cobra.Command{Use: source + " --file PATH", Short: "Preview or import an offline " + source + " YAML configuration", Args: cobra.NoArgs,
		Long: "Convert a single YAML file (at most 4 MiB) without reading referenced files or fetching network sources. Use --file - for stdin. The default prints a partial conversion and a bounded loss report without contacting kudurru. Review unsupported semantics before --apply, which starts the existing tracked migration with normal validation, ownership and license checks. --wait requires --apply and returns the final result; without it, success means only that the job was accepted. Select objects and settings explicitly to limit application. Destination-derived targets (--use-vips or --update-nameservers) explicitly opt into destination access. No source credentials or file paths enter the plan or migration journal.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && dryRun {
				return fmt.Errorf("--apply and --dry-run cannot be used together")
			}
			if wait && !apply {
				return fmt.Errorf("--wait requires --apply")
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			if !f.Structured() {
				f = cli.FormatYAML
			}
			r := o.stdin
			if file != "-" {
				fh, err := os.Open(file) //nolint:gosec // G304: operator-selected input; referenced source paths are never opened.
				if err != nil {
					return fmt.Errorf("cannot open offline source file")
				}
				defer func() { _ = fh.Close() }()
				r = fh
			}
			if r == nil {
				return fmt.Errorf("offline source input is unavailable")
			}
			ns, err := migrationNamespace(o)
			if err != nil {
				return err
			}
			targetNames, targetAddresses, err := migrationTargets(cmd.Context(), o, names, addresses, useVIPs, updateNameservers)
			if err != nil {
				return err
			}
			plan, err := preview(cmd.Context(), importers.OfflineRequest{Namespace: ns, LocalZones: zones, Nameservers: targetNames, NameserverAddresses: targetAddresses}, r)
			if err != nil {
				return err
			}
			if err := selectMigrationPlan(cmd, plan, objects, settings, skipSettings); err != nil {
				return err
			}
			if err := admitOfflinePlan(plan); err != nil {
				return err
			}
			return finishMigration(cmd, o, plan, f, apply, wait)
		}}
	cmd.Flags().StringVarP(&file, "file", "f", "", "source YAML file; - reads stdin (maximum 4 MiB)")
	cmd.Flags().BoolVar(&apply, "apply", false, "start a tracked background migration of the reviewed conversion")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview only (the default)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for final migration results (requires --apply)")
	cmd.Flags().BoolVar(&skipSettings, "skip-settings", false, "leave settings unchanged")
	cmd.Flags().StringSliceVar(&objects, "select-object", nil, "import only these Kind/[namespace/]name objects (repeatable)")
	cmd.Flags().StringSliceVar(&settings, "select-setting", nil, "import only these top-level settings sections (repeatable)")
	cmd.Flags().StringSliceVar(&names, "nameserver", nil, "replace imported authority with these DNS names")
	cmd.Flags().StringSliceVar(&addresses, "nameserver-address", nil, "generate authority from these serving IPs or VIPs")
	cmd.Flags().BoolVar(&useVIPs, "use-vips", false, "use discovered destination VIPs (explicit destination API access)")
	cmd.Flags().BoolVar(&updateNameservers, "update-nameservers", false, "derive authority from the destination context hostname or IP")
	if source == "adguard" {
		cmd.Flags().StringSliceVar(&zones, "local-zone", nil, "explicit local suffixes to make authoritative for exact IP/CNAME rewrites")
	}
	cmd.MarkFlagsMutuallyExclusive("skip-settings", "select-setting")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// Selection does not strip unselected plan entries. Check the exact encoded
// request before printing a preview or attempting an import-start request.
func admitOfflinePlan(plan *importers.Plan) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("offline conversion cannot be encoded")
	}
	if len(raw) > api.MaxRequestBodyBytes {
		return fmt.Errorf("generated offline plan exceeds the public API 32 MiB request limit; reduce the source configuration (selection retains unselected entries)")
	}
	return nil
}
