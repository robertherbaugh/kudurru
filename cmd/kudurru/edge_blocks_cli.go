package main

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// newEdgeBlockCmds are `kudurru edge block|blocks|unblock` (D388): the
// operator edge block commands of the API. They use the API endpoint and
// credentials of every other client command, not the supervisor flags.
func newEdgeBlockCmds(o *cliOpts) []*cobra.Command {
	var reason, until string
	block := &cobra.Command{
		Use:   "block <address|prefix> [duration]",
		Short: "Drop a client address or prefix at every dnsdist edge for a while (1m to 7d, default 10m)",
		Example: "  kudurru edge block 192.0.2.7 10m --reason 'query flood'\n" +
			"  kudurru edge block 2001:db8:1::/48 --until 2026-11-10T18:00:00Z",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"prefix": args[0]}
			if reason != "" {
				body["reason"] = reason
			}
			switch {
			case until != "" && len(args) == 2:
				return fmt.Errorf("give a duration or --until, not both")
			case until != "":
				t, err := time.Parse(time.RFC3339, until)
				if err != nil {
					return fmt.Errorf("--until: %w", err)
				}
				body["until"] = t.UTC().Format(time.RFC3339)
			default:
				secs := 600
				if len(args) == 2 {
					var err error
					if secs, err = parseSeconds(args[1]); err != nil {
						return err
					}
				}
				body["seconds"] = secs
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var b api.EdgeBlock
			if err := c.Post(cmd.Context(), "/edge/blocks", nil, body, &b); err != nil {
				return err
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, b)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "edge block %s: %s dropped until %s; edges apply it within a few seconds\n", b.ID, b.Prefix, b.Until.Format(time.RFC3339))
			return err
		},
	}
	block.Flags().StringVar(&reason, "reason", "", "note kept in the block list and the audit trail (never sent to edges)")
	block.Flags().StringVar(&until, "until", "", "block until this RFC 3339 time instead of for a duration")
	list := &cobra.Command{
		Use:   "blocks",
		Short: "List the active operator edge blocks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var l api.EdgeBlockList
			if err := c.Get(cmd.Context(), "/edge/blocks", nil, &l); err != nil {
				return err
			}
			return o.render(cmd, l, func(wide bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"ID", "PREFIX", "UNTIL", "LEFT"}}
				if wide {
					t.Headers = append(t.Headers, "CREATED-BY", "REASON")
				}
				for _, b := range l.Items {
					row := []string{b.ID, b.Prefix, b.Until.Format(time.RFC3339), max(b.Until.Sub(o.now()).Round(time.Second), 0).String()}
					if wide {
						row = append(row, firstNonEmpty(b.CreatedBy, "-"), firstNonEmpty(b.Reason, "-"))
					}
					t.Add(row...)
				}
				return t, nil
			})
		},
	}
	unblock := &cobra.Command{
		Use:   "unblock <id>",
		Short: "Revoke an operator edge block before it expires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The ID goes into the request path: refuse anything that is not
			// one, so the operator's credential never reaches another route.
			if !edgeblock.ValidID(args[0]) {
				return fmt.Errorf("edge block ID %q is not %d lowercase hex characters (see kudurru edge blocks)", args[0], 2*edgeblock.IDBytes)
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var b api.EdgeBlock
			if err := c.Do(cmd.Context(), http.MethodDelete, "/edge/blocks/"+url.PathEscape(args[0]), nil, nil, &b); err != nil {
				return err
			}
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, b)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "edge block %s revoked: %s; edges stop dropping it within a few seconds\n", b.ID, b.Prefix)
			return err
		},
	}
	return []*cobra.Command{block, list, unblock}
}
