package main

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/health"
)

// newLameduckCmd is `kudurru lameduck [duration]` (§3.5, D51): it puts the
// local node into lameduck over the ops listener and then waits duration,
// so a Kubernetes preStop hook or a rolling restart script drains the node
// before SIGTERM. In lameduck readiness fails (the Service drops the pod)
// and the health name health.kudurru.internal. answers SERVFAIL (dnsdist's
// must-resolve check drains the node from every backend pool) while every
// other query is still answered. The VIP never moves (D51).
func newLameduckCmd() *cobra.Command {
	var (
		opsAddr string
		cancel  bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "lameduck [duration]",
		Short: "Drain this node (readiness and the dnsdist health name fail), then wait",
		Long: `Put the local kudurru into lameduck through the ops listener (POST
/lameduck, loopback only) and wait for the duration, if one is given, so the
edge's health checks (dnsdist must-resolve on health.kudurru.internal.,
every second) and the Kubernetes endpoints take the node out before it
stops. Queries that still arrive are answered. --cancel leaves lameduck.

In Kubernetes use it as the preStop hook:
  lifecycle: { preStop: { exec: { command: ["kudurru", "lameduck", "5s"] } } }`,
		Example: `  kudurru lameduck 5s
  kudurru lameduck --cancel`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var wait time.Duration
			if len(args) == 1 {
				d, err := time.ParseDuration(args[0])
				if err != nil || d < 0 {
					return fmt.Errorf("duration %q: want e.g. 5s or 1m", args[0])
				}
				wait = d
			}
			method := http.MethodPost
			if cancel {
				method = http.MethodDelete
			}
			req, err := http.NewRequestWithContext(c.Context(), method, "http://"+opsAddr+health.PathLameduck, nil)
			if err != nil {
				return err
			}
			resp, err := (&http.Client{Timeout: timeout}).Do(req)
			if err != nil {
				return fmt.Errorf("lameduck: %w (is kudurru serve running with listeners.ops.address reachable on %s?)", err, opsAddr)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("lameduck: %s: %s", resp.Status, body)
			}
			out := c.OutOrStdout()
			if cancel {
				_, err = fmt.Fprintln(out, "lameduck off: serving and ready again")
				return err
			}
			_, _ = fmt.Fprintln(out, "lameduck on: readiness fails and health.kudurru.internal. answers SERVFAIL")
			if wait > 0 {
				_, _ = fmt.Fprintf(out, "draining for %s\n", wait)
				select {
				case <-time.After(wait):
				case <-c.Context().Done():
					return c.Context().Err()
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opsAddr, "ops-address", "127.0.0.1:8081", "ops listener of the local node (loopback)")
	cmd.Flags().BoolVar(&cancel, "cancel", false, "leave lameduck")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "request timeout")
	return cmd
}
