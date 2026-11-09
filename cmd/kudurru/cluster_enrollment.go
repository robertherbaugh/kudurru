package main

import (
	"errors"

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
}
