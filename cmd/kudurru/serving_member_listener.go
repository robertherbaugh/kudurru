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
)

const memberIdentityFile = "member-identity.json"

// prepareMemberListener mounts retained enrollment only. Activating a dormant
// database and creating its private identity remain explicit operator work;
// first activation requires restart to install the listener. Existing enrolled
// listeners observe membership changes for each new handshake, and every
// request separately retains the original Store membership through output.
func (s *servingRuntime) prepareMemberListener(ctx context.Context, credentials *internalapi.Credentials) (*internalapi.EnrolledListener, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, owner, err := s.opts.Store.ReadMemberEnrollment(ctx)
	if err != nil {
		return nil, err
	}
	if owner.State() == nil {
		// A separately installed read-only image can retain explicit dormant
		// enrollment. It supplies DNS data, never enrolled member routes.
		return nil, nil
	}
	if credentials == nil {
		return nil, errors.New("enrolled membership requires internal listener credentials")
	}
	identity, err := memberidentity.Load(filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile), a.MemberID)
	if err != nil {
		return nil, errors.New("enrolled membership requires the original local member identity")
	}
	if _, err := memberListenerPeers(owner, identity.Trust()); err != nil {
		return nil, err
	}
	server, err := clusterreplica.NewMemberServer(clusterreplica.MemberServerOptions{
		Store: s.opts.Store, Credentials: credentials, ExecuteScoped: s.executeMemberCommand,
	})
	if err != nil {
		return nil, err
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
	return internalapi.NewEnrolledListener(identity, peers, server.Handler())
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
