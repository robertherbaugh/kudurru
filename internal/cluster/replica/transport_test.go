package replica

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestTLSImageETagCompleteDeleteAndEnrollmentGuard(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	if _, e := p.Apply(ctx, zoneObject("home"), store.ApplyOptions{Manager: store.ManagerAPI}); e != nil {
		t.Fatal(e)
	}
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	server := serverFor(t, p, creds, mirror.Authority().MemberID, nil)
	address := tlsServer(t, creds, server.Handler())
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address}
	client := clientFor(t, mirror, creds)
	source := sourceFor(t, client, peer, p)
	response, e := source.Fetch(ctx, "")
	if e != nil {
		t.Fatal(e)
	}
	if response.Image == nil || !response.Image.Generation().Same(imageOf(t, p).Generation()) {
		t.Fatal("complete image changed")
	}
	response, e = source.Fetch(ctx, response.ETag)
	if e != nil || !response.NotModified || response.Image != nil {
		t.Fatalf("304: %v", e)
	}
	if e := p.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{}); e != nil {
		t.Fatal(e)
	}
	pull := &follower.Puller{Mirror: mirror, Source: source}
	if e := pull.Pull(ctx); e != nil {
		t.Fatal(e)
	}
	if len(imageOf(t, mirror).Payload.Objects) != 0 {
		t.Fatal("transport lost absence deletion")
	}
	wrongClient := clientFor(t, mirror, credentials(t, 2))
	if _, e := sourceFor(t, wrongClient, peer, p).Fetch(ctx, ""); !errors.Is(e, ErrUntrusted) {
		t.Fatalf("wrong TLS key accepted: %v", e)
	}
	badToken := clientFor(t, mirror, creds, func(o *ClientOptions) { o.Tokens = func() []string { return []string{"invalid"} } })
	if _, e := sourceFor(t, badToken, peer, p).Fetch(ctx, ""); !errors.Is(e, ErrUnauthorized) {
		t.Fatalf("bad token accepted: %v", e)
	}
	badMember := clientFor(t, mirror, creds, func(o *ClientOptions) {
		o.Self = func() Peer { return Peer{MemberID: "not-enrolled", ClusterID: peer.ClusterID} }
	})
	if _, e := sourceFor(t, badMember, peer, p).Fetch(ctx, ""); !errors.Is(e, ErrUnauthorized) {
		t.Fatalf("unknown member accepted: %v", e)
	}
}

func TestAuthenticatedHigherTermObservedBeforeRejectedBody(t *testing.T) {
	for _, test := range []string{"incompatible", "corrupt", "declared-oversize", "streamed-oversize"} {
		t.Run(test, func(t *testing.T) {
			ctx := context.Background()
			p := openStore(t, replication.Primary)
			mirror := mirrorOf(t, p)
			before := imageOf(t, mirror)
			higher := p.Authority().Term
			higher.Epoch++
			higher.PromotionID = "higher-promotion"
			creds := credentials(t, 1)
			body := []byte("not-json")
			want := ErrProtocol
			if test == "incompatible" {
				i := *imageOf(t, p)
				i.Term = higher
				i.StoreSchemaVersion++
				body, _ = json.Marshal(i)
				want = replication.ErrIncompatible
			}
			if test == "declared-oversize" || test == "streamed-oversize" {
				body = bytes.Repeat([]byte("x"), 8192)
				want = replication.ErrTooLarge
			}
			var readBeforeObservation atomic.Bool
			address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(HeaderMember, p.Authority().MemberID)
				w.Header().Set(HeaderCluster, higher.ClusterID)
				w.Header().Set(HeaderTerm, encodeTerm(higher))
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", before.Generation().ETag())
				if test == "declared-oversize" {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				}
				if test == "streamed-oversize" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(body)
			}))
			peer := Peer{MemberID: p.Authority().MemberID, ClusterID: higher.ClusterID, Address: address}
			transport := &http.Transport{TLSClientConfig: creds.ClientTLS()}
			t.Cleanup(transport.CloseIdleConnections)
			client := clientFor(t, mirror, creds, func(o *ClientOptions) {
				o.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					response, err := transport.RoundTrip(r)
					if err == nil {
						response.Body = guardedBody{response.Body, func() {
							if !mirror.Authority().Term.Same(higher) {
								readBeforeObservation.Store(true)
							}
						}}
					}
					return response, err
				})
			})
			source := sourceFor(t, client, peer, p, func(o *SourceOptions) { o.Validation.Limits.MaxBytes = 4096 })
			_, e := source.Fetch(ctx, "")
			if test == "corrupt" {
				want = replication.ErrInvalid
			}
			if !errors.Is(e, want) {
				t.Fatalf("body rejection: %v want %v", e, want)
			}
			if readBeforeObservation.Load() || mirror.Authority().Mode != replication.Fenced || !mirror.Authority().Term.Same(higher) || !imageOf(t, mirror).Generation().Same(before.Generation()) {
				t.Fatal("header authority was lost on body failure or last-good replaced")
			}
			var exchange *ExchangeError
			if !errors.As(e, &exchange) || !exchange.Reached {
				t.Fatal("authenticated incompatible peer reported unreachable")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type guardedBody struct {
	io.ReadCloser
	beforeRead func()
}

func (b guardedBody) Read(p []byte) (int, error) { b.beforeRead(); return b.ReadCloser.Read(p) }

func TestErrorResponseAuthorityObservedBeforeDrain(t *testing.T) {
	for _, status := range []int{401, 307, 409} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			p := openStore(t, replication.Primary)
			mirror := mirrorOf(t, p)
			higher := p.Authority().Term
			higher.Epoch++
			higher.PromotionID = "new-term"
			creds := credentials(t, 1)
			address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(HeaderMember, p.Authority().MemberID)
				w.Header().Set(HeaderCluster, higher.ClusterID)
				w.Header().Set(HeaderTerm, encodeTerm(higher))
				w.Header().Set("Location", "https://untrusted.invalid/")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("corrupt-error-body"))
			}))
			transport := &http.Transport{TLSClientConfig: creds.ClientTLS()}
			t.Cleanup(transport.CloseIdleConnections)
			var readBeforeObservation atomic.Bool
			c := clientFor(t, mirror, creds, func(o *ClientOptions) {
				o.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					response, err := transport.RoundTrip(r)
					if err == nil {
						response.Body = guardedBody{response.Body, func() {
							if !mirror.Authority().Term.Same(higher) {
								readBeforeObservation.Store(true)
							}
						}}
					}
					return response, err
				})
			})
			peer := Peer{MemberID: p.Authority().MemberID, ClusterID: higher.ClusterID, Address: address}
			if _, err := sourceFor(t, c, peer, p).Fetch(context.Background(), ""); err == nil {
				t.Fatal("invalid response accepted")
			}
			if readBeforeObservation.Load() || !mirror.Authority().Term.Same(higher) || mirror.Authority().Mode != replication.Fenced {
				t.Fatal("error body drained before durable authority observation")
			}
		})
	}
}

func TestMalformedAuthorityMetadataNeverFences(t *testing.T) {
	for _, test := range []string{"missing-member", "duplicate-member", "duplicate-term", "bad-term", "duplicate-term-key", "wrong-cluster"} {
		t.Run(test, func(t *testing.T) {
			p := openStore(t, replication.Primary)
			mirror := mirrorOf(t, p)
			before := mirror.Authority()
			higher := before.Term
			higher.Epoch++
			creds := credentials(t, 1)
			address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(HeaderMember, p.Authority().MemberID)
				w.Header().Set(HeaderCluster, higher.ClusterID)
				w.Header().Set(HeaderTerm, encodeTerm(higher))
				switch test {
				case "missing-member":
					w.Header().Del(HeaderMember)
				case "duplicate-member":
					w.Header().Add(HeaderMember, "other")
				case "duplicate-term":
					w.Header().Add(HeaderTerm, encodeTerm(higher))
				case "bad-term":
					w.Header().Set(HeaderTerm, "bad-base64!")
				case "duplicate-term-key":
					b, _ := json.Marshal(higher)
					b = append([]byte(`{"epoch":"2",`), b[1:]...)
					w.Header().Set(HeaderTerm, base64.RawURLEncoding.EncodeToString(b))
				case "wrong-cluster":
					higher.ClusterID = "foreign"
					w.Header().Set(HeaderTerm, encodeTerm(higher))
				}
				w.WriteHeader(401)
			}))
			peer := Peer{MemberID: p.Authority().MemberID, ClusterID: before.Term.ClusterID, Address: address}
			if _, err := sourceFor(t, clientFor(t, mirror, creds), peer, p).Fetch(context.Background(), ""); err == nil {
				t.Fatal("malformed authority accepted")
			}
			if !mirror.Authority().Term.Same(before.Term) || mirror.Authority().Mode != before.Mode {
				t.Fatal("unauthenticated or malformed authority fenced writer")
			}
		})
	}
}

func TestImageHeaderMismatchRedirectSelfAndFollowerRefusal(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	i := imageOf(t, p)
	body, _ := json.Marshal(i)
	address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderMember, p.Authority().MemberID)
		w.Header().Set(HeaderCluster, i.Term.ClusterID)
		w.Header().Set(HeaderTerm, encodeTerm(i.Term))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"wrong"`)
		_, _ = w.Write(body)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: i.Term.ClusterID, Address: address}
	client := clientFor(t, mirror, creds)
	if _, e := sourceFor(t, client, peer, p).Fetch(ctx, ""); !errors.Is(e, ErrProtocol) {
		t.Fatalf("etag mismatch: %v", e)
	}
	var destination atomic.Int64
	destinationAddress := tlsServer(t, creds, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { destination.Add(1) }))
	redirect := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+destinationAddress+PathImage, http.StatusTemporaryRedirect)
	}))
	peer.Address = redirect
	if _, e := sourceFor(t, client, peer, p).Fetch(ctx, ""); !errors.Is(e, ErrRedirect) || destination.Load() != 0 {
		t.Fatalf("redirect followed: %v calls=%d", e, destination.Load())
	}
	peer.MemberID = mirror.Authority().MemberID
	if _, e := sourceFor(t, client, peer, p).Fetch(ctx, ""); !errors.Is(e, ErrLoop) {
		t.Fatalf("self target: %v", e)
	}
	// A legitimate follower cannot masquerade as an authoritative image source.
	peer.MemberID = mirror.Authority().MemberID
	another := mirrorOf(t, p)
	followerServer := serverFor(t, mirror, creds, another.Authority().MemberID, nil)
	peer.Address = tlsServer(t, creds, followerServer.Handler())
	if _, e := sourceFor(t, clientFor(t, another, creds), peer, p).Fetch(ctx, ""); !errors.Is(e, replication.ErrReadOnly) {
		t.Fatalf("follower served authoritative image: %v", e)
	}
}

func TestMethodBodyBoundsAndRotationRetry(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	server := serverFor(t, p, creds, mirror.Authority().MemberID, nil)
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: tlsServer(t, creds, server.Handler())}
	c := clientFor(t, mirror, creds, func(o *ClientOptions) {
		o.Tokens = func() []string { return []string{"old-unknown-token", creds.Token()} }
	})
	if _, e := sourceFor(t, c, peer, p).Fetch(ctx, ""); e != nil {
		t.Fatalf("explicit token fallback: %v", e)
	}
	response, e := c.request(ctx, peer, http.MethodGet, PathImage, []byte("body"), "")
	if e != nil {
		t.Fatal(e)
	}
	defer closeResponse(response)
	if response.StatusCode != 400 {
		t.Fatalf("GET body accepted: %d", response.StatusCode)
	}
	response, e = c.request(ctx, peer, http.MethodDelete, PathWrite, nil, "")
	if e != nil {
		t.Fatal(e)
	}
	defer closeResponse(response)
	if response.StatusCode != 405 {
		t.Fatalf("write method accepted: %d", response.StatusCode)
	}
}
