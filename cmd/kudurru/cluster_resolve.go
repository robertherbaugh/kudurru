package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/spf13/cobra"
)

// clusterResolutionReview is the host-local observation an operator saves and
// passes back unchanged. It identifies the branch; it grants nothing.
type clusterResolutionReview struct {
	MemberID   string                  `json:"memberID"`
	Node       string                  `json:"node"`
	Mode       string                  `json:"mode"`
	Term       replication.Term        `json:"term"`
	Installed  replication.Generation  `json:"installed"`
	Conflict   *replication.Term       `json:"conflict,omitempty"`
	Divergence *replication.Generation `json:"divergence,omitempty"`
	Resolvable bool                    `json:"resolvable"`
	Reason     string                  `json:"reason"`
}

// clusterResolutionReceipt confirms only the local durable transition. Other
// members adopt the new term through later authenticated peer exchanges.
type clusterResolutionReceipt struct {
	Committed bool                   `json:"committed"`
	MemberID  string                 `json:"memberID"`
	Node      string                 `json:"node"`
	RequestID string                 `json:"requestID"`
	Term      replication.Term       `json:"term"`
	Kept      replication.Generation `json:"kept"`
}

type clusterResolveOptions struct {
	directory, review, node, requestID, reason string
	offline                                    bool
	abandonTerm                                uint64
}

// resolutionFencedBehindTerm is the review reason of a fenced member that
// holds no image of its current term and no conflict or divergence. Keeping
// its branch abandons that term, so cluster resolve also needs the exact
// --abandon-term epoch (D369).
const resolutionFencedBehindTerm = "fenced_behind_term"

func addClusterResolutionCommands(root *cobra.Command) {
	var review clusterResolveOptions
	reviewCmd := &cobra.Command{Use: "resolution", Short: "Review a stopped member's promotion conflict or divergence", Args: cobra.NoArgs,
		Long: `Read the durable conflict, divergence or fenced state of a stopped enrolled SQLite member.
Stop the local kudurru process first; the normal exclusive database lock refuses a
running server. Writers on a conflicted member stay refused and its HTTP API is closed
while DNS continues from its last valid local data. Save this JSON output and pass it
to cluster resolve on the member whose branch should survive. A fenced member that
holds no image of its current term reads "reason":"fenced_behind_term"; it can keep its
branch only by abandoning that term (cluster resolve --abandon-term). Nothing is modified.`,
		RunE: func(c *cobra.Command, _ []string) error {
			if !review.offline || review.directory == "" {
				return errors.New("--offline and --data-dir are required")
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()
			s, node, err := openOfflineMember(ctx, review.directory)
			if err != nil {
				return err
			}
			out, err := offlineResolutionReview(ctx, s, node)
			err = errors.Join(err, s.Close())
			if err != nil {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(out)
		}}
	reviewCmd.Flags().StringVar(&review.directory, "data-dir", "", "Existing stopped member data directory")
	reviewCmd.Flags().BoolVar(&review.offline, "offline", false, "Acknowledge that all local kudurru processes are stopped")
	root.AddCommand(reviewCmd)

	var o clusterResolveOptions
	cmd := &cobra.Command{Use: "resolve", Short: "Keep a stopped member's reviewed branch after a promotion conflict", Args: cobra.NoArgs,
		Long: `Keep this member's branch after a durable equal-epoch promotion conflict or
divergence. Run cluster resolution --offline on the member whose data should survive,
save its JSON, and pass it with --review, the exact --confirm-node, a unique
--request-id and an audit --reason. To keep another member's branch, run this on that
member instead. Stop the local kudurru process first.

One 30-second attempt rechecks the exact reviewed term, installed generation and
conflict/divergence, allocates a new epoch above every observed term, retains the
prior rows as private quarantine and commits a host-attributed audit event in the same
transaction. Start the member afterwards: other members adopt the new term only through
later scheduled authenticated peer exchanges, quarantine their own branch when they
install this one and never replay its writes. Never replay this command after an
unknown result; review current state first.

A review with "reason":"fenced_behind_term" is a member fenced by a newer term whose
image it never installed. Keeping its branch abandons that term: pass --abandon-term
with that term's epoch, and only when its primary and every member that followed it
are lost for good. Their writes are not replayed, and a member of that term that
returns adopts this branch.`,
		RunE: func(c *cobra.Command, _ []string) error {
			out, err := runClusterResolve(c.Context(), o)
			if out != nil {
				err = errors.Join(err, json.NewEncoder(c.OutOrStdout()).Encode(out))
			}
			return err
		}}
	cmd.Flags().StringVar(&o.directory, "data-dir", "", "Existing stopped member data directory")
	cmd.Flags().StringVar(&o.review, "review", "", "Local file containing the saved cluster resolution JSON")
	cmd.Flags().StringVar(&o.node, "confirm-node", "", "Exact reviewed local member name")
	cmd.Flags().StringVar(&o.requestID, "request-id", "", "Unique identifier for this operator decision")
	cmd.Flags().StringVar(&o.reason, "reason", "", "Audit reason for keeping this branch")
	cmd.Flags().BoolVar(&o.offline, "offline", false, "Acknowledge that all local kudurru processes are stopped")
	cmd.Flags().Uint64Var(&o.abandonTerm, "abandon-term", 0, "Epoch of the lost term to abandon (only for a fenced_behind_term review)")
	root.AddCommand(cmd)
}

func offlineStoreOptions(identity string) (sqlite.Options, error) {
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		return sqlite.Options{}, err
	}
	v, err := validator.New()
	if err != nil {
		return sqlite.Options{}, err
	}
	return sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: privateStoreKinds(), Identity: identity,
		ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage}, nil
}

// openOfflineMember opens an existing enrolled member under the exclusive
// process lock, verifies its retained private identity and reopens with the
// member's own roster name so a new term carries the correct primary name.
func openOfflineMember(ctx context.Context, directory string) (*sqlite.Store, string, error) {
	path := filepath.Join(directory, sqlite.FileName)
	opts, err := offlineStoreOptions("")
	if err != nil {
		return nil, "", err
	}
	s, err := sqlite.OpenExisting(ctx, path, opts)
	if err != nil {
		return nil, "", err
	}
	a, owner, err := s.ReadMemberEnrollment(ctx)
	node := ""
	if err == nil && owner.State() != nil {
		for _, m := range owner.State().Members() {
			if m.ID == a.MemberID && !m.Revoked {
				node = m.Name
			}
		}
	}
	if err == nil && node == "" {
		err = errors.New("data directory is not an active enrolled member")
	}
	if err == nil {
		_, err = memberidentity.Load(filepath.Join(directory, memberIdentityFile), a.MemberID)
	}
	if err = errors.Join(err, s.Close()); err != nil {
		return nil, "", err
	}
	if opts, err = offlineStoreOptions(node); err != nil {
		return nil, "", err
	}
	s, err = sqlite.OpenExisting(ctx, path, opts)
	if err != nil {
		return nil, "", err
	}
	if s.Authority().MemberID != a.MemberID {
		return nil, "", errors.Join(replication.ErrCAS, s.Close())
	}
	return s, node, nil
}

func offlineResolutionReview(ctx context.Context, s *sqlite.Store, node string) (clusterResolutionReview, error) {
	a := s.Authority()
	out := clusterResolutionReview{MemberID: a.MemberID, Node: node, Mode: string(a.Mode), Term: a.Term, Conflict: a.Conflict, Divergence: a.Divergence}
	image, err := s.ReadGeneration(ctx)
	if err == nil {
		out.Installed = image.Generation()
	}
	switch {
	case ctx.Err() != nil:
		return out, ctx.Err()
	case a.Conflict == nil && a.Divergence == nil && a.Mode == replication.Fenced && err == nil &&
		out.Installed.Term.ClusterID == a.Term.ClusterID && out.Installed.Term.Epoch < a.Term.Epoch:
		out.Resolvable, out.Reason = true, resolutionFencedBehindTerm
	case a.Conflict == nil && a.Divergence == nil:
		out.Reason = "no_conflict"
	case a.Mode != replication.Conflict && a.Mode != replication.Fenced:
		out.Reason = "not_fenced"
	case err != nil:
		out.Reason = "no_installed_image"
	default:
		out.Resolvable, out.Reason = true, "resolvable"
	}
	return out, nil
}

func sameResolutionReview(a, b clusterResolutionReview) bool {
	sameTerm := func(x, y *replication.Term) bool { return x == nil && y == nil || x != nil && y != nil && x.Same(*y) }
	sameGeneration := func(x, y *replication.Generation) bool {
		return x == nil && y == nil || x != nil && y != nil && x.Same(*y)
	}
	return a.MemberID == b.MemberID && a.Node == b.Node && a.Mode == b.Mode && a.Term.Same(b.Term) && a.Installed.Same(b.Installed) &&
		sameTerm(a.Conflict, b.Conflict) && sameGeneration(a.Divergence, b.Divergence) && a.Resolvable == b.Resolvable && a.Reason == b.Reason
}

func runClusterResolve(parent context.Context, o clusterResolveOptions) (*clusterResolutionReceipt, error) {
	if !o.offline || o.directory == "" || o.review == "" || o.node == "" || len(o.node) > 63 ||
		strings.TrimSpace(o.requestID) == "" || len(o.requestID) > 256 || strings.TrimSpace(o.reason) == "" || len(o.reason) > 512 {
		return nil, errors.New("--offline, --data-dir, --review, --confirm-node, --request-id and --reason are required")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	f, err := os.Open(o.review) //nolint:gosec // explicit operator-selected local review file
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err := errors.Join(readErr, f.Close()); err != nil || len(raw) > 64<<10 {
		return nil, errors.New("invalid or oversized resolution review")
	}
	var reviewed clusterResolutionReview
	if err := decodeReviewedCommandJSON(raw, &reviewed, "invalid resolution review JSON"); err != nil {
		return nil, errors.New("invalid resolution review JSON")
	}
	if id, err := uuid.Parse(reviewed.MemberID); err != nil || id == uuid.Nil || !reviewed.Resolvable || reviewed.Node != o.node ||
		reviewed.Term.Validate() != nil || reviewed.Installed.Validate() != nil {
		return nil, errors.New("review is not a resolvable observation of the confirmed node")
	}
	var abandon *replication.Term
	switch {
	case reviewed.Reason == resolutionFencedBehindTerm && o.abandonTerm != reviewed.Term.Epoch:
		return nil, errors.New("this member is fenced by a newer term it never installed; keeping its branch abandons that term: pass --abandon-term with the reviewed term epoch, and only if that term's primary and every member that followed it are lost for good")
	case reviewed.Reason == resolutionFencedBehindTerm:
		term := reviewed.Term
		abandon = &term
	case o.abandonTerm != 0:
		return nil, errors.New("--abandon-term applies only to a fenced_behind_term review")
	}
	s, node, err := openOfflineMember(ctx, o.directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	current, err := offlineResolutionReview(ctx, s, node)
	if err != nil {
		return nil, err
	}
	if !sameResolutionReview(current, reviewed) || node != o.node {
		return nil, errors.New("member state changed since the review; review current state before a new decision")
	}
	if _, err := s.BindAnchorLifecycle(ctx); err != nil {
		return nil, err
	}
	// Capture the actual anchor/authority cut before reading the kept image.
	captured, err := s.CaptureAnchorInstall(ctx)
	if err != nil {
		return nil, err
	}
	image, err := s.ReadGeneration(captured)
	if err != nil {
		return nil, err
	}
	if !image.Generation().Same(reviewed.Installed) {
		return nil, errors.New("installed generation changed since the review")
	}
	chosen, err := s.ValidateImage(captured, *image)
	if err != nil {
		return nil, err
	}
	authz := "host-local data directory and member identity"
	if abandon != nil {
		authz += "; abandons unreachable term " + strconv.FormatUint(abandon.Epoch, 10)
	}
	result, err := s.ResolveAudited(captured, chosen, replication.ResolveRequest{ExpectedTerm: reviewed.Term, ExpectedInstalled: reviewed.Installed,
		ExpectedConflict: reviewed.Conflict, ExpectedDivergence: reviewed.Divergence, AbandonTerm: abandon, RequestID: o.requestID},
		func(ctx context.Context) error { return ctx.Err() },
		sqlite.AuditEntry{Actor: "host:offline-cli", Authz: authz, Reason: o.reason})
	if errors.Is(err, sqlite.ErrResolutionCompletionUnknown) {
		return nil, errors.New("resolution completion is unknown; review current state before a new decision; do not replay this command")
	}
	if err != nil {
		return nil, err
	}
	if result.Parent == nil {
		return nil, replication.ErrInvalid
	}
	return &clusterResolutionReceipt{Committed: true, MemberID: result.MemberID, Node: node, RequestID: o.requestID, Term: result.Term, Kept: *result.Parent}, s.Close()
}
