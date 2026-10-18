package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

func newMigrationCmd(o *cliOpts, source string) *cobra.Command {
	var sourceURL, keyFile string
	var apply, dryRun, insecure, updateNameservers, useVIPs, wait, skipSettings bool
	var localZones, nameservers, addresses, selectedObjects, selectedSettings []string
	env := "TECHNITIUM_API_TOKEN"
	credential := "API token"
	if source == "pihole" {
		env = "PIHOLE_APP_PASSWORD"
		credential = "v6 application password"
	}
	cmd := &cobra.Command{
		Use: source + " --source-url URL", Short: "Import from a live " + source + " API",
		Long:    "Connect to the source API using its " + credential + ". By default, print a preview containing converted objects, settings and warnings without changing either server. Add --apply to start a tracked background import on the selected kudurru server using its normal validation, ownership and license checks. Use --wait to wait for its final report, or import runs RUN_ID to inspect it later. All items are selected by default; --select-object and --select-setting select only the named items. Inspect every result: writes are not transactional. Source credentials are never stored by kudurru.",
		Example: "  kudurru import " + source + " --source-url http://dns.local --source-key-file ./source-key\n  kudurru import " + source + " --source-url http://dns.local --source-key-file ./source-key --apply",
		Args:    cobra.NoArgs,
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
			key := strings.TrimSpace(o.getenv(env))
			if keyFile != "" {
				fh, err := os.Open(keyFile) //nolint:gosec // user-selected credential file
				if err != nil {
					return fmt.Errorf("read source credential file: %w", err)
				}
				b, readErr := io.ReadAll(io.LimitReader(fh, 65537))
				_ = fh.Close()
				if readErr != nil {
					return fmt.Errorf("cannot read source credential file")
				}
				if len(b) > 65536 {
					return fmt.Errorf("source credential file exceeds 64 KiB")
				}
				key = strings.TrimSpace(string(b))
			}
			if key == "" {
				return fmt.Errorf("provide --source-key-file or set %s", env)
			}
			ns := o.namespace
			if ns == "" {
				cfg, err := o.loadConfig()
				if err != nil {
					return err
				}
				name := firstNonEmpty(o.context, o.getenv(cli.EnvContext), cfg.CurrentContext)
				for _, c := range cfg.Contexts {
					if c.Name == name {
						ns = c.Namespace
					}
				}
			}
			nameservers, addresses, err := migrationTargets(cmd.Context(), o, nameservers, addresses, useVIPs, updateNameservers)
			if err != nil {
				return err
			}
			plan, err := importers.Preview(cmd.Context(), importers.MigrationRequest{Source: source, URL: sourceURL, Credential: key, Namespace: ns, InsecureSkipVerify: insecure, LocalZones: localZones, Nameservers: nameservers, NameserverAddresses: addresses})
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("select-object") || cmd.Flags().Changed("select-setting") || skipSettings {
				objects := append([]string{}, selectedObjects...)
				settings := append([]string{}, selectedSettings...)
				if skipSettings && !cmd.Flags().Changed("select-object") && !cmd.Flags().Changed("select-setting") {
					for _, doc := range plan.Objects {
						objects = append(objects, importers.ObjectID(doc))
					}
				}
				if skipSettings {
					settings = []string{}
				}
				plan.Selection = &importers.Selection{Objects: objects, Settings: settings}
				if _, _, err := plan.Selected(); err != nil {
					return err
				}
			}
			for _, w := range plan.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
			}
			if !apply {
				return cli.WriteStructured(cmd.OutOrStdout(), f, plan)
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var run api.MigrationRun
			if err := c.Post(cmd.Context(), "/imports/start", nil, plan, &run); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Import started: %s. Inspect with kudurru import runs %s\n", run.ID, run.ID)
			for wait && run.Data.Status == "running" {
				timer := time.NewTimer(time.Second)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return fmt.Errorf("import continues in the background as %s: %w", run.ID, cmd.Context().Err())
				case <-timer.C:
				}
				if err := c.Get(cmd.Context(), "/imports/runs/"+run.ID, nil, &run); err != nil {
					return err
				}
			}
			if err := cli.WriteStructured(cmd.OutOrStdout(), f, run); err != nil {
				return err
			}
			if wait && run.Data.Status != "completed" {
				return fmt.Errorf("import finished with errors or unconfirmed items; successful writes remain; inspect run %s", run.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceURL, "source-url", "", "source console URL (including any reverse-proxy prefix)")
	cmd.Flags().StringVar(&keyFile, "source-key-file", "", "file containing the source "+credential+" (otherwise $"+env+")")
	cmd.Flags().BoolVar(&insecure, "source-insecure-skip-tls-verify", false, "skip source TLS verification for a lab with a self-signed certificate")
	cmd.Flags().StringSliceVar(&nameservers, "nameserver", nil, "kudurru authoritative DNS names; replace imported NS and SOA primary (repeatable; first is primary)")
	cmd.Flags().StringSliceVar(&addresses, "nameserver-address", nil, "kudurru serving IPs or VIPs; generate in-zone NS names and A/AAAA records (repeatable)")
	cmd.Flags().BoolVar(&useVIPs, "use-vips", false, "use discovered kudurru VIPs for nameservers and address records")
	cmd.Flags().BoolVar(&updateNameservers, "update-nameservers", false, "use the destination context hostname or IP for imported authority")
	cmd.Flags().BoolVar(&apply, "apply", false, "start a tracked background import of the converted configuration")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the background import and report its final results (requires --apply)")
	cmd.Flags().StringSliceVar(&selectedObjects, "select-object", nil, "import only these Kind/[namespace/]name objects (repeatable; combine with --select-setting)")
	cmd.Flags().StringSliceVar(&selectedSettings, "select-setting", nil, "import only these top-level settings sections (repeatable; combine with --select-object)")
	cmd.Flags().BoolVar(&skipSettings, "skip-settings", false, "leave settings unchanged")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview only (the default)")
	if source == "pihole" {
		cmd.Flags().StringSliceVar(&localZones, "local-zone", nil, "local DNS suffixes to make authoritative (default: Pi-hole's local domain)")
	}
	_ = cmd.MarkFlagRequired("source-url")
	return cmd
}
