package dnsserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func configuredLimits(t *testing.T, key, value string) Limits {
	t.Helper()
	cfg, err := config.Load("", []string{"KUDURRU_LISTENERS__LIMITS__" + strings.ToUpper(key) + "=" + value})
	if err != nil {
		t.Fatal(err)
	}
	return LimitsFromConfig(cfg.Listeners)
}

func TestConfiguredIdleTimeouts(t *testing.T) {
	for _, proto := range []Protocol{ProtoTCP, ProtoDoT, ProtoDoH} {
		t.Run(string(proto), func(t *testing.T) {
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, stages: []pipeline.Stage{localAnswer{}}, limits: configuredLimits(t, "idleTimeout", "200ms")})
			var conn net.Conn
			var err error
			if proto == ProtoTCP {
				conn, err = net.DialTimeout("tcp", f.addr(proto), time.Second)
			} else {
				conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.addr(proto), (&tlsPool{f.cert}).client())
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if proto == ProtoDoH {
				wire, _ := query("example.com").Pack()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+f.addr(proto)+DefaultDoHPath+"?dns="+base64.RawURLEncoding.EncodeToString(wire), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = req.Write(conn); err != nil {
					t.Fatal(err)
				}
				response, err := http.ReadResponse(bufio.NewReader(conn), req)
				if err != nil {
					t.Fatal(err)
				}
				_, copyErr := io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if copyErr != nil || response.StatusCode != http.StatusOK {
					t.Fatal("initial DoH request failed", copyErr, response.StatusCode)
				}
			} else {
				client := &dns.Client{Net: "tcp", Timeout: time.Second}
				if _, _, err := client.ExchangeWithConn(query("example.com"), &dns.Conn{Conn: conn}); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			_, err = conn.Read(b[:])
			if !errors.Is(err, io.EOF) {
				t.Fatalf("idle connection did not close: %v", err)
			}
		})
	}
	for _, proto := range []Protocol{ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			accepted := make(chan *quic.Conn, 1)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, stages: []pipeline.Stage{localAnswer{}}, limits: configuredLimits(t, "quicIdleTimeout", "200ms"), beforeRun: func(reg *Registry) {
				if proto == ProtoDoH3 {
					reg.Listeners()[0].(*doh3Listener).srv.ConnContext = func(ctx context.Context, conn *quic.Conn) context.Context {
						accepted <- conn
						return ctx
					}
				}
			}})
			alpn := "doq"
			if proto == ProtoDoH3 {
				alpn = "h3"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			conn, err := quic.DialAddr(ctx, f.addr(proto), (&tlsPool{f.cert}).client(alpn), &quic.Config{MaxIdleTimeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQUIC(conn)
			observed := conn
			if proto == ProtoDoQ {
				q := query("example.com")
				q.Id = 0
				if _, err := doqExchange(conn, q); err != nil {
					t.Fatal(err)
				}
				// quic-go clamps the peer-advertised idle timeout to five
				// seconds. Observe the actual server connection's timeout,
				// not the client's later detection of this silent close.
				listener := f.reg.Listeners()[0].(*doqListener)
				listener.mu.Lock()
				for current := range listener.active {
					observed = current
				}
				listener.mu.Unlock()
				if observed == conn {
					t.Fatal("server connection not admitted")
				}
			} else {
				// The transport can close silently before HTTP/3's idle
				// handler sends a close frame. As with DoQ, observe resource
				// retirement at the server rather than the client's longer
				// minimum peer-timeout detection window.
				select {
				case observed = <-accepted:
				case <-ctx.Done():
					t.Fatal("HTTP/3 server connection not admitted")
				}
			}
			select {
			case <-observed.Context().Done():
				cause := context.Cause(observed.Context())
				var application *quic.ApplicationError
				httpIdleClose := proto == ProtoDoH3 && errors.As(cause, &application) && !application.Remote && application.ErrorCode == quic.ApplicationErrorCode(http3.ErrCodeNoError)
				if !errors.As(cause, new(*quic.IdleTimeoutError)) && !httpIdleClose {
					t.Fatal("server closed for a reason other than idle timeout")
				}
			case <-ctx.Done():
				t.Fatal("configured QUIC idle timeout did not close connection")
			}
		})
	}
}

func TestConfiguredDoH3ConnectionLimit(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoH3}}, stages: []pipeline.Stage{localAnswer{}}, limits: configuredLimits(t, "quicMaxConns", "1")})
	pool := &tlsPool{f.cert}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	first, err := quic.DialAddr(ctx, f.addr(ProtoDoH3), pool.client("h3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQUIC(first)
	// A completed HTTP/3 exchange proves the first connection holds its slot.
	tr := &http3.Transport{TLSClientConfig: pool.client("h3")}
	hc := tr.NewClientConn(first)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+f.addr(ProtoDoH3)+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	second, err := quic.DialAddr(ctx, f.addr(ProtoDoH3), pool.client("h3"), nil)
	if err == nil {
		defer closeQUIC(second)
		select {
		case <-second.Context().Done():
			err = context.Cause(second.Context())
		case <-ctx.Done():
			t.Fatal("excess HTTP/3 connection not closed")
		}
	}
	var appErr *quic.ApplicationError
	var transportErr *quic.TransportError
	// HTTP/3 accepts early connections. RFC 9000 section 10.2.3 maps an
	// application close before handshake confirmation to APPLICATION_ERROR.
	applicationClose := errors.As(err, &appErr) && appErr.Remote && appErr.ErrorCode == quic.ApplicationErrorCode(http3.ErrCodeExcessiveLoad)
	earlyClose := errors.As(err, &transportErr) && transportErr.Remote && transportErr.ErrorCode == quic.ApplicationErrorErrorCode
	if !applicationClose && !earlyClose {
		t.Fatal("excess HTTP/3 connection did not receive its protocol refusal")
	}
}
