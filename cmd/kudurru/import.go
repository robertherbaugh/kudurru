package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

func newMigrationCmd(o *cliOpts, source string) *cobra.Command {
	var sourceURL, keyFile string
	var apply, dryRun, insecure bool
	var localZones, nameservers []string
	env := "TECHNITIUM_API_TOKEN"
	credential := "API token"
	if source == "pihole" {
		env = "PIHOLE_APP_PASSWORD"
		credential = "v6 application password"
	}
	cmd := &cobra.Command{
		Use: source + " --source-url URL", Short: "Import from a live " + source + " API",
		Long:    "Connect to the source API using its " + credential + ". By default, print a preview containing converted objects, settings and warnings without changing either server. Add --apply to import directly into the selected kudurru server using its normal validation, ownership and license checks. Inspect every result: writes are not transactional. Source credentials are never stored by kudurru.",
		Example: "  kudurru import " + source + " --source-url http://dns.local --source-key-file ./source-key\n  kudurru import " + source + " --source-url http://dns.local --source-key-file ./source-key --apply",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && dryRun {
				return fmt.Errorf("--apply and --dry-run cannot be used together")
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
			plan, err := importers.Preview(cmd.Context(), importers.MigrationRequest{Source: source, URL: sourceURL, Credential: key, Namespace: ns, InsecureSkipVerify: insecure, LocalZones: localZones, Nameservers: nameservers})
			if err != nil {
				return err
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
			var result api.MigrationResult
			if err := c.Post(cmd.Context(), "/imports/apply", nil, plan, &result); err != nil {
				return err
			}
			failed := result.SettingsError != "" || result.JournalError != ""
			for _, r := range result.Items {
				if r.Result == api.ResultError {
					failed = true
				}
			}
			if err := cli.WriteStructured(cmd.OutOrStdout(), f, result); err != nil {
				return err
			}
			if failed {
				return fmt.Errorf("migration incomplete; inspect the object and settings errors above")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceURL, "source-url", "", "source console URL (including any reverse-proxy prefix)")
	cmd.Flags().StringVar(&keyFile, "source-key-file", "", "file containing the source "+credential+" (otherwise $"+env+")")
	cmd.Flags().BoolVar(&insecure, "source-insecure-skip-tls-verify", false, "skip source TLS verification for a lab with a self-signed certificate")
	cmd.Flags().StringSliceVar(&nameservers, "nameserver", nil, "kudurru authoritative DNS names; replace imported NS and SOA primary (repeatable; first is primary)")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the converted configuration directly to kudurru")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview only (the default)")
	if source == "pihole" {
		cmd.Flags().StringSliceVar(&localZones, "local-zone", nil, "local DNS suffixes to make authoritative (default: Pi-hole's local domain)")
	}
	_ = cmd.MarkFlagRequired("source-url")
	return cmd
}
