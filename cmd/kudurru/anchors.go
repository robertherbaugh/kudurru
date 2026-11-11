package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

type anchorOfflineOptions struct {
	database, expected, reason string
	ds, servers                []string
	offline                    bool
	timeout                    time.Duration
}

type anchorOfflineResult struct {
	Version     accepted.Version `json:"version"`
	Quarantined bool             `json:"quarantined"`
	Recoveries  int              `json:"recoveries"`
	Outcome     string           `json:"outcome"`
}

func newAnchorsCmd() *cobra.Command {
	var opts anchorOfflineOptions
	cmd := &cobra.Command{
		Use: "anchors", Short: "Inspect or recover offline SQLite managed root trust",
		Long: `Operate directly on an existing current-schema SQLite database using local
OS file permissions. Stop kudurru and all other readers/writers first, preserve a
backup, and keep managed edges uncached. No API credentials or network discovery
are used. This command cannot initialize, migrate or repair a database.

Recovery is available only after canonical loss of every trusted root key. Obtain
replacement root DS records through an independent trusted channel. The command
authenticates one fresh DNSKEY response, preserves all revoked keys and records
explicit recovery history plus an audit event in the same transaction. Do not
replay an uncertain result: inspect the retained version first. Before distributing
recovered history, check that every member runs kudurru v0.2.0 or later; older
releases refuse v0.2.0 images by store schema.`,
	}
	cmd.PersistentFlags().StringVar(&opts.database, "database", "", "existing state.db path (required)")
	cmd.PersistentFlags().BoolVar(&opts.offline, "offline", false, "acknowledge that all database readers and writers are stopped")
	cmd.PersistentFlags().DurationVar(&opts.timeout, "timeout", 30*time.Second, "original operation deadline (positive, at most 30s)")
	for _, operation := range []string{"status", "recover"} {
		sub := &cobra.Command{
			Use: operation, Args: cobra.NoArgs,
			Short: map[string]string{"status": "Read the exact retained anchor version", "recover": "Accept explicit replacement trust after complete anchor loss"}[operation],
			RunE: func(c *cobra.Command, _ []string) error {
				result, err := runAnchorOffline(c.Context(), opts, operation == "recover")
				if result != nil {
					err = errors.Join(err, json.NewEncoder(c.OutOrStdout()).Encode(result))
				}
				return err
			},
		}
		if operation == "recover" {
			sub.Flags().StringVar(&opts.expected, "expected-digest", "", "exact accepted-envelope digest from status (required)")
			sub.Flags().StringVar(&opts.reason, "reason", "", "audited recovery reason (required, at most 512 bytes)")
			sub.Flags().StringArrayVar(&opts.ds, "ds", nil, "independently verified replacement root DS record; repeat for each record")
			sub.Flags().StringArrayVar(&opts.servers, "root-server", nil, "numeric root DNS IP:port; TCP, at most three selected endpoints")
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

func runAnchorOffline(parent context.Context, opts anchorOfflineOptions, recoverTrust bool) (result *anchorOfflineResult, err error) {
	if !opts.offline || opts.database == "" || opts.timeout <= 0 || opts.timeout > 30*time.Second {
		return nil, errors.New("--offline, --database and a positive --timeout of at most 30s are required")
	}
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()
	var endpoints []netip.AddrPort
	if recoverTrust {
		digest, e := hex.DecodeString(opts.expected)
		if e != nil || len(digest) != 32 || strings.ToLower(opts.expected) != opts.expected || !anchorRecoveryReason(opts.reason) || len(opts.ds) == 0 || len(opts.ds) > trustanchors.MaxLiveKeys || len(opts.servers) == 0 || len(opts.servers) > anchorEndpointLimit {
			return nil, errors.New("recovery requires an exact digest, bounded reason, explicit DS records and numeric servers")
		}
		total := 0
		for _, record := range opts.ds {
			total += len(record)
			if total > 64<<10 {
				return nil, errors.New("replacement DS records exceed 64 KiB")
			}
		}
		for _, server := range opts.servers {
			endpoint, err := netip.ParseAddrPort(server)
			if err != nil {
				return nil, errors.New("--root-server must be a numeric IP:port")
			}
			endpoints = append(endpoints, endpoint)
		}
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		return nil, err
	}
	s, err := sqlite.OpenExisting(ctx, opts.database, sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	image, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		return nil, err
	}
	result = &anchorOfflineResult{Version: owner.Version(), Outcome: "read-only"}
	if owner.Inspect().Phase == accepted.Managed {
		state, err := owner.State()
		if err != nil {
			return result, err
		}
		result.Quarantined, result.Recoveries = state.Inspect().Quarantined, len(state.Inspect().Recoveries)
	}
	if !recoverTrust {
		return result, nil
	}
	result.Outcome = "not-attempted"
	if owner.Version().Digest != opts.expected || !result.Quarantined {
		return result, errors.New("recovery requires the exact quarantined accepted owner")
	}
	writer, err := s.WriterToken()
	if err != nil {
		return result, err
	}
	ctx = sqlite.WithWriterToken(ctx, writer)
	if _, err := s.BindAnchorLifecycle(ctx); err != nil {
		return result, err
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		return result, err
	}
	original, err := persistence.CaptureTransition(ctx, owner.Version(), clock)
	if err != nil {
		return result, err
	}
	update, err := s.CaptureAnchorUpdate(ctx, owner.Version())
	if err != nil {
		return result, err
	}
	wire, err := captureAnchorWirePlan(ctx, endpoints, "tcp", 0)
	if err != nil {
		return result, err
	}
	if err := s.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error { return nil }); err != nil {
		return result, err
	}
	raw, received, err := acquireAnchorWire(wire, func() (trustanchors.Sample, error) { return clock.Sample(), nil })
	if err != nil {
		return result, err
	}
	message, err := unpackAnchorBootstrap(raw)
	if err != nil {
		return result, err
	}
	before, err := owner.State()
	if err != nil {
		return result, err
	}
	proposed, proof, err := trustanchors.Recover(before, opts.ds, message, received)
	if err != nil {
		return result, err
	}
	// Check the full original cut in memory while AnchorCommand holds wmu.
	// Our own transaction has not published its successor at the final check.
	check := func(context.Context) error {
		if !token.Matches(s.Authority()) {
			return replication.ErrCAS
		}
		return nil
	}
	completion, commandErr := s.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := check(ctx); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		staged, err := persistence.Recover(ctx, tx, original, proposed, proof)
		if err != nil {
			return err
		}
		prior, err := json.Marshal(owner.Version())
		if err != nil {
			return err
		}
		after, err := json.Marshal(staged.ProposedVersion)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "trustanchors.recover", Actor: fmt.Sprintf("os:uid:%d", os.Geteuid()), Manager: "cli:anchors", Authz: "local-os", Reason: opts.reason, Before: prior, After: after})
	})
	result.Outcome = map[sqlite.AnchorOutcome]string{sqlite.AnchorUnknown: "unknown", sqlite.AnchorRolledBack: "rolled-back", sqlite.AnchorUnchanged: "unchanged", sqlite.AnchorCommitted: "committed"}[completion.Outcome()]
	if version, ok := completion.Version(); ok {
		result.Version = version
		if completion.Outcome() == sqlite.AnchorCommitted {
			result.Quarantined, result.Recoveries = false, len(proposed.Inspect().Recoveries)
		}
	}
	return result, commandErr
}

func anchorRecoveryReason(reason string) bool {
	if reason == "" || len(reason) > 512 || strings.TrimSpace(reason) != reason || !utf8.ValidString(reason) {
		return false
	}
	for _, r := range reason {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
