package main

import (
	"errors"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

func newAuditVerifyCmd(o *cliOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Verify the retained audit trail and available batch seals",
		Long: `Ask the server to verify the complete retained audit trail and available
batch seals. Requires audit.read and an audit license. The server caches
verification results for 30 seconds; checkedAt identifies the scan.

Use -o json for a structured result. Exits nonzero for an integrity failure,
unavailable verification, or denied access.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := o.format()
			if err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var result api.AuditVerification
			if err := c.Get(cmd.Context(), "/audit/verify", nil, &result); err != nil {
				return err
			}
			if f.Structured() {
				err = cli.WriteStructured(cmd.OutOrStdout(), f, result)
			} else {
				pairs := [][2]string{
					{"Verified", strconv.FormatBool(result.Verified)},
					{"Checked at", result.CheckedAt.UTC().Format(time.RFC3339)},
				}
				if result.Reason != "" {
					pairs = append(pairs, [2]string{"Reason", result.Reason})
				}
				err = cli.KeyValues(cmd.OutOrStdout(), "", pairs)
			}
			if err != nil {
				return err
			}
			if !result.Verified {
				return errors.New("audit verification failed")
			}
			return nil
		},
	}
}
