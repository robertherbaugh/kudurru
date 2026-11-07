package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

func newAuditKeysCmd() *cobra.Command {
	root := &cobra.Command{Use: "keys", Short: "Manage an independent private AuditLog key file offline", Long: `Manage an operator-protected 0600 ring file. Publish it separately as the
ring.json entry of audit.keySecret in the release namespace. These commands
never contact a server or print private material. Back up the ring and AuditLog
history separately; ordinary configuration backups contain neither.

Before upgrading existing AuditLog storage, explicitly import each retained
legacy master-key file and set a past --legacy-before cutoff. Missing old keys
cannot be recovered. Rotation preserves public keys; revocation withdraws trust
in affected history. Keep the containing directory private and do not remove
the persistent .lock file while commands may run.`}
	for _, action := range []string{"init", "rotate", "revoke"} {
		var path, cluster, namespace, cutoff, id string
		var legacyFiles []string
		var legacy bool
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs, Short: map[string]string{"init": "Initialize an explicitly cluster-bound ring", "rotate": "Replace the signer and retain old public keys", "revoke": "Withdraw trust in a retained signer"}[action]}
		cmd.Flags().StringVar(&path, "file", "", "Private ring file (required; 0600)")
		if action == "init" {
			cmd.Flags().StringVar(&cluster, "cluster-id", "", "Original kube-system namespace UID (required)")
			cmd.Flags().StringVar(&namespace, "namespace", "", "AuditLog release namespace (required)")
			cmd.Flags().StringSliceVar(&legacyFiles, "legacy-master-file", nil, "Explicit protected legacy master-key files (maximum 16)")
			cmd.Flags().StringVar(&cutoff, "legacy-before", "", "Past whole-second RFC3339 cutoff; legacy objects must predate it")
		}
		if action == "revoke" {
			cmd.Flags().StringVar(&id, "key-id", "", "Retained public-key fingerprint to revoke")
			cmd.Flags().BoolVar(&legacy, "legacy", false, "Withdraw trust in all imported legacy HMAC keys")
		}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return errors.New("audit keys: --file is required")
			}
			count, err := auditkeys.UpdateFile(cmd.Context(), path, action == "init", func(ring *auditkeys.Ring) (*auditkeys.Ring, error) {
				switch action {
				case "init":
					if len(legacyFiles) > 16 {
						return nil, auditkeys.ErrUnavailable
					}
					var before *time.Time
					if cutoff != "" {
						value, err := time.Parse(time.RFC3339, cutoff)
						if err != nil {
							return nil, errors.New("audit keys: invalid legacy cutoff")
						}
						before = &value
					}
					var imported [][]byte
					for _, file := range legacyFiles {
						data, err := auditkeys.ReadProtected(file)
						if err != nil {
							return nil, err
						}
						master, err := session.ParseKey(strings.TrimSpace(string(data)))
						if err != nil {
							return nil, errors.New("audit keys: invalid legacy master-key file")
						}
						key, err := auditlog.SealKey(master)
						if err != nil {
							return nil, auditkeys.ErrUnavailable
						}
						imported = append(imported, key)
					}
					return auditkeys.New(cluster, namespace, imported, before, time.Now())
				case "rotate":
					return ring.Rotate()
				case "revoke":
					if legacy == (id != "") {
						return nil, errors.New("audit keys: choose exactly one of --key-id or --legacy")
					}
					if legacy {
						return ring.RevokeLegacy()
					}
					return ring.Revoke(id)
				}
				return nil, auditkeys.ErrUnavailable
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Audit keys %s complete; retained public keys: %d\n", action, count)
			return err
		}
		root.AddCommand(cmd)
	}
	return root
}
