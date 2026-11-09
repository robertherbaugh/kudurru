package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/spf13/cobra"
)

func addClusterPromotionCommands(root *cobra.Command, o *cliOpts) {
	root.AddCommand(&cobra.Command{Use: "promotion", Short: "Review local follower promotion eligibility", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.format(); err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var out api.ClusterPromotionInfo
			if err := c.Get(cmd.Context(), "/cluster/promotion", nil, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}})
	var review string
	var in api.ClusterPromoteCommand
	command := &cobra.Command{Use: "promote", Short: "Promote an explicitly reviewed enrolled follower", Args: cobra.NoArgs,
		Long: `Promote one enrolled follower after reviewing cluster promotion -o json.
Save that response in a local file and pass --review with the exact local
--confirm-node, a unique --request-id and an audit --reason. The server repeats
permission, source, serving agreement and continuous primary-loss checks.
The command never refreshes, redirects or retries. A lost response does not
prove rollback: inspect current state before a new decision. A committed
receipt does not claim new-term publication or remote convergence.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if _, err := o.format(); err != nil {
				return err
			}
			if review == "" {
				return errors.New("--review, --confirm-node, --request-id and --reason are required")
			}
			f, err := os.Open(review) //nolint:gosec // explicit operator-selected local review file
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
			if err != nil || len(raw) > 64<<10 {
				return errors.New("invalid or oversized promotion review")
			}
			var observed api.ClusterPromotionInfo
			if err := decodeReviewedCommandJSON(raw, &observed, "invalid promotion review JSON"); err != nil {
				return errors.New("invalid promotion review JSON")
			}
			in.ClusterPromotionReview = observed.ClusterPromotionReview
			if !validPromotionCommand(in) || in.ConfirmNode != observed.Node {
				return errors.New("exact reviewed source, matching --confirm-node, --request-id and --reason required")
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var out api.ClusterPromotionReceipt
			if err := c.PromoteCluster(ctx, in, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}}
	command.Flags().StringVar(&review, "review", "", "Local file containing separately reviewed cluster promotion JSON")
	command.Flags().StringVar(&in.ConfirmNode, "confirm-node", "", "Exact reviewed local node name")
	command.Flags().StringVar(&in.RequestID, "request-id", "", "Unique identifier for this operator decision")
	command.Flags().StringVar(&in.Reason, "reason", "", "Audit reason for promoting this follower")
	root.AddCommand(command)
}
