package main

import (
	"encoding/json"
	"time"

	"github.com/robertherbaugh/kudurru/internal/integrationconformance"
	"github.com/spf13/cobra"
)

func newIntegrationWebhookCmd() *cobra.Command {
	group := &cobra.Command{Use: "webhook", Short: "Test an explicit external webhook protocol target"}
	var cfg integrationconformance.Config
	cmd := &cobra.Command{
		Use: "conformance URL", Short: "Probe webhook protocol shapes without retries or ambient credentials", Args: cobra.ExactArgs(1),
		Long: "Probe an explicit webhook URL and print a sanitized JSON report. Read probes run by default; untested mutations are reported skipped. Mutating probes require --allow-mutations and explicit input files. Use a disposable target: acknowledged or uncertain effects are never retried or cleaned up automatically. This command does not activate an integration or prove persistence.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.URL = args[0]
			report, err := integrationconformance.Run(cmd.Context(), cfg)
			if len(report.Checks) > 0 {
				if e := json.NewEncoder(cmd.OutOrStdout()).Encode(report); e != nil {
					return e
				}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.CAFile, "ca-file", "", "Explicit PEM trust roots for this target")
	f.StringVar(&cfg.BearerFile, "bearer-file", "", "Explicit target bearer token file (not the CLI API token)")
	f.StringVar(&cfg.CertFile, "client-cert", "", "Explicit PEM client certificate for mTLS")
	f.StringVar(&cfg.KeyFile, "client-key", "", "Explicit PEM client private key for mTLS")
	f.DurationVar(&cfg.Timeout, "run-timeout", time.Minute, "Whole run deadline, at most 5m")
	f.DurationVar(&cfg.RequestTimeout, "request-timeout", 10*time.Second, "Per-request deadline within the original run deadline")
	f.StringVar(&cfg.AdjustFile, "endpoints-file", "", "Endpoint array for normalization (default empty array)")
	f.BoolVar(&cfg.AllowMutations, "allow-mutations", false, "Permit only explicitly supplied mutation probes")
	f.StringVar(&cfg.ChangesFile, "changes-file", "", "Exact external-dns Changes JSON to apply once")
	f.StringVar(&cfg.ExpectFile, "expect-endpoints-file", "", `Read-back expectation {"contain":[endpoints],"absent":[endpoints]} checked against the record list after any apply`)
	f.StringVar(&cfg.EventFile, "event-file", "", "Exact event JSON to deliver once")
	f.StringVar(&cfg.HMACFile, "hmac-file", "", "Raw event HMAC-SHA256 key, 32 to 4096 bytes")
	f.StringVar(&cfg.Action, "action", "", "Exact advertised action to invoke once")
	f.StringVar(&cfg.ParamsFile, "params-file", "", "Exact JSON object of action parameters")
	group.AddCommand(cmd)
	return group
}
