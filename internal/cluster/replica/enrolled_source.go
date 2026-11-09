package replica

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// EnrolledSourceOptions retains an existing local identity and complete local
// schema. Validation must build only pure local validators for the supplied
// original context; it cannot read Store or acquire another owner's locks.
type EnrolledSourceOptions struct {
	Store       *sqlite.Store
	Identity    *memberidentity.Identity
	Credentials *internalapi.Credentials
	Validation  func(context.Context) replication.Validation
	Timeout     time.Duration
}

// EnrolledSource acquires from the actual installed primary membership. No
// caller-supplied peer resolver, replacement trust or transport is accepted.
// It neither initializes enrollment nor installs images itself.
type EnrolledSource struct{ opts EnrolledSourceOptions }

// NewEnrolledSource binds acquisition to the retained local identity and Store.
func NewEnrolledSource(o EnrolledSourceOptions) (*EnrolledSource, error) {
	if o.Store == nil || o.Identity == nil || o.Credentials == nil || o.Validation == nil || o.Timeout < 0 || o.Timeout > 30*time.Second {
		return nil, ErrProtocol
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	return &EnrolledSource{opts: o}, nil
}

// Fetch retains one original Store membership scope through the actual TLS
// exchange and body decode. Authenticated higher terms fence before body decode
// and refuse the old acquisition; a later poll must start independently.
func (s *EnrolledSource) Fetch(ctx context.Context, etag string) (follower.Response, error) {
	if s == nil || ctx == nil {
		return follower.Response{}, ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	var result follower.Response
	err := s.opts.Store.WithMemberAcquisition(ctx, s.opts.Identity.Trust(), func(ctx context.Context, original *sqlite.MemberAcquisition) error {
		member, term := original.Peer(), original.Term()
		peer := Peer{MemberID: member.ID, ClusterID: term.ClusterID, Address: member.Address, Trust: member.Trust}
		self := Peer{MemberID: original.MemberID(), ClusterID: term.ClusterID}
		client, err := NewMemberClient(ClientOptions{Credentials: s.opts.Credentials, Observer: original,
			Self: func() Peer { return self }, Timeout: s.opts.Timeout}, s.opts.Identity)
		if err != nil {
			return err
		}
		source, err := NewSource(client, SourceOptions{Resolve: func(context.Context) (Peer, error) { return peer, nil }, Validation: s.opts.Validation(ctx)})
		if err != nil {
			return err
		}
		result, err = source.Fetch(ctx, etag)
		return err
	})
	if err != nil {
		return follower.Response{}, err
	}
	return result, nil
}
