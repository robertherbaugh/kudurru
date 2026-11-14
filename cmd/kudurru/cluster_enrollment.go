package main

import (
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/spf13/cobra"
)

func addClusterEnrollmentCommands(root *cobra.Command, o *cliOpts) {
	root.AddCommand(&cobra.Command{Use: "enrollment", Short: "Review the local SQLite enrollment source", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := o.format(); err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out api.ClusterEnrollmentInfo
		if err := c.Get(cmd.Context(), "/cluster/enrollment", nil, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}})
	var in api.ActivateClusterCommand
	activate := &cobra.Command{Use: "activate", Short: "Activate an explicitly reviewed standalone SQLite primary", Args: cobra.NoArgs,
		Long: `Activate the first member from an explicitly dormant standalone SQLite owner.
First inspect cluster enrollment and separately review its four exact identity fields.
Pass those fields with the local member name, internal host:port address, and reason.
The command sends one request and never refreshes, redirects or retries it. A confirmed
commit requires a process restart to mount the enrolled internal listener. A lost or
unknown response does not prove rollback: inspect enrollment before a new decision.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.format(); err != nil {
				return err
			}
			if !validClusterReview(in.ClusterEnrollmentReview) || in.Name == "" || in.Address == "" || in.Reason == "" {
				return errors.New("all four reviewed identity flags, --name, --address and --reason are required")
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var out api.ClusterActivationReceipt
			if err := c.ActivateCluster(cmd.Context(), in, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}}
	activate.Flags().StringVar(&in.ClusterID, "cluster-id", "", "Exact reviewed clusterID")
	activate.Flags().StringVar(&in.MemberID, "member-id", "", "Exact reviewed memberID")
	activate.Flags().StringVar(&in.SourceDigest, "source-digest", "", "Exact reviewed sourceDigest")
	activate.Flags().StringVar(&in.OwnerDigest, "owner-digest", "", "Exact reviewed ownerDigest")
	activate.Flags().StringVar(&in.Name, "name", "", "Local member name")
	activate.Flags().StringVar(&in.Address, "address", "", "Reachable internal member host:port")
	activate.Flags().StringVar(&in.Reason, "reason", "", "Audit reason for activating HA")
	root.AddCommand(activate)
	addClusterJoinTokenCommand(root, o)
}

func addClusterJoinTokenCommand(root *cobra.Command, o *cliOpts) {
	var in api.ClusterJoinTokenCommand
	var expires string
	command := &cobra.Command{Use: "join-token", Short: "Issue an invitation from an explicitly reviewed primary", Args: cobra.NoArgs,
		Long: `Issue one invitation for a target member from a separately reviewed primary.
Supply all four identity fields from cluster enrollment, the target name and internal
host:port, an absolute RFC3339 expiry (within ten minutes), and an audit reason.
The response contains a bearer secret: transfer it privately to the intended member.
Only its hash is retained by the primary. This command never refreshes, redirects or
retries; a lost or unknown response does not prove rollback. Do not replay it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.format(); err != nil {
				return err
			}
			var err error
			in.ExpiresAt, err = time.Parse(time.RFC3339, expires)
			if err != nil || !validClusterReview(in.ClusterEnrollmentReview) || in.Name == "" || in.Address == "" || in.Reason == "" {
				return errors.New("all four reviewed identity flags, --name, --address, --expires-at (RFC3339) and --reason are required")
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var out api.ClusterJoinTokenReceipt
			if err := c.IssueClusterJoinToken(cmd.Context(), in, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}}
	command.Flags().StringVar(&in.ClusterID, "cluster-id", "", "Exact reviewed clusterID")
	command.Flags().StringVar(&in.MemberID, "member-id", "", "Exact reviewed primary memberID")
	command.Flags().StringVar(&in.SourceDigest, "source-digest", "", "Exact reviewed sourceDigest")
	command.Flags().StringVar(&in.OwnerDigest, "owner-digest", "", "Exact reviewed ownerDigest")
	command.Flags().StringVar(&in.Name, "name", "", "Target member name")
	command.Flags().StringVar(&in.Address, "address", "", "Target internal member host:port")
	command.Flags().StringVar(&expires, "expires-at", "", "Original absolute invitation expiry (RFC3339, at most ten minutes)")
	command.Flags().StringVar(&in.Reason, "reason", "", "Audit reason for issuing the invitation")
	root.AddCommand(command)
}
