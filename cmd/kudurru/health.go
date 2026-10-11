package main

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

func newHealthCmd() *cobra.Command {
	var (
		opsAddr string
		ready   bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Probe a running instance (Docker HEALTHCHECK; --vrrp is the keepalived check)",
		RunE: func(c *cobra.Command, _ []string) error {
			path := "/healthz/live"
			if ready {
				path = "/healthz/ready"
			}
			client := &http.Client{Timeout: timeout}
			resp, err := client.Get("http://" + opsAddr + path)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_, _ = fmt.Fprint(c.OutOrStdout(), string(body))
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s returned %s", path, resp.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opsAddr, "ops-address", "127.0.0.1:8081", "ops listener to probe")
	cmd.Flags().BoolVar(&ready, "ready", false, "probe readiness instead of liveness")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "request timeout (with --vrrp: the probe budget, 900ms unless set)")
	// --vrrp turns the command into the keepalived track_script probe
	// (§3.6), the same probe as `kudurru vip probe`.
	installVRRPHealth(cmd)
	return cmd
}
