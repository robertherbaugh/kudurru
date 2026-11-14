package replica

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func memberIdentityFor(t *testing.T, store *sqlite.Store) *memberidentity.Identity {
	t.Helper()
	i, err := memberidentity.Create(filepath.Join(t.TempDir(), "member.json"), store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func memberReplicaServer(t *testing.T, identity *memberidentity.Identity, peer memberidentity.Trust, handler http.Handler) string {
	t.Helper()
	config, err := identity.ServerTLS([]memberidentity.Trust{peer})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = config
	s.StartTLS()
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "https://")
}

func TestMemberClientCapturesTargetBeforeHTTPAndNeverReusesOldTrust(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	primaryIdentity := memberIdentityFor(t, p)
	followerIdentity := memberIdentityFor(t, mirror)
	creds := credentials(t, 1)
	var calls atomic.Int32
	server, err := NewServer(ServerOptions{Store: p, Credentials: creds, AuthorizePeer: func(_ context.Context, peer Peer, r *http.Request) error {
		if peer.MemberID != mirror.Authority().MemberID {
			return ErrUnauthorized
		}
		return followerIdentity.Trust().VerifyPeer(r.TLS)
	}})
	if err != nil {
		t.Fatal(err)
	}
	address := memberReplicaServer(t, primaryIdentity, followerIdentity.Trust(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !r.Close {
			t.Error("member exchange permitted cross-enrollment connection reuse")
		}
		server.Handler().ServeHTTP(w, r)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	client, err := NewMemberClient(ClientOptions{Credentials: creds, Observer: mirror, Self: func() Peer {
		return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID}
	}}, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	source := sourceFor(t, client, peer, p, func(o *SourceOptions) {
		o.Resolve = func(context.Context) (Peer, error) { return peer, nil }
	})
	first, err := source.Fetch(t.Context(), "")
	if err != nil || first.Image == nil {
		t.Fatal("independent member TLS failed the actual replica image", err)
	}
	second, err := source.Fetch(t.Context(), first.ETag)
	if err != nil || !second.NotModified || calls.Load() != 2 {
		t.Fatal("original member 304 failed", err, calls.Load())
	}
	before := mirror.Authority()
	replacement := memberIdentityFor(t, p) // same UUID, independently generated CA
	peer.Trust = replacement.Trust()
	if response, err := source.Fetch(t.Context(), ""); !errors.Is(err, ErrUntrusted) || response.Image != nil {
		t.Fatal("new trust used an old verified connection at the same address", err)
	}
	if calls.Load() != 2 || !mirror.Authority().Term.Same(before.Term) {
		t.Fatal("untrusted target received HTTP or changed authority")
	}
	peer.Trust = memberidentity.Trust{}
	if _, err := source.Fetch(t.Context(), ""); !errors.Is(err, ErrUntrusted) || calls.Load() != 2 {
		t.Fatal("missing enrollment fell back to shared trust", err)
	}
}

func TestMemberClientCannotSendCommandToSharedCredentialImpostor(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	primaryIdentity := memberIdentityFor(t, p)
	followerIdentity := memberIdentityFor(t, mirror)
	impostor := memberIdentityFor(t, p)
	creds := credentials(t, 1)
	var requests atomic.Int32
	address := memberReplicaServer(t, impostor, followerIdentity.Trust(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		higher := p.Authority().Term
		higher.Epoch++
		w.Header().Set(HeaderMember, p.Authority().MemberID)
		w.Header().Set(HeaderCluster, higher.ClusterID)
		w.Header().Set(HeaderTerm, encodeTerm(higher))
		w.WriteHeader(500)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	opts := ClientOptions{Credentials: creds, Observer: mirror, Self: func() Peer { return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID} }}
	client, err := NewMemberClient(opts, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	before := mirror.Authority()
	out, err := client.Write(t.Context(), peer, commandFor(p, "original-member-command"))
	if !errors.Is(err, ErrUntrusted) || errors.Is(err, ErrOutcomeUnknown) || out.Status != 0 || requests.Load() != 0 || !mirror.Authority().Term.Same(before.Term) {
		t.Fatal("impostor received command credentials/body or fenced the original member", err, requests.Load())
	}
	opts.Transport = http.DefaultTransport
	if _, err := NewMemberClient(opts, followerIdentity); !errors.Is(err, ErrProtocol) {
		t.Fatal("member client allowed a transport verification bypass", err)
	}
	client.opts.Self = func() Peer { return Peer{MemberID: p.Authority().MemberID, ClusterID: peer.ClusterID} }
	if _, err := client.Write(t.Context(), peer, commandFor(p, "wrong-local-key")); !errors.Is(err, ErrUnauthorized) || requests.Load() != 0 {
		t.Fatal("local UUID did not match its private identity", err)
	}
}

func TestMemberClientSuccessfulWriteAndRefusalKeepOneDispatch(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	primaryIdentity := memberIdentityFor(t, p)
	followerIdentity := memberIdentityFor(t, mirror)
	creds := credentials(t, 1)
	domain := &fixtureDomain{}
	server, err := NewServer(ServerOptions{Store: p, Credentials: creds, Commands: serviceFor(p, domain), AuthorizePeer: func(_ context.Context, peer Peer, r *http.Request) error {
		if peer.MemberID != mirror.Authority().MemberID {
			return ErrUnauthorized
		}
		return followerIdentity.Trust().VerifyPeer(r.TLS)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	address := memberReplicaServer(t, primaryIdentity, followerIdentity.Trust(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		server.Handler().ServeHTTP(w, r)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	client, err := NewMemberClient(ClientOptions{Credentials: creds, Observer: mirror, Self: func() Peer { return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID} }}, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	command := commandFor(p, "single-member-write")
	if _, err := client.Write(t.Context(), peer, command); err != nil || domain.executions.Load() != 1 {
		t.Fatal("actual independently authenticated write failed", err)
	}
	if _, err := client.Write(t.Context(), peer, command); err != nil || domain.executions.Load() != 1 || calls.Load() != 2 {
		t.Fatal("explicit same-intent retry repeated the domain effect", err)
	}
	client.opts.Tokens = func() []string { return []string{"wrong-original-token", creds.Token()} }
	if _, err := client.Write(t.Context(), peer, commandFor(p, "refused-member-write")); !errors.Is(err, ErrUnauthorized) || calls.Load() != 3 || domain.executions.Load() != 1 {
		t.Fatal("original credential refusal tried a replacement or replayed body", err)
	}
}

type cancelAtEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelAtEOF) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.cancel()
	}
	return n, err
}

func TestWriteCompletedBodyCancellationPreservesUnknownCommit(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	domain := &fixtureDomain{}
	server := serverFor(t, p, creds, mirror.Authority().MemberID, serviceFor(p, domain))
	address := tlsServer(t, creds, server.Handler())
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transport := &http.Transport{TLSClientConfig: creds.ClientTLS()}
	t.Cleanup(transport.CloseIdleConnections)
	client := clientFor(t, mirror, creds, func(o *ClientOptions) {
		o.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(r)
			if err == nil {
				response.Body = cancelAtEOF{ReadCloser: response.Body, cancel: cancel}
			}
			return response, err
		})
	})
	out, err := client.Write(ctx, peer, commandFor(p, "completed-body-original-cancel"))
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) || out.Status != 0 || domain.executions.Load() != 1 {
		t.Fatal("late cancellation claimed success or erased actual committed work", err, domain.executions.Load())
	}
}

func TestMemberClientTLSRefusalCannotManufactureNetworkLoss(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	other := openStore(t, replication.Follower)
	primaryIdentity := memberIdentityFor(t, p)
	followerIdentity := memberIdentityFor(t, mirror)
	otherIdentity := memberIdentityFor(t, other)
	var calls atomic.Int32
	address := memberReplicaServer(t, primaryIdentity, otherIdentity.Trust(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	client, err := NewMemberClient(ClientOptions{Credentials: credentials(t, 1), Observer: mirror, Self: func() Peer { return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID} }}, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sourceFor(t, client, peer, p).Fetch(t.Context(), "")
	if !errors.Is(err, ErrUntrusted) || errors.Is(err, ErrUnavailable) || calls.Load() != 0 {
		t.Fatal("actual peer TLS refusal became promotion-eligible network loss", err, calls.Load())
	}
}

func TestMemberClientAuthenticatedAbortDoesNotCreatePromotionWindow(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	primaryIdentity, followerIdentity := memberIdentityFor(t, p), memberIdentityFor(t, mirror)
	var calls atomic.Int32
	address := memberReplicaServer(t, primaryIdentity, followerIdentity.Trust(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1); panic(http.ErrAbortHandler) }))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	client, err := NewMemberClient(ClientOptions{Credentials: credentials(t, 1), Observer: mirror, Self: func() Peer { return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID} }}, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	var elapsed time.Duration
	coordinator, err := NewCoordinator(CoordinatorOptions{Store: mirror, Source: sourceFor(t, client, peer, p), NodeName: "follower", Interval: time.Second, Elapsed: func() time.Duration { return elapsed }, Served: func(context.Context) (replication.Generation, func(), error) {
		return imageOf(t, mirror).Generation(), func() {}, nil
	}, AuthorizeOperator: func(context.Context) (PromotionAuthorization, error) {
		return PromotionAuthorization{Check: func(context.Context) error { return nil }, Release: func() {}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	before := mirror.Authority()
	for n := 0; n < 3; n++ {
		elapsed = time.Duration(n) * time.Second
		err := coordinator.Pull(t.Context())
		var contact *ExchangeError
		if !errors.As(err, &contact) || !contact.Reached {
			t.Fatal("authenticated primary abort was classified as unreachable", err)
		}
		status := coordinator.Status(t.Context())
		if status.Eligible || status.MissedPulls != 0 || status.Reason == "primary_unreachable" {
			t.Fatal("authenticated abort created promotion eligibility", status)
		}
	}
	if calls.Load() != 3 || !mirror.Authority().Term.Same(before.Term) {
		t.Fatal("failed original exchange retried or adopted authority", calls.Load())
	}
}

func TestMemberClientAuthenticatedWriteAbortKeepsUnknownAndContact(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	primaryIdentity, followerIdentity := memberIdentityFor(t, p), memberIdentityFor(t, mirror)
	var calls atomic.Int32
	address := memberReplicaServer(t, primaryIdentity, followerIdentity.Trust(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1); panic(http.ErrAbortHandler) }))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	client, err := NewMemberClient(ClientOptions{Credentials: credentials(t, 1), Observer: mirror, Self: func() Peer { return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID} }}, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.Write(t.Context(), peer, commandFor(p, "authenticated-abort"))
	var contact *ExchangeError
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.As(err, &contact) || !contact.Reached || out.Status != 0 || calls.Load() != 1 {
		t.Fatal("failed acknowledgment lost unknown outcome or original contact", err, calls.Load())
	}
}

func TestMemberClientConnectionFailureRemainsNetworkLoss(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	primaryIdentity, followerIdentity := memberIdentityFor(t, p), memberIdentityFor(t, mirror)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address, Trust: primaryIdentity.Trust()}
	client, err := NewMemberClient(ClientOptions{Credentials: credentials(t, 1), Observer: mirror, Self: func() Peer { return Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID} }}, followerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sourceFor(t, client, peer, p).Fetch(t.Context(), "")
	var contact *ExchangeError
	if !errors.Is(err, ErrUnavailable) || errors.As(err, &contact) && contact.Reached {
		t.Fatal("failed connection manufactured authenticated contact", err)
	}
}
