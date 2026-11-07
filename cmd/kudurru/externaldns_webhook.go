package main

import (
	"errors"
	"net"
	"net/netip"

	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/spf13/cobra"
)

func newExternalDNSWebhookCmd(o *cliOpts) *cobra.Command {
	var options externaldns.RelayOptions
	var address string
	cmd := &cobra.Command{
		Use: "externaldns-webhook", Short: "Relay a pod-local external-dns webhook to a configured Kudurru server",
		Long: "Run beside external-dns and relay its four webhook operations to an existing Kudurru webhook.\nThe upstream server retains licensing, namespace, zone selection, ownership and write admission.\nOnly literal loopback listeners are accepted. Upstream TLS and token files are captured at startup;\nrestart after rotation. No redirect or failed operation is automatically replayed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.server != "" || o.token != "" || o.caFile != "" || o.insecure || o.cliConfig != "" {
				return errors.New("externaldns-webhook requires its explicit upstream options; API connection overrides are not accepted")
			}
			addr, err := netip.ParseAddrPort(address)
			if err != nil || !addr.Addr().Unmap().IsLoopback() {
				return errors.New("externaldns-webhook --listen must be a literal loopback address and port")
			}
			relay, err := externaldns.NewRelay(options)
			if err != nil {
				return err
			}
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			ln, err := net.Listen("tcp", address)
			if err != nil {
				return errors.New("externaldns-webhook listener unavailable")
			}
			return relay.Serve(cmd.Context(), ln)
		},
	}
	cmd.Flags().StringVar(&address, "listen", "127.0.0.1:8888", "pod-local literal loopback listener")
	cmd.Flags().StringVar(&options.Upstream, "upstream", "", "fixed existing webhook HTTPS URL, including any configured prefix")
	cmd.Flags().StringVar(&options.TokenFile, "upstream-token-file", "", "regular file holding the upstream webhook bearer token (required for remote servers)")
	cmd.Flags().StringVar(&options.CAFile, "upstream-ca-file", "", "optional PEM CA bundle for upstream HTTPS, added to system trust")
	return cmd
}
