package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// lameduckPath is the ops-listener endpoint that puts the server into
// lameduck: readiness answers 503, the health.kudurru.internal. TXT
// check fails so dnsdist drains the node (D51), and the VIP stays.
const lameduckPath = "/lameduck"

// lameduckRequest is the body of POST /lameduck.
type lameduckRequest struct {
	Seconds int `json:"seconds"`
}

// parseLameduckDuration accepts a Go duration ("5s", "1m") or a plain
// number of seconds ("5").
func parseLameduckDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		s = strconv.Itoa(n) + "s"
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("lameduck: %q is not a duration (e.g. 5s)", s)
	}
	return d, nil
}

func newLameduckCmd() *cobra.Command {
	var (
		opsAddr string
		noWait  bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "lameduck [duration]",
		Short: "Drain this node before shutdown (Kubernetes preStop: kudurru lameduck 5s)",
		Long: "Asks the local server (POST /lameduck on the ops listener) to enter lameduck: readiness fails and\n" +
			"health.kudurru.internal. stops answering, so Services and dnsdist drain the node while it keeps\n" +
			"serving in-flight clients. The VIP never moves (D51; `kudurru vip drain` does that). The command then\n" +
			"waits for the duration (default 5s) so the drain completes before the container gets SIGTERM;\n" +
			"--no-wait returns at once.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			d := 5 * time.Second
			if len(args) == 1 {
				var err error
				if d, err = parseLameduckDuration(args[0]); err != nil {
					return err
				}
			}
			secs := int((d + time.Second - 1) / time.Second)
			body, _ := json.Marshal(lameduckRequest{Seconds: secs})
			req, err := http.NewRequestWithContext(c.Context(), http.MethodPost, "http://"+opsAddr+lameduckPath, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := (&http.Client{Timeout: timeout}).Do(req)
			if err != nil {
				return fmt.Errorf("lameduck: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			switch {
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
				return fmt.Errorf("lameduck: %s answers %s for POST %s: this server predates the lameduck endpoint; "+
					"use a preStop sleep instead", opsAddr, resp.Status, lameduckPath)
			case resp.StatusCode/100 != 2:
				return fmt.Errorf("lameduck: POST %s returned %s: %s", lameduckPath, resp.Status, strings.TrimSpace(string(msg)))
			}
			w := c.OutOrStdout()
			_, _ = fmt.Fprintf(w, "lameduck: %s draining for %s\n", opsAddr, d)
			if noWait || d == 0 {
				return nil
			}
			select {
			case <-c.Context().Done():
				return c.Context().Err()
			case <-time.After(d):
			}
			_, _ = fmt.Fprintln(w, "lameduck: drained")
			return nil
		},
	}
	cmd.Flags().StringVar(&opsAddr, "ops-address", "127.0.0.1:8081", "ops listener of the local server")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return after the server acknowledged instead of waiting for the duration")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "request timeout")
	return cmd
}
