package replica

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// EnrolledForwardOptions binds outgoing commands to retained local identity.
// No caller-selected peer resolver or replacement transport is accepted.
type EnrolledForwardOptions struct {
	Store       *sqlite.Store
	Identity    *memberidentity.Identity
	Credentials *internalapi.Credentials
}

// EnrolledForwarder sends one immutable typed command to its original enrolled
// primary. Root owns user admission, dependency exclusions and public emission.
type EnrolledForwarder struct{ opts EnrolledForwardOptions }

// NewEnrolledForwarder retains the already loaded original private identity.
func NewEnrolledForwarder(o EnrolledForwardOptions) (*EnrolledForwarder, error) {
	if o.Store == nil || o.Identity == nil || o.Credentials == nil {
		return nil, ErrProtocol
	}
	return &EnrolledForwarder{opts: o}, nil
}

// Capture binds the original public source and lifetime before body acquisition.
func (f *EnrolledForwarder) Capture(ctx context.Context, token sqlite.GenerationToken) (*sqlite.MemberForwardRequest, error) {
	if f == nil {
		return nil, ErrProtocol
	}
	return f.opts.Store.CaptureMemberForward(ctx, token, f.opts.Identity.Trust())
}

// Forward never retries, refreshes the source/term, or installs an image. emit
// runs only after an actual authenticated, completely decoded response, while
// the Store still excludes local source and membership replacement. It must
// synchronously write and flush under ctx, join cancellation callbacks, and obey
// Store.WithMemberForward's no-reentry contract. A caller-claimed outcome cannot
// enter this callback. received is independent of later emission/cancellation
// errors; it is transport evidence, not a local durable/publication receipt.
func (f *EnrolledForwarder) Forward(ctx context.Context, request *sqlite.MemberForwardRequest, command Command, emit func(context.Context, Outcome) error) (received bool, result error) {
	if f == nil || ctx == nil || request == nil || emit == nil {
		return false, ErrProtocol
	}
	if IsForwarded(ctx) || sqlite.HasWriterToken(ctx) {
		return false, ErrLoop
	}
	if !command.ExpectedTerm.Same(request.OriginalTerm()) {
		return false, replication.ErrCAS
	}
	if command.NotAfter.After(request.Deadline()) {
		return false, ErrProtocol
	}
	if err := command.validate(time.Now(), DefaultCommandLifetime); err != nil {
		return false, err
	}
	ctx, cancel := context.WithDeadline(ctx, command.NotAfter)
	defer cancel()
	dispatched := false
	defer func() {
		if result != nil && !dispatched {
			result = errors.Join(ErrNotSent, result)
		}
	}()
	err := f.opts.Store.WithMemberForward(ctx, f.opts.Identity.Trust(), request, func(scope context.Context, original *sqlite.MemberAcquisition) error {
		member, term := original.Peer(), original.Term()
		peer := Peer{MemberID: member.ID, ClusterID: term.ClusterID, Address: member.Address, Trust: member.Trust}
		self := Peer{MemberID: original.MemberID(), ClusterID: term.ClusterID}
		client, err := NewMemberClient(ClientOptions{Credentials: f.opts.Credentials, Observer: original,
			Self: func() Peer { return self }, Timeout: 30 * time.Second}, f.opts.Identity)
		if err != nil {
			return err
		}
		dispatched = true // from here on the exchange may have begun
		out, err := client.Write(scope, peer, command)
		if err != nil {
			return err
		}
		received = true
		if err := request.Check(scope); err != nil {
			return err
		}
		return emit(scope, out)
	})
	return received, err
}
