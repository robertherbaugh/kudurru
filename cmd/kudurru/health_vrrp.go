package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/health"
)

// vrrpProbeFlags are the keepalived track_script flags (§3.6), shared by
// `kudurru vip probe` and `kudurru health --vrrp`.
type vrrpProbeFlags struct {
	dns, qname, api, keyFile string
	require                  int
}

func (f *vrrpProbeFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.dns, "dns", "", "VIP address:port to probe over udp and tcp (loopback is probed while this node does not hold the VIP)")
	fl.StringVar(&f.qname, "qname", "health.kudurru.internal.", "name queried for TXT")
	fl.StringVar(&f.api, "dnsdist-api", "http://127.0.0.1:8083", "local dnsdist API; empty skips the backend check")
	fl.StringVar(&f.keyFile, "dnsdist-api-key-file", "/run/kudurru/dnsdist-api.key", "dnsdist API key published by `kudurru edge run`")
	fl.IntVar(&f.require, "require-backends", 1, "minimum number of dnsdist backends up")
}

func (f *vrrpProbeFlags) run(cmd *cobra.Command, timeout time.Duration) error {
	if f.dns == "" {
		return fmt.Errorf("--dns is required")
	}
	res, err := health.VRRPProbe{
		DNS: f.dns, QName: f.qname, DNSDistAPI: f.api, DNSDistAPIKeyFile: f.keyFile,
		RequireBackends: f.require, Timeout: timeout,
	}.Run(cmd.Context())
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), res.String())
	return err
}

// newVIPProbeCmd is `kudurru vip probe`, the command keepalived.conf
// renders by default. It is the §3.6 `kudurru health --vrrp` under the vip
// command: the health command lives in health.go (CLI package), which can
// alias it with installVRRPHealth.
func newVIPProbeCmd() *cobra.Command {
	var (
		f       vrrpProbeFlags
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "VRRP health probe for keepalived's track_script (FAULT on failure)",
		Long: "Queries --qname over udp and tcp on the VIP while this node holds it (loopback otherwise) and requires\n" +
			"--require-backends backends up in the local dnsdist. keepalived runs it every second with a 1 s timeout.",
		RunE: func(c *cobra.Command, _ []string) error { return f.run(c, timeout) },
	}
	f.register(cmd)
	cmd.Flags().DurationVar(&timeout, "timeout", 900*time.Millisecond, "overall probe timeout (keepalived's script timeout is 1 s)")
	return cmd
}

// installVRRPHealth adds `--vrrp` and the probe flags to an existing
// `kudurru health` command (health.go) and dispatches to the probe when
// --vrrp is set, otherwise to the command's own RunE. The health
// command's --timeout, when present, bounds the probe (capped at 900 ms
// unless set explicitly, below keepalived's 1 s script timeout). Wiring:
// call installVRRPHealth(cmd) at the end of newHealthCmd.
func installVRRPHealth(cmd *cobra.Command) {
	var (
		f    vrrpProbeFlags
		vrrp bool
	)
	f.register(cmd)
	cmd.Flags().BoolVar(&vrrp, "vrrp", false, "run the keepalived VRRP probe (see `kudurru vip probe`)")
	orig := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if !vrrp {
			if orig == nil {
				return nil
			}
			return orig(c, args)
		}
		timeout := 900 * time.Millisecond
		if fl := c.Flags().Lookup("timeout"); fl != nil && fl.Changed {
			if d, err := time.ParseDuration(fl.Value.String()); err == nil {
				timeout = d
			}
		}
		return f.run(c, timeout)
	}
}
