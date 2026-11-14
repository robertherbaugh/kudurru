package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// memberDNSUpdate is the version-1 member command that carries one RFC 2136
// UPDATE from an enrolled follower to its primary (D381).
const memberDNSUpdate = "dns.update"

// memberForwardUpdateTimeout bounds one follower's exchange with its primary
// below the DNS request's own deadline: a client that waited longer has
// already retried or given up.
const memberForwardUpdateTimeout = 4 * time.Second

// memberUpdate is the follower's verified UPDATE. Message is the exact signed
// request without its TSIG record; the follower verified that signature with
// the key named in the command subject. The primary admits the key again
// against its own current keyring and runs its own grants and prerequisites.
type memberUpdate struct {
	Message           []byte    `json:"message"`
	Algorithm         string    `json:"algorithm"`
	Transport         string    `json:"transport"`
	SignatureDeadline time.Time `json:"signatureDeadline"`
}

// memberUpdateResult is the primary's DNS answer to the forwarded UPDATE.
// The follower relays exactly this rcode and signs it with the request's key.
type memberUpdateResult struct {
	Rcode int `json:"rcode"`
}

// forwardedUpdate is the private context capability of one forwarded UPDATE
// on the primary. Only executeMemberUpdate sets it, inside an authenticated
// member command; a native DNS request can never carry it.
type forwardedUpdate struct {
	key      dnsserver.TSIGIdentity
	deadline time.Time
	retryID  string
	// stale is set by captureUpdate when it rejected the request only
	// because its lease was not this node's current writer publication,
	// before anything was admitted or applied.
	stale bool
}

type forwardedUpdateKey struct{}

func withForwardedUpdate(ctx context.Context, f *forwardedUpdate) context.Context {
	return context.WithValue(ctx, forwardedUpdateKey{}, f)
}

func forwardedUpdateFrom(ctx context.Context) (*forwardedUpdate, bool) {
	if ctx == nil {
		return nil, false
	}
	f, ok := ctx.Value(forwardedUpdateKey{}).(*forwardedUpdate)
	return f, ok && f != nil
}

// forwardUpdate answers a signed UPDATE on an enrolled SQLite follower. The
// follower selects the zone in its own complete cut, verifies the signature
// and admits the key against its own current keyring, then sends one member
// command to the primary and relays the primary's rcode. A command that was
// not sent, and one whose outcome is unknown, both answer SERVFAIL and are
// never retried (D381). handled=false keeps the caller's local answer.
func (s *servingRuntime) forwardUpdate(req *pipeline.Request, apex string) (*pipeline.Response, bool) {
	if s == nil || req == nil || req.Msg == nil || req.Ctx == nil {
		return nil, false
	}
	token, err := servingdns.SourceToken(req)
	if err != nil || token.Authority().Mode != replication.Follower || s.memberForward == nil || s.opts.Config.Cluster.Role == "static" {
		return nil, false
	}
	reply := func(code int) (*pipeline.Response, bool) {
		response := pipeline.Reply(req, code, pipeline.StatusAuthoritative)
		if output, ok := dnsserver.CapturedOutput(req).(*rootAnchorOutput); ok {
			// The answer depends on the primary, not on this node's data:
			// a pull that installs the primary's commit before emission must
			// not withhold it.
			output.completeForwardedUpdate(req, response)
		}
		return response, true
	}
	now := time.Now()
	owner, err := servingdns.Component[*servingbuild.RuntimeOwner](req, "runtime")
	if err != nil || owner.Zones() == nil {
		return reply(dns.RcodeServerFailure)
	}
	native, verified := dnsserver.VerifiedTSIGFor(req, now)
	bound := memberForwardUpdateTimeout
	if s.memberForwardUpdateBound > 0 {
		bound = s.memberForwardUpdateBound
	}
	ctx, cancel := context.WithTimeout(req.Ctx, bound)
	defer cancel()
	if verified {
		if signed := native.Deadline(); !signed.IsZero() {
			var stop context.CancelFunc
			ctx, stop = context.WithDeadline(ctx, signed)
			defer stop()
		}
	}
	// held is a later publication of this node that the forward uses instead
	// of the request's own, see below.
	var held *serving.Lease
	defer func() {
		if held != nil {
			held.Release()
		}
	}()
	var wire []byte
	commandID := uuid.NewString()
	var result memberUpdateResult
	for {
		route := owner.Zones().Zones(time.Now()).Match(apex)
		if route == nil || route.Name() != apex || route.Type() != public.ZoneTypePrimary {
			return reply(dns.RcodeNotAuth)
		}
		_, keys, current := owner.NotifyKeys()
		if !verified || keys == nil || current == nil {
			return reply(dns.RcodeRefused)
		}
		var request *sqlite.MemberForwardRequest
		if current() {
			identity, ok := keys.Identity(native.Identity().Key)
			if !ok || identity != native.Identity() {
				return reply(dns.RcodeRefused)
			}
			if wire == nil {
				msg := native.Message()
				if msg == nil {
					return reply(dns.RcodeServerFailure)
				}
				extra := msg.Extra[:0]
				for _, rr := range msg.Extra {
					if rr.Header().Rrtype != dns.TypeTSIG {
						extra = append(extra, rr)
					}
				}
				msg.Extra = extra
				msg.Compress = false
				if wire, err = msg.Pack(); err != nil {
					return reply(dns.RcodeFormatError)
				}
			}
			request, err = s.memberForward.Capture(ctx, token)
			if err == nil {
				err = s.sendMemberUpdate(ctx, req, request, commandID, identity, native.Deadline(), wire, &result)
				if err == nil {
					return reply(result.Rcode)
				}
			}
			// Only a source change before anything was sent is waited out:
			// the forward capture refused the changed source, or the forward
			// found it changed when it acquired it, before its exchange with
			// the primary began. Any other failure, and every failure once
			// the exchange may have begun, answers SERVFAIL and is never
			// retried or replayed: no rcode states "unknown", and RFC 2136
			// section 4.5 directs the requestor to try another server on
			// SERVFAIL.
			if !errors.Is(err, replication.ErrCAS) || request != nil && !errors.Is(err, clusterreplica.ErrNotSent) {
				return reply(dns.RcodeServerFailure)
			}
		}
		// This node installed a newer commit (in a burst, usually the
		// previous UPDATE's own) or replaced a dependency after the request
		// captured its publication: the source changed under the request,
		// and nothing was sent. The follower decides nothing that the
		// primary does not decide again, so instead of refusing it waits,
		// within the forward's bound, until it serves a current publication,
		// and selects the zone and admits the key again there (D381). A
		// command that was sent is never sent again.
		var previous *serving.Bundle
		if held != nil {
			previous = held.Bundle()
		}
		lease, next, err := s.awaitFollowerPublication(ctx, previous)
		if err != nil {
			return reply(dns.RcodeServerFailure)
		}
		if held != nil {
			held.Release()
		}
		held = lease
		if token, err = lease.SourceToken(); err != nil || token.Authority().Mode != replication.Follower {
			return reply(dns.RcodeServerFailure)
		}
		owner = next
	}
}

// sendMemberUpdate sends one dns.update member command for the follower's
// verified UPDATE on request and decodes the primary's rcode into result.
// An error joined with clusterreplica.ErrNotSent was refused before the
// exchange with the primary began; any other error may follow a dispatch.
func (s *servingRuntime) sendMemberUpdate(ctx context.Context, req *pipeline.Request, request *sqlite.MemberForwardRequest, commandID string, identity dnsserver.TSIGIdentity, signature time.Time, wire []byte, result *memberUpdateResult) error {
	deadline, _ := ctx.Deadline()
	if forward := request.Deadline(); !forward.IsZero() && forward.Before(deadline) {
		deadline = forward
	}
	payload, err := json.Marshal(memberUpdate{Message: wire, Algorithm: identity.Algorithm, Transport: req.Client.Proto.String(), SignatureDeadline: signature.UTC()})
	if err != nil {
		return errors.Join(clusterreplica.ErrNotSent, err)
	}
	issued := time.Now().UTC()
	command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: commandID, ExpectedTerm: request.OriginalTerm(),
		Hops: 1, IssuedAt: issued, NotAfter: deadline, Operation: memberDNSUpdate,
		Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: identity.Key, Provider: "tsig",
			CredentialKind: clusterreplica.CredentialTSIG, CredentialID: identity.CredentialVersion, IssuedAt: issued},
		Payload: payload}
	_, err = s.memberForward.Forward(ctx, request, command, func(_ context.Context, out clusterreplica.Outcome) error {
		d := json.NewDecoder(bytes.NewReader(out.Body))
		d.DisallowUnknownFields()
		if out.Status != http.StatusOK || d.Decode(result) != nil || result.Rcode < 0 || result.Rcode > 15 {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	return err
}

// awaitFollowerPublication returns, within ctx, this follower's current
// complete publication, waiting while it prepares one, with its runtime
// owner. A publication that is the previous one is waited past, so a caller
// that found it no longer current does not get it again. It never installs
// anything itself.
func (s *servingRuntime) awaitFollowerPublication(ctx context.Context, previous *serving.Bundle) (*serving.Lease, *servingbuild.RuntimeOwner, error) {
	for {
		changed := s.publisher.Subscribe()
		lease, err := s.publisher.CaptureHTTP(ctx)
		switch {
		case err == nil && lease.Bundle() != previous:
			value, _ := lease.Bundle().Component("runtime")
			if owner, ok := value.(*servingbuild.RuntimeOwner); ok && owner != nil && owner.Zones() != nil {
				return lease, owner, nil
			}
			lease.Release()
			return nil, nil, serving.ErrComponents
		case err == nil:
			lease.Release()
		case !errors.Is(err, serving.ErrPending):
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
}

// executeMemberUpdate runs one forwarded UPDATE on the primary through the
// same protocol adapter, zone selection, key admission, grants, prerequisites
// and atomic commit as a native UPDATE. The command ID is the update's retry
// identity in the zone's receipt ledger, so a duplicate delivery of the same
// command cannot apply it twice. The answer is acknowledged only inside the
// member request's own command output scope.
func (s *servingRuntime) executeMemberUpdate(ctx context.Context, request *sqlite.MemberRequest, command clusterreplica.Command, emit func(context.Context, clusterreplica.Outcome) error) error {
	if command.Version != 1 || command.Operation != memberDNSUpdate || command.Credential != nil || len(command.Preconditions) != 0 {
		return clusterreplica.ErrProtocol
	}
	var in memberUpdate
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || len(in.Message) == 0 || len(in.Message) > dns.MaxMsgSize || in.SignatureDeadline.IsZero() ||
		(in.Algorithm != dns.HmacSHA256 && in.Algorithm != dns.HmacSHA512) {
		return clusterreplica.ErrProtocol
	}
	claim := command.Subject
	if claim.Kind != clusterreplica.SubjectTSIGKey || claim.Provider != "tsig" || claim.CredentialKind != clusterreplica.CredentialTSIG ||
		claim.ID == "" || claim.ID != dns.CanonicalName(claim.ID) || claim.CredentialID == "" {
		return clusterreplica.ErrProtocol
	}
	var proto pipeline.Proto
	switch in.Transport {
	case "udp":
		proto = pipeline.ProtoUDP
	case "tcp":
		proto = pipeline.ProtoTCP
	case "dot":
		proto = pipeline.ProtoDoT
	default:
		return clusterreplica.ErrProtocol
	}
	msg := new(dns.Msg)
	if msg.Unpack(in.Message) != nil || msg.Opcode != dns.OpcodeUpdate || msg.Response || msg.IsTsig() != nil {
		return clusterreplica.ErrProtocol
	}
	status := s.primary.driver.Status()
	if !status.Running || !status.Ready {
		return replication.ErrFenced
	}
	original := s.primary.writer.active.Load()
	ctx, cancel := context.WithDeadline(ctx, command.NotAfter)
	defer cancel()
	bound, release, err := s.primary.writer.bind(ctx, status.Writer)
	if err != nil {
		return err
	}
	defer release()
	response, releaseLease, err := s.runMemberUpdate(bound, status.Writer, command.ID, msg, proto, claim, in)
	if err != nil {
		return err
	}
	defer releaseLease()
	defer response.Close()
	body, err := json.Marshal(memberUpdateResult{Rcode: response.Msg.Rcode})
	if err != nil {
		return err
	}
	check := func(ctx context.Context) error {
		if !s.primary.writer.current(original) {
			return replication.ErrFenced
		}
		return ctx.Err()
	}
	// The UPDATE committed (or refused) in the zone runtime's own fenced
	// transaction. This empty member command only opens the request's
	// command output scope, so the acknowledgment is admitted under the
	// same original peer, membership and authority checks as any other.
	// Unlike a generic member command, the effect itself is not rechecked
	// against current membership: a member revoked after CaptureMemberRequest
	// can still commit, and only its answer is withheld (D381). The effect
	// remains bound to this writer, term and the admitted key identity.
	_, err = s.opts.Store.MemberCommandWithOutput(bound, request, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := check(ctx); err != nil {
			return err
		}
		return tx.RequireCommitCheck(check)
	}, func(scope context.Context) error {
		if err := check(scope); err != nil {
			return err
		}
		if err := emit(scope, clusterreplica.Outcome{Status: http.StatusOK, Body: body}); err != nil {
			return err
		}
		return check(scope)
	})
	return err
}

// runMemberUpdate runs a forwarded UPDATE on the primary's current writer
// publication. Each commit is followed within milliseconds by its
// publication, and often by the node's own status and lifecycle writes and
// their publications, so a lease captured at once is often already replaced,
// or its zone is briefly not admitted for updates, and captureUpdate refuses
// it with SERVFAIL before any admission: most of a burst of UPDATEs through a
// follower failed so (D381). A forwarded UPDATE instead waits, inside the
// member command's bound, until this writer serves a current publication. It
// runs again only when nothing was decided or committed: when captureUpdate
// rejected it only because of that publication state (forwardedUpdate.stale),
// when every compare-and-swap attempt of Apply lost against another writer
// of the zone and nothing committed (pr.ErrConflict), or when the command ID
// was already in the receipt ledger and only the publication of its
// retained outcome failed. It then waits for the next publication first. Every other answer is final, including a commit
// whose publication failed; the command ID is the retry identity in the
// zone's receipt ledger throughout, so no attempt can apply twice. The
// returned release ends the serving lease the answer was computed under.
func (s *servingRuntime) runMemberUpdate(ctx context.Context, writer pr.PublicationFence, commandID string, msg *dns.Msg, proto pipeline.Proto, claim clusterreplica.SubjectRef, in memberUpdate) (*pipeline.Response, func(), error) {
	target := serving.TermActivation{Term: writer.Term, Incarnation: writer.Incarnation}
	var previous *serving.Bundle
	for {
		if previous != nil {
			// The last attempt's publication is still served: wait until it
			// is replaced instead of capturing it again.
			changed := s.publisher.Subscribe()
			if lease, err := s.publisher.Capture(); err == nil {
				same := lease.Bundle() == previous
				lease.Release()
				if same {
					select {
					case <-ctx.Done():
						return nil, nil, ctx.Err()
					case <-changed:
					}
					continue
				}
			}
		}
		if err := s.publisher.WaitTerm(ctx, target); err != nil {
			return nil, nil, err
		}
		captured, err := s.publisher.Capture()
		if err != nil {
			return nil, nil, err
		}
		lease := servingdns.Wrap(captured)
		forwarded := &forwardedUpdate{key: dnsserver.TSIGIdentity{Key: claim.ID, Algorithm: in.Algorithm, CredentialVersion: claim.CredentialID},
			deadline: in.SignatureDeadline, retryID: commandID}
		deadline, _ := ctx.Deadline()
		var applied *pr.UpdateOutcome
		attempt := network.WithOutcome(withForwardedUpdate(ctx, forwarded), func(out pr.UpdateOutcome) { applied = &out })
		update := &pipeline.Request{Ctx: attempt, Msg: msg.Copy(), Received: time.Now(), Deadline: deadline, Serving: lease,
			Client: pipeline.Client{Proto: proto, TSIGKey: claim.ID}}
		response, err := s.primary.update(update)
		if err != nil {
			lease.Release()
			return nil, nil, err
		}
		if response == nil || response.Msg == nil {
			if response != nil {
				response.Close()
			}
			lease.Release()
			return nil, nil, clusterreplica.ErrProtocol
		}
		// Apply also refuses, without committing, when every one of its
		// compare-and-swap attempts lost against another writer of the zone
		// (the node's own status and lifecycle writes follow each commit).
		lost := applied != nil && !applied.Committed && errors.Is(applied.Err, pr.ErrConflict)
		// A command ID the zone's receipt ledger already holds commits
		// nothing again: Apply only publishes the retained outcome, and that
		// publication can meet a replaced writer publication ("requested term
		// superseded"; 2 of 15 HA-OPEN6 fix-diag04 runs of
		// TestRootMemberUpdateKeyIdentityAndRetryActualHTTP). The retained
		// answer is read again on the next publication.
		replay := applied != nil && applied.Deduplicated && !applied.Published
		if (forwarded.stale || lost || replay) && response.Msg.Rcode == dns.RcodeServerFailure {
			previous = captured.Bundle()
			response.Close()
			lease.Release()
			continue
		}
		return response, lease.Release, nil
	}
}
