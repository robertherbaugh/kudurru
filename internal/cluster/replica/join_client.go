package replica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// JoinResult keeps primary acceptance independent from local image installation.
// Neither receipt establishes serving publication or peer convergence.
type JoinResult struct {
	Primary   JoinReceipt            `json:"primary"`
	Installed replication.Generation `json:"installed"`
}

// JoinReceiver uses one original finite attempt, one proposal dispatch and one
// authenticated first-image fetch. The caller creates/loads the exact local
// private identity explicitly, binds the pristine Store's anchor lifecycle and
// supplies the normal complete local schema. An error never triggers replay,
// discovery, replacement trust, identity repair or a new local capture.
func JoinReceiver(ctx context.Context, s *sqlite.Store, identity *memberidentity.Identity, invitation enrollment.Invitation, credentials *internalapi.Credentials, validation replication.Validation) (JoinResult, error) {
	var result JoinResult
	if ctx == nil || s == nil || identity == nil || credentials == nil {
		return result, ErrProtocol
	}
	deadline, finite := ctx.Deadline()
	if !finite || !time.Now().Before(deadline) || time.Until(deadline) > 30*time.Second {
		return result, ErrProtocol
	}
	ctx, cancel := context.WithDeadline(ctx, invitation.ExpiresAt())
	defer cancel()
	original, err := s.CaptureInitialJoin(ctx, invitation, identity.Trust())
	if err != nil {
		return result, err
	}
	result.Primary, err = acceptInvitation(ctx, identity, invitation, credentials)
	if err != nil {
		return result, err
	}
	issuer, address, trust := invitation.IssuingPeer()
	term := result.Primary.Term
	self := Peer{MemberID: identity.Trust().MemberID(), ClusterID: invitation.ClusterID()}
	peer := Peer{MemberID: issuer.MemberID, ClusterID: self.ClusterID, Address: address, Trust: trust}
	client, err := NewMemberClient(ClientOptions{Credentials: credentials, Self: func() Peer { return self }, Observer: joiningTerm{term: term}}, identity)
	if err != nil {
		return result, err
	}
	source, err := NewSource(client, SourceOptions{Resolve: func(context.Context) (Peer, error) { return peer, nil }, Validation: validation})
	if err != nil {
		return result, err
	}
	response, err := source.Fetch(ctx, "")
	if err != nil {
		return result, err
	}
	if response.Image == nil || response.NotModified {
		return result, ErrProtocol
	}
	validated, err := s.ValidateImage(ctx, *response.Image)
	if err != nil {
		return result, err
	}
	result.Installed, err = s.InstallJoinedReplica(original, validated)
	return result, err
}

// A never-enrolled receiver cannot observe/commit a remote term before the
// first image. This observer only refuses any change to the confirmed original
// primary; the Store atomically installs that term with the complete image.
type joiningTerm struct{ term replication.Term }

func (o joiningTerm) ObserveAuthority(ctx context.Context, term replication.Term) (replication.Authority, error) {
	if err := ctx.Err(); err != nil {
		return replication.Authority{}, err
	}
	if !o.term.Same(term) {
		return replication.Authority{}, replication.ErrCAS
	}
	return replication.Authority{}, nil
}

func acceptInvitation(ctx context.Context, identity *memberidentity.Identity, invitation enrollment.Invitation, credentials *internalapi.Credentials) (JoinReceipt, error) {
	var out JoinReceipt
	if err := invitation.CheckLifetime(identity.Trust()); err != nil {
		return out, err
	}
	proposal, err := invitation.Propose(identity)
	if err != nil {
		return out, err
	}
	body, err := proposal.Encode()
	if err != nil {
		return out, err
	}
	issuer, address, trust := invitation.IssuingPeer()
	config, err := trust.EnrollmentClientTLS()
	if err != nil {
		return out, err
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: config, DisableKeepAlives: true, TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	u := url.URL{Scheme: "https", Host: address, Path: PathJoin}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return out, ErrProtocol
	}
	r.GetBody = nil
	r.Header.Set("Authorization", "Bearer "+credentials.Token())
	r.Header.Set("Content-Type", "application/json")
	response, err := client.Do(r)
	// Once dispatched, inability to verify an explicit refusal or complete
	// receipt is unknown. Do not infer rollback from a transport failure.
	if err != nil {
		return out, errors.Join(ErrOutcomeUnknown, err)
	}
	defer func() { _ = response.Body.Close() }()
	if trust.VerifyPeer(response.TLS) != nil {
		return out, errors.Join(ErrOutcomeUnknown, ErrUntrusted)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return out, ErrUnauthorized
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" {
		return out, ErrOutcomeUnknown
	}
	raw, err := readBounded(response.Body, 4096)
	if err != nil {
		return out, errors.Join(ErrOutcomeUnknown, err)
	}
	if json.Unmarshal(raw, &out) != nil || !out.Committed || out.MemberID != identity.Trust().MemberID() || out.Term.Validate() != nil || out.Term.ClusterID != invitation.ClusterID() || out.Term.PrimaryID != issuer.MemberID || out.Term.Epoch != issuer.Epoch || out.Term.PromotionID != issuer.PromotionID || len(out.OwnerDigest) != 64 {
		return JoinReceipt{}, ErrOutcomeUnknown
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, invitation.CheckLifetime(identity.Trust())
}
