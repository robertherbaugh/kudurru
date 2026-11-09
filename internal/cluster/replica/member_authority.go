package replica

import (
	"context"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func (s *MemberServer) authority(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || len(r.Header.Values(HeaderTerm)) != 1 {
		writeProblem(w, ErrProtocol)
		return
	}
	term, err := decodeTerm(r.Header.Get(HeaderTerm))
	if err != nil {
		writeProblem(w, ErrProtocol)
		return
	}
	started := false
	err = s.opts.Store.ObserveMemberProbe(r.Context(), r.Header.Get(HeaderCluster), r.Header.Get(HeaderMember), term, r.TLS,
		func(ctx context.Context, member string, current replication.Term, check func() error) error {
			controller := http.NewResponseController(w)
			deadline, _ := ctx.Deadline()
			if err := controller.SetWriteDeadline(deadline); err != nil {
				return err
			}
			done := make(chan struct{})
			stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()); close(done) })
			defer func() {
				if !stop() {
					<-done
				}
			}()
			if err := check(); err != nil {
				return err
			}
			w.Header().Set(HeaderMember, member)
			w.Header().Set(HeaderCluster, current.ClusterID)
			w.Header().Set(HeaderTerm, encodeTerm(current))
			started = true
			w.WriteHeader(http.StatusNoContent)
			if err := controller.Flush(); err != nil {
				return err
			}
			return check()
		})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		writeProblem(w, ErrUnavailable)
	}
}

// probeAuthority exchanges only an original primary term and authenticated peer
// metadata. It neither reads a replica image nor contributes promotion liveness.
func (c *Client) probeAuthority(ctx context.Context, peer Peer, term replication.Term) error {
	if c == nil || c.member == nil || term.Validate() != nil {
		return ErrProtocol
	}
	identity, err := c.captureIdentity(ctx)
	if err != nil {
		return err
	}
	if term.PrimaryID != identity.self.MemberID || term.ClusterID != identity.self.ClusterID {
		return ErrProtocol
	}
	identity.announcement = &term
	identity.tokens = identity.tokens[:1] // one original exchange, no credential retry
	response, err := c.requestCaptured(ctx, identity, peer, http.MethodGet, PathAuthority, nil, "")
	if err != nil {
		return err
	}
	defer closeResponse(response)
	if _, err := c.observe(ctx, peer, response); err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		return ErrProtocol
	}
	return c.checkResponse(ctx, peer, response)
}

// ObserveNextPeer starts a fresh bounded metadata observation for one enrolled
// peer. Only a current primary sends announcements; ordinary follower pulls
// retain their existing liveness/installation path. A response can fence before
// this method returns an error. It is never a commit/publication receipt.
func (s *EnrolledSource) ObserveNextPeer(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	a, owner, err := s.opts.Store.ReadMemberEnrollment(ctx)
	if err != nil {
		return err
	}
	if a.Mode != replication.Primary || !a.Writable() || owner.State() == nil {
		return nil
	}
	var peers []string
	for _, member := range owner.State().Members() {
		if member.ID != a.MemberID && !member.Revoked && member.Trust.ValidAt(time.Now()) {
			peers = append(peers, member.ID)
		}
	}
	if len(peers) == 0 {
		return nil
	}
	// The Store captures actual peer trust/address before the exchange. This
	// scheduling roster grants nothing and cannot refresh an admitted attempt.
	peerID := peers[(s.probeCursor.Add(1)-1)%uint64(len(peers))]
	return s.opts.Store.WithMemberProbe(ctx, s.opts.Identity.Trust(), peerID, func(scope context.Context, original *sqlite.MemberAcquisition) error {
		member, term := original.Peer(), original.Term()
		peer := Peer{MemberID: member.ID, ClusterID: term.ClusterID, Address: member.Address, Trust: member.Trust}
		self := Peer{MemberID: original.MemberID(), ClusterID: term.ClusterID}
		client, err := NewMemberClient(ClientOptions{Credentials: s.opts.Credentials, Observer: original,
			Self: func() Peer { return self }, Timeout: 2 * time.Second}, s.opts.Identity)
		if err != nil {
			return err
		}
		return client.probeAuthority(scope, peer, term)
	})
}
