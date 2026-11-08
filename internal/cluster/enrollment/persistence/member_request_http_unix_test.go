//go:build unix

package persistence_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func memberHTTP(t *testing.T, f memberFixture, http2 bool, handler http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	serverConfig, err := f.local.ServerTLS([]memberidentity.Trust{f.remote.Trust()})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2, server.TLS = http2, serverConfig
	server.StartTLS()
	t.Cleanup(server.Close)
	clientConfig, err := f.remote.ClientTLS(f.local.Trust())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientConfig, ForceAttemptHTTP2: http2}
	t.Cleanup(transport.CloseIdleConnections)
	return server, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func emitMemberHTTP(ctx context.Context, w http.ResponseWriter, status int, body []byte) error {
	deadline, finite := ctx.Deadline()
	if !finite {
		return errors.New("missing original output deadline")
	}
	control := http.NewResponseController(w)
	if err := control.SetWriteDeadline(deadline); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store, private")
	if status != http.StatusNotModified {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if len(body) != 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return control.Flush()
}

func TestMemberRequestActualHTTPSmallBodyAnd304FlushBeforeHandlerReturn(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, mode := range []string{"image", "command", "304"} {
			t.Run(fmt.Sprintf("http2=%t/%s", http2, mode), func(t *testing.T) {
				f := newMemberFixture(t)
				release := make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(release) })
				outcomes := make(chan error, 1)
				server, client := memberHTTP(t, f, http2, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
					defer cancel()
					original, err := f.s.CaptureMemberRequest(ctx, f.s.Authority().Term.ClusterID, f.remote.Trust().MemberID(), request.TLS)
					if err == nil {
						if mode == "command" {
							err = f.s.MemberCommand(ctx, original, func(ctx context.Context, tx *sqlite.CommandTx) error {
								return tx.PutSection(ctx, "dedup", replication.Entry{Key: "http-own-commit", Data: []byte(`{}`)})
							})
						} else {
							_, err = original.ReadImage()
						}
					}
					if err == nil {
						err = original.WithOutput(func(ctx context.Context) error {
							status, body := http.StatusOK, []byte(`{"original":true}`)
							if mode == "304" {
								status, body = http.StatusNotModified, nil
							}
							return emitMemberHTTP(ctx, w, status, body)
						})
					}
					outcomes <- err
					select {
					case <-release:
					case <-ctx.Done():
					}
				}))
				response, err := client.Get(server.URL)
				if err != nil {
					t.Fatal("actual flushed headers unavailable before handler return", err)
				}
				if response.ProtoMajor != map[bool]int{false: 1, true: 2}[http2] {
					t.Fatal("actual protocol failed", response.Proto)
				}
				// Flush must expose every protected byte (or the304 headers)
				// before handler return. HTTP/2 END_STREAM follows return; waiting
				// for EOF here would deliberately deadlock that separate framing.
				var body []byte
				if mode != "304" {
					body = make([]byte, len(`{"original":true}`))
					if _, err := io.ReadFull(response.Body, body); err != nil {
						t.Fatal("protected bytes not flushed before handler return", err)
					}
				}
				if mode == "304" {
					if response.StatusCode != http.StatusNotModified || len(body) != 0 {
						t.Fatal("actual304 was not flushed", response.StatusCode, string(body))
					}
				} else if response.StatusCode != http.StatusOK || string(body) != `{"original":true}` {
					t.Fatal("small protected body was not flushed", response.StatusCode, string(body))
				}
				if err := <-outcomes; err != nil {
					t.Fatal("original output scope refused actual emission", err)
				}
				once.Do(func() { close(release) })
				tail, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil || len(tail) != 0 {
					t.Fatal("response did not finish cleanly after return", err, len(tail))
				}
			})
		}
	}
}

type heldMemberFlush struct {
	http.ResponseWriter
	entered chan<- struct{}
	release <-chan struct{}
	ctx     context.Context
}

func (w heldMemberFlush) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w heldMemberFlush) FlushError() error {
	close(w.entered)
	select {
	case <-w.release:
		return http.NewResponseController(w.ResponseWriter).Flush()
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func TestMemberRequestActualHTTPFlushExcludesRevocation(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			f := newMemberFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			outcomes := make(chan error, 1)
			server, client := memberHTTP(t, f, http2, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
				defer cancel()
				original, err := f.s.CaptureMemberRequest(ctx, f.s.Authority().Term.ClusterID, f.remote.Trust().MemberID(), request.TLS)
				if err == nil {
					_, err = original.ReadImage()
				}
				if err == nil {
					err = original.WithOutput(func(ctx context.Context) error {
						return emitMemberHTTP(ctx, heldMemberFlush{ResponseWriter: w, entered: entered, release: release, ctx: ctx}, http.StatusOK, []byte("protected"))
					})
				}
				outcomes <- err
			}))
			clientResult := make(chan error, 1)
			go func() {
				response, err := client.Get(server.URL)
				if err == nil {
					body, readErr := io.ReadAll(response.Body)
					_ = response.Body.Close()
					err = readErr
					if response.StatusCode != http.StatusOK || string(body) != "protected" {
						err = errors.New("wrong protected response")
					}
				}
				clientResult <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("actual HTTP did not enter original flush")
			}
			started, revoked := make(chan struct{}), make(chan error, 1)
			go func() { close(started); revoked <- f.revoke(t) }()
			<-started
			select {
			case err := <-revoked:
				t.Fatal("revocation crossed actual outstanding flush", err)
			case <-time.After(25 * time.Millisecond):
			}
			once.Do(func() { close(release) })
			if err := <-outcomes; err != nil {
				t.Fatal(err)
			}
			if err := <-clientResult; err != nil {
				t.Fatal(err)
			}
			if err := <-revoked; err != nil {
				t.Fatal(err)
			}
		})
	}
}
