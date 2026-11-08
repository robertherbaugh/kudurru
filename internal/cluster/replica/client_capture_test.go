package replica

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestSourceCapturesIdentityAndRotationBeforeResolution(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	server := serverFor(t, p, creds, mirror.Authority().MemberID, nil)
	var calls atomic.Int32
	address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		server.Handler().ServeHTTP(w, r)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address}
	tokens := []string{"old-unrecognized-token", creds.Token()}
	selfCalls, tokenCalls := 0, 0
	self := Peer{MemberID: mirror.Authority().MemberID, ClusterID: peer.ClusterID}
	c := clientFor(t, mirror, creds, func(o *ClientOptions) {
		o.Self = func() Peer { selfCalls++; return self }
		o.Tokens = func() []string { tokenCalls++; return tokens }
	})
	s := sourceFor(t, c, peer, p, func(o *SourceOptions) {
		o.Resolve = func(context.Context) (Peer, error) {
			self.MemberID = "successor-member"
			tokens[1] = "successor-token"
			return peer, nil
		}
	})
	response, err := s.Fetch(t.Context(), "")
	if err != nil || response.Image == nil || selfCalls != 1 || tokenCalls != 1 || calls.Load() != 2 {
		t.Fatal("read did not retain one identity/token vector across original rotation", err, selfCalls, tokenCalls, calls.Load())
	}
}

func TestSourceOriginalLifetimePrecedesResolution(t *testing.T) {
	for _, earlierCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "configured", true: "earlier-caller"}[earlierCaller], func(t *testing.T) {
			p := openStore(t, replication.Primary)
			mirror := mirrorOf(t, p)
			creds := credentials(t, 1)
			var transportCalls atomic.Int32
			c := clientFor(t, mirror, creds, func(o *ClientOptions) {
				o.Timeout = 30 * time.Millisecond
				if earlierCaller {
					o.Timeout = time.Second
				}
				o.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					return nil, ErrProtocol
				})
			})
			peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: "127.0.0.1:1"}
			ctx := t.Context()
			var originalDeadline time.Time
			if earlierCaller {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
				originalDeadline, _ = ctx.Deadline()
			}
			resolved := 0
			s := sourceFor(t, c, peer, p, func(o *SourceOptions) {
				o.Resolve = func(original context.Context) (Peer, error) {
					resolved++
					deadline, ok := original.Deadline()
					if !ok || deadline.After(time.Now().Add(c.opts.Timeout)) || earlierCaller && !deadline.Equal(originalDeadline) {
						t.Fatal("resolution did not inherit original finite bound")
					}
					<-original.Done()
					// Even a resolver returning a peer after expiry cannot dial it.
					return peer, nil
				}
			})
			response, err := s.Fetch(ctx, "")
			if !errors.Is(err, context.DeadlineExceeded) || response.Image != nil || resolved != 1 || transportCalls.Load() != 0 {
				t.Fatal("expired original resolution reached transport", err, resolved, transportCalls.Load())
			}
		})
	}
}

func TestWriteRefusesCredentialAndBodyReplay(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	domain := &fixtureDomain{}
	server := serverFor(t, p, creds, mirror.Authority().MemberID, serviceFor(p, domain))
	var calls atomic.Int32
	address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		server.Handler().ServeHTTP(w, r)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address}
	c := clientFor(t, mirror, creds, func(o *ClientOptions) {
		o.Tokens = func() []string { return []string{"old-unrecognized-token", creds.Token()} }
	})
	base := c.http.Transport
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil {
			t.Fatal("mutation exposed a transport body replay function")
		}
		return base.RoundTrip(r)
	})
	outcome, err := c.Write(t.Context(), peer, commandFor(p, "original-credential"))
	if !errors.Is(err, ErrUnauthorized) || outcome.Status != 0 || calls.Load() != 1 || domain.executions.Load() != 0 {
		t.Fatal("refused mutation retried with another credential", err, calls.Load(), domain.executions.Load())
	}
}

func TestWriteKeepsOriginalCommandExpiry(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	command := commandFor(p, "original-expiry")
	command.NotAfter = time.Now().Add(50 * time.Millisecond)
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: "127.0.0.1:1"}
	calls := 0
	c := clientFor(t, mirror, credentials(t, 1), func(o *ClientOptions) {
		o.Timeout = time.Second
		o.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			deadline, ok := r.Context().Deadline()
			if !ok || !deadline.Equal(command.NotAfter) || r.GetBody != nil {
				t.Fatal("command expiry changed or a replay body escaped")
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
	})
	outcome, err := c.Write(t.Context(), peer, command)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrOutcomeUnknown) || outcome.Status != 0 || calls != 1 {
		t.Fatal("ambiguous expiry acquired a fresh lifetime or success", err, calls)
	}
}

type cancelAtImageEnd struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelAtImageEnd) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.cancel()
	}
	return n, err
}

func TestSourceCannotReturnImageAfterOriginalCancel(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	server := serverFor(t, p, creds, mirror.Authority().MemberID, nil)
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: tlsServer(t, creds, server.Handler())}
	c := clientFor(t, mirror, creds)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := c.http.Transport
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err == nil {
			response.Body = cancelAtImageEnd{ReadCloser: response.Body, cancel: cancel}
		}
		return response, err
	})
	response, err := sourceFor(t, c, peer, p).Fetch(ctx, "")
	var exchange *ExchangeError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &exchange) || !exchange.Reached || response.Image != nil {
		t.Fatal("completed bytes escaped the original canceled acquisition", err)
	}
}
