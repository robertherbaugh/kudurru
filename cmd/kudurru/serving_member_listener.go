package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const memberIdentityFile = "member-identity.json"

// prepareMemberListener mounts retained enrollment only. Activating a dormant
// database and creating its private identity remain explicit operator work;
// first activation requires restart to install the listener. Existing enrolled
// listeners observe membership changes for each new handshake, and every
// request separately retains the original Store membership through output.
func (s *servingRuntime) prepareMemberListener(ctx context.Context, credentials *internalapi.Credentials) (*internalapi.EnrolledListener, error) {
	listener, _, _, err := s.prepareMemberServices(ctx, credentials)
	return listener, err
}

// prepareMemberServices prepares the listener and outgoing worker without
// starting either. Root installs both before Run; preparing a listener alone
// never changes the workers of an already running runtime.
func (s *servingRuntime) prepareMemberServices(ctx context.Context, credentials *internalapi.Credentials) (*internalapi.EnrolledListener, *clusterreplica.Coordinator, *clusterreplica.EnrolledForwarder, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, owner, err := s.opts.Store.ReadMemberEnrollment(ctx)
	if errors.Is(err, sqlite.ErrMemberEnrollmentPending) {
		// No complete enrollment exists yet. Keep ordinary pending-follower
		// listeners operational without creating identity or member routes.
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if owner.State() == nil {
		// A separately installed read-only image can retain explicit dormant
		// enrollment. It supplies DNS data, never enrolled member routes.
		return nil, nil, nil, nil
	}
	if credentials == nil {
		return nil, nil, nil, errors.New("enrolled membership requires internal listener credentials")
	}
	identity, err := memberidentity.Load(filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile), a.MemberID)
	if err != nil {
		return nil, nil, nil, errors.New("enrolled membership requires the original local member identity")
	}
	if _, err := memberListenerPeers(owner, identity.Trust()); err != nil {
		return nil, nil, nil, err
	}
	server, err := clusterreplica.NewMemberServer(clusterreplica.MemberServerOptions{
		Store: s.opts.Store, Credentials: credentials, ExecuteScoped: s.executeMemberCommand,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	peers := func(ctx context.Context) ([]memberidentity.Trust, error) {
		current, owner, err := s.opts.Store.ReadMemberEnrollment(ctx)
		if err != nil {
			return nil, err
		}
		if current.MemberID != a.MemberID || current.Term.ClusterID != a.Term.ClusterID {
			return nil, memberidentity.ErrIdentity
		}
		return memberListenerPeers(owner, identity.Trust())
	}
	join, err := clusterreplica.NewJoinServer(s.opts.Store, identity, credentials)
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := internalapi.NewEnrollingListener(identity, peers, server.Handler(), join)
	if err != nil {
		return nil, nil, nil, err
	}
	source, err := clusterreplica.NewEnrolledSource(clusterreplica.EnrolledSourceOptions{
		Store: s.opts.Store, Identity: identity, Credentials: credentials,
		Validation: func(original context.Context) replication.Validation {
			// The immutable schema is reused, but each exchange owns its
			// validators' lifetime. Setup's five-second context is never retained.
			return s.schema.PrimaryImageOptions(original, a.Term.ClusterID).Validation
		},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	name := s.opts.Config.Node.Name
	if name == "" {
		for _, member := range owner.State().Members() {
			if member.ID == a.MemberID {
				name = member.Name
			}
		}
	}
	coordinator, err := clusterreplica.NewCoordinator(clusterreplica.CoordinatorOptions{
		Store: s.opts.Store, Source: source, NodeName: name,
		Served:       s.publisher.Pin,
		WithOperator: s.withPromotionAuthorization,
		Interval:     s.memberPullInterval,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	forward, err := clusterreplica.NewEnrolledForwarder(clusterreplica.EnrolledForwardOptions{Store: s.opts.Store, Identity: identity, Credentials: credentials})
	if err != nil {
		return nil, nil, nil, err
	}
	return listener, coordinator, forward, nil
}

func memberListenerPeers(owner accepted.Owner, local memberidentity.Trust) ([]memberidentity.Trust, error) {
	if owner.State() == nil || !local.ValidAt(time.Now()) {
		return nil, memberidentity.ErrIdentity
	}
	found := false
	var peers []memberidentity.Trust
	for _, member := range owner.State().Members() {
		if member.Revoked {
			continue
		}
		if member.ID == local.MemberID() {
			found = member.Trust.Fingerprint() == local.Fingerprint()
		} else if member.Trust.ValidAt(time.Now()) {
			peers = append(peers, member.Trust)
		}
	}
	if !found {
		return nil, memberidentity.ErrIdentity
	}
	return peers, nil
}
