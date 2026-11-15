package replica

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// PathJoin is reachable only through the distinct invitation TLS SNI.
const PathJoin = "/internal/replica/join"

// JoinReceipt confirms the primary's acceptance commit, not initial image
// installation, serving publication or convergence at the joining node.
type JoinReceipt struct {
	Committed   bool             `json:"committed"`
	MemberID    string           `json:"memberID"`
	Term        replication.Term `json:"term"`
	OwnerDigest string           `json:"ownerDigest"`
}

// JoinServer admits only bounded signed proposals for existing invitations. It
// retains the original complete source before reading a body. No HTTP operator
// principal is asserted: the original operator already issued this exact ticket.
type JoinServer struct {
	store       *sqlite.Store
	identity    *memberidentity.Identity
	credentials *internalapi.Credentials
	slots       chan struct{}
}

// NewJoinServer binds acceptance to the original loaded local identity and Store.
func NewJoinServer(s *sqlite.Store, identity *memberidentity.Identity, credentials *internalapi.Credentials) (*JoinServer, error) {
	if s == nil || identity == nil || credentials == nil || s.Authority().MemberID != identity.Trust().MemberID() {
		return nil, ErrProtocol
	}
	return &JoinServer{store: s, identity: identity, credentials: credentials, slots: make(chan struct{}, 4)}, nil
}

// ServeHTTP checks isolated TLS routing, shared credentials and invitation
// possession before committing membership and its audit in one transaction.
func (s *JoinServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != PathJoin || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 || r.TLS.ServerName != s.identity.Trust().EnrollmentServerName() {
		writeProblem(w, ErrUnauthorized)
		return
	}
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(r.Header.Values("Authorization")) != 1 || len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !s.credentials.ValidToken(parts[1]) {
		writeProblem(w, ErrUnauthorized)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeProblem(w, ErrUnavailable)
		return
	}
	deadline, _ := ctx.Deadline()
	c := http.NewResponseController(w)
	if c.SetReadDeadline(deadline) != nil || c.SetWriteDeadline(deadline) != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()); _ = c.SetWriteDeadline(time.Now()); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	image, token, err := s.store.ReadServingCandidate(ctx)
	if err != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	a := token.Authority()
	if a.Mode != replication.Primary || a.MemberID != s.identity.Trust().MemberID() {
		writeProblem(w, ErrUnauthorized)
		return
	}
	owner, err := accepted.DecodeRows(image.Payload.Membership, a.Term.ClusterID)
	if err != nil || owner.State() == nil {
		writeProblem(w, ErrUnauthorized)
		return
	}
	local := false
	for _, m := range owner.State().Members() {
		if m.ID == a.MemberID && !m.Revoked && m.Trust.Fingerprint() == s.identity.Trust().Fingerprint() {
			local = true
		}
	}
	if !local {
		writeProblem(w, ErrUnauthorized)
		return
	}
	writer, err := s.store.WriterToken()
	if err != nil || !writer.Term().Same(a.Term) || writer.Incarnation() != a.Incarnation {
		writeProblem(w, ErrUnavailable)
		return
	}
	ctx = sqlite.WithWriterToken(ctx, writer)
	authorize := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.identity.Trust().ValidAt(time.Now()) {
			return ErrUnauthorized
		}
		return nil
	}
	original, err := persistence.Capture(ctx, owner, authorize)
	if err != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	output, err := s.store.CaptureCommandOutput(ctx, token)
	if err != nil {
		writeProblem(w, ErrUnavailable)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" || r.ContentLength > enrollment.MaxProposalBytes {
		writeProblem(w, ErrProtocol)
		return
	}
	body, err := readBounded(http.MaxBytesReader(w, r.Body, enrollment.MaxProposalBytes), enrollment.MaxProposalBytes)
	if err != nil {
		writeProblem(w, ErrProtocol)
		return
	}
	proposal, err := enrollment.DecodeProposal(body)
	if err != nil {
		writeProblem(w, ErrProtocol)
		return
	}
	receipt := JoinReceipt{MemberID: proposal.Member().ID, Term: a.Term}
	started := false
	committed, err := s.store.CommandWithOutput(ctx, output, func(ctx context.Context, tx *sqlite.CommandTx) error {
		staged, err := persistence.Accept(ctx, tx, original, proposal)
		if err != nil {
			return err
		}
		receipt.OwnerDigest = staged.ProposedDigest
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "cluster.join", Actor: "member:" + receipt.MemberID, Authz: "invitation:key-possession", Key: store.Key{Kind: "Cluster", Name: a.Term.ClusterID}, Reason: "accept original one-time member invitation", RequestMethod: r.Method, RequestPath: r.URL.Path})
	}, func(scope context.Context) error {
		// Keep the invitation/member certificate bound through acknowledgment,
		// independently of the already confirmed transaction disposition.
		until := proposal.ExpiresAt()
		if expiry := proposal.Member().Trust.ExpiresAt(); expiry.Before(until) {
			until = expiry
		}
		if expiry := s.identity.Trust().ExpiresAt(); expiry.Before(until) {
			until = expiry
		}
		scope, cancel := context.WithDeadline(scope, until)
		defer cancel()
		deadline, _ := scope.Deadline()
		if err := c.SetWriteDeadline(deadline); err != nil {
			return err
		}
		if err := authorize(); err != nil {
			return err
		}
		if err := output.CheckOutput(scope); err != nil {
			return err
		}
		receipt.Committed = true
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		started = true
		w.WriteHeader(http.StatusOK)
		if _, err = w.Write(raw); err != nil {
			return err
		}
		if err = c.Flush(); err != nil {
			return err
		}
		return output.CheckOutput(scope)
	})
	if err == nil {
		return
	}
	if started {
		panic(http.ErrAbortHandler)
	}
	if committed || errors.Is(err, sqlite.ErrCommandCompletionUnknown) {
		writeProblem(w, ErrOutcomeUnknown)
		return
	}
	if errors.Is(err, enrollment.ErrRefused) || errors.Is(err, replication.ErrCAS) {
		writeProblem(w, ErrUnauthorized)
		return
	}
	writeProblem(w, ErrUnavailable)
}
