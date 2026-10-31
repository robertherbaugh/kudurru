package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

const maxMigrationPollFailures = 3

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
		Long:    "Connect to the source API using its " + credential + ". By default, print a preview containing converted objects, settings and warnings without changing either server. Add --apply to start a tracked background import on the selected kudurru server using its normal validation, ownership and license checks. Without --wait, exit status 0 means the server accepted the job, not that every item imported successfully. Scripts that need the final success/failure exit status must use --apply --wait. Use import runs RUN_ID to inspect the report later. All items are selected by default; --select-object and --select-setting select only the named items. Inspect every result: writes are not transactional. Source credentials are never stored by kudurru.",
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
			if err := selectMigrationPlan(cmd, plan, selectedObjects, selectedSettings, skipSettings); err != nil {
				return err
			}
			return finishMigration(cmd, o, plan, f, apply, wait)
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
	cmd.MarkFlagsMutuallyExclusive("skip-settings", "select-setting")
	_ = cmd.MarkFlagRequired("source-url")
	return cmd
}

// Retry bounded read failures without replaying the import-start request.
func transientMigrationPollError(err error) bool {
	var apiError *apiclient.Error
	if errors.As(err, &apiError) {
		return apiError.StatusCode == http.StatusRequestTimeout || apiError.StatusCode == http.StatusTooManyRequests || apiError.StatusCode >= http.StatusInternalServerError
	}
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
