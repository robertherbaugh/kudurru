package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
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
	route := owner.Zones().Zones(now).Match(apex)
	if route == nil || route.Name() != apex || route.Type() != public.ZoneTypePrimary {
		return reply(dns.RcodeNotAuth)
	}
	native, ok := dnsserver.VerifiedTSIGFor(req, now)
	_, keys, current := owner.NotifyKeys()
	if !ok || keys == nil || current == nil {
		return reply(dns.RcodeRefused)
	}
	if !current() {
		// This node installed a newer commit (in a burst, often the previous
		// UPDATE's own) or replaced a dependency after the request captured
		// its publication. The source changed under the request; the key is
		// not refused. Not sent, never retried (D381).
		return reply(dns.RcodeServerFailure)
	}
	identity, ok := keys.Identity(native.Identity().Key)
	if !ok || identity != native.Identity() {
		return reply(dns.RcodeRefused)
	}
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
	wire, err := msg.Pack()
	if err != nil {
		return reply(dns.RcodeFormatError)
	}
	ctx, cancel := context.WithTimeout(req.Ctx, memberForwardUpdateTimeout)
	defer cancel()
	if signed := native.Deadline(); !signed.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, signed)
		defer stop()
	}
	request, err := s.memberForward.Capture(ctx, token)
	if err != nil {
		return reply(dns.RcodeServerFailure)
	}
	deadline, _ := ctx.Deadline()
	if forward := request.Deadline(); !forward.IsZero() && forward.Before(deadline) {
		deadline = forward
	}
	payload, err := json.Marshal(memberUpdate{Message: wire, Algorithm: identity.Algorithm, Transport: req.Client.Proto.String(), SignatureDeadline: native.Deadline().UTC()})
	if err != nil {
		return reply(dns.RcodeServerFailure)
	}
	issued := time.Now().UTC()
	command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: uuid.NewString(), ExpectedTerm: request.OriginalTerm(),
		Hops: 1, IssuedAt: issued, NotAfter: deadline, Operation: memberDNSUpdate,
		Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: identity.Key, Provider: "tsig",
			CredentialKind: clusterreplica.CredentialTSIG, CredentialID: identity.CredentialVersion, IssuedAt: issued},
		Payload: payload}
	var result memberUpdateResult
	_, err = s.memberForward.Forward(ctx, request, command, func(_ context.Context, out clusterreplica.Outcome) error {
		d := json.NewDecoder(bytes.NewReader(out.Body))
		d.DisallowUnknownFields()
		if out.Status != http.StatusOK || d.Decode(&result) != nil || result.Rcode < 0 || result.Rcode > 15 {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	if err != nil {
		// Not sent, refused by the primary's member admission before
		// execution, or lost after sending: no rcode states "unknown", and
		// RFC 2136 section 4.5 directs the requestor to try another server
		// on SERVFAIL. Nothing here retries or replays the command.
		return reply(dns.RcodeServerFailure)
	}
	return reply(result.Rcode)
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
	lease, err := s.publisher.Capture()
	if err != nil {
		return err
	}
	serving := servingdns.Wrap(lease)
	defer serving.Release()
	forwarded := &forwardedUpdate{key: dnsserver.TSIGIdentity{Key: claim.ID, Algorithm: in.Algorithm, CredentialVersion: claim.CredentialID},
		deadline: in.SignatureDeadline, retryID: command.ID}
	deadline, _ := bound.Deadline()
	update := &pipeline.Request{Ctx: withForwardedUpdate(bound, forwarded), Msg: msg, Received: time.Now(), Deadline: deadline, Serving: serving,
		Client: pipeline.Client{Proto: proto, TSIGKey: claim.ID}}
	response, err := s.primary.update(update)
	if err != nil {
		return err
	}
	if response == nil || response.Msg == nil {
		return clusterreplica.ErrProtocol
	}
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
