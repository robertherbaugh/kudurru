//go:build unix

package persistence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type memberServerDomain struct{ executions atomic.Int32 }

func (*memberServerDomain) Authorize(ctx context.Context, _ replica.CommandTx, _ replica.Command) (context.Context, error) {
	// This fixture exercises the actual transaction boundary. Production domain
	// policy and external dependency pins remain the root caller's obligation.
	return ctx, nil
}
func (d *memberServerDomain) Execute(ctx context.Context, tx replica.CommandTx, _ replica.Command) (replica.Outcome, error) {
	if err := tx.State().(*sqlite.CommandTx).PutSection(ctx, "dedup", replication.Entry{Key: "member-server-effect", Data: []byte(`{"changed":true}`)}); err != nil {
		return replica.Outcome{}, err
	}
	d.executions.Add(1)
	return replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)}, nil
}
func memberCommand(f memberFixture) replica.Command {
	now := time.Now().UTC()
	return replica.Command{Format: replica.CommandFormat, Version: 1, ID: "member-server-command", ExpectedTerm: f.s.Authority().Term, Hops: 1, IssuedAt: now, NotAfter: now.Add(time.Minute), Operation: "fixture.change", Subject: replica.SubjectRef{Kind: auth.KindUser, ID: "alice", Provider: "local", CredentialKind: "session", CredentialID: "original-session", IssuedAt: now.Add(-time.Minute)}, Payload: json.RawMessage(`{"change":true}`)}
}
func memberCommandHandler(f memberFixture, d *memberServerDomain, after func() error) replica.MemberCommandHandler {
	return func(ctx context.Context, original *sqlite.MemberRequest, command replica.Command, emit func(replica.Outcome) error) error {
		service := replica.CommandService{Backend: replica.SQLiteMemberAtomic{Store: f.s, Request: original}, Domain: d}
		out, err := service.Execute(ctx, command)
		if err != nil {
			return err
		}
		if after != nil {
			if err := after(); err != nil {
				return err
			}
		}
		return emit(out)
	}
}
func memberServer(t *testing.T, f memberFixture, handler replica.MemberCommandHandler) (*replica.MemberServer, *internalapi.Credentials) {
	t.Helper()
	credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{19}, 32), NodeName: "member-server-test"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := replica.NewMemberServer(replica.MemberServerOptions{Store: f.s, Credentials: credentials, Execute: handler, Timeout: 4 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return server, credentials
}
func memberHTTPRequest(t *testing.T, f memberFixture, credentials *internalapi.Credentials, url, method, path string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credentials.Token())
	req.Header.Set(replica.HeaderMember, f.remote.Trust().MemberID())
	req.Header.Set(replica.HeaderCluster, f.s.Authority().Term.ClusterID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestMemberServerActualImage304AndSmallCommandFlush(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, mode := range []string{"image", "304", "command"} {
			t.Run(fmt.Sprintf("http2=%t/%s", http2, mode), func(t *testing.T) {
				f := newMemberFixture(t)
				domain := &memberServerDomain{}
				server, credentials := memberServer(t, f, memberCommandHandler(f, domain, nil))
				release, returned := make(chan struct{}), make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(release) })
				live, client := memberHTTP(t, f, http2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					server.Handler().ServeHTTP(w, r)
					close(returned)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}))
				method, path := http.MethodGet, replica.PathImage
				var requestBody, want []byte
				status := http.StatusOK
				originalImage := image(t, f.s)
				switch mode {
				case "command":
					method, path = http.MethodPost, replica.PathWrite
					requestBody, _ = json.Marshal(memberCommand(f))
					want, _ = json.Marshal(replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)})
				case "304":
					status = http.StatusNotModified
				}
				req := memberHTTPRequest(t, f, credentials, live.URL, method, path, requestBody)
				if mode == "304" {
					req.Header.Set("If-None-Match", originalImage.Generation().ETag())
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatal("actual mounted headers not flushed", err)
				}
				defer func() { _ = response.Body.Close() }()
				if response.StatusCode != status || response.ProtoMajor != map[bool]int{false: 1, true: 2}[http2] {
					t.Fatal("wrong mounted status/protocol", response.StatusCode, response.Proto)
				}
				if response.Header.Get(replica.HeaderMember) != f.local.Trust().MemberID() || response.Header.Get(replica.HeaderTerm) == "" {
					t.Fatal("missing original authority metadata")
				}
				if mode == "image" {
					var got replication.Image
					if err := json.NewDecoder(response.Body).Decode(&got); err != nil || !got.Generation().Same(originalImage.Generation()) || !reflect.DeepEqual(got.Payload, originalImage.Payload) {
						t.Fatal("complete original image not flushed before handler return", err)
					}
				} else {
					got := make([]byte, len(want))
					if _, err := io.ReadFull(response.Body, got); err != nil || !bytes.Equal(got, want) {
						t.Fatal("protected body not flushed before handler return", err)
					}
				}
				select {
				case <-returned:
				case <-time.After(3 * time.Second):
					t.Fatal("mount did not finish its scoped flush")
				}
				once.Do(func() { close(release) })
				if tail, err := io.ReadAll(response.Body); err != nil || len(tail) != 0 {
					t.Fatal("response framing did not complete", err)
				}
				if mode == "command" && domain.executions.Load() != 1 {
					t.Fatal("actual command did not commit once")
				}
			})
		}
	}
}

type memberBodyInterposition struct {
	io.ReadCloser
	once   sync.Once
	before func()
}

func (b *memberBodyInterposition) Read(p []byte) (int, error) {
	b.once.Do(b.before)
	return b.ReadCloser.Read(p)
}

func TestMemberServerRevocationBeforeCommandAndAfterCommitRefusesOutput(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, mode := range []string{"during-body", "after-commit", "proposal-only", "image-only", "missing-emission"} {
			t.Run(fmt.Sprintf("http2=%t/%s", http2, mode), func(t *testing.T) {
				f := newMemberFixture(t)
				domain := &memberServerDomain{}
				var interposed atomic.Bool
				revoke := func() error { interposed.Store(true); return f.revoke(t) }
				var after func() error
				if mode == "after-commit" {
					after = revoke
				}
				handler := memberCommandHandler(f, domain, after)
				if mode == "proposal-only" {
					handler = func(_ context.Context, _ *sqlite.MemberRequest, _ replica.Command, emit func(replica.Outcome) error) error {
						return emit(replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)})
					}
				}
				if mode == "image-only" {
					handler = func(_ context.Context, r *sqlite.MemberRequest, _ replica.Command, emit func(replica.Outcome) error) error {
						if _, err := r.ReadImage(); err != nil {
							return err
						}
						return emit(replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)})
					}
				}
				if mode == "missing-emission" {
					handler = func(ctx context.Context, r *sqlite.MemberRequest, _ replica.Command, _ func(replica.Outcome) error) error {
						return f.s.MemberCommand(ctx, r, func(context.Context, *sqlite.CommandTx) error { return nil })
					}
				}
				server, credentials := memberServer(t, f, handler)
				live, client := memberHTTP(t, f, http2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "during-body" {
						r.Body = &memberBodyInterposition{ReadCloser: r.Body, before: func() {
							if err := revoke(); err != nil {
								t.Error(err)
							}
						}}
					}
					server.Handler().ServeHTTP(w, r)
				}))
				body, _ := json.Marshal(memberCommand(f))
				response, err := client.Do(memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body))
				if response != nil {
					_ = response.Body.Close()
				}
				if err == nil {
					t.Fatal("refused/unknown completion became a response", response.StatusCode)
				}
				want := int32(0)
				if mode == "after-commit" {
					want = 1
				}
				if domain.executions.Load() != want {
					t.Fatal("wrong actual command count", domain.executions.Load())
				}
				if (mode == "during-body" || mode == "after-commit") && !interposed.Load() {
					t.Fatal("revocation target not reached")
				}
				payload := image(t, f.s).Payload
				effect := false
				for _, entry := range payload.Dedup {
					if entry.Key == "member-server-effect" {
						effect = true
					}
				}
				if effect != (mode == "after-commit") {
					t.Fatal("response refusal changed durable outcome", effect)
				}
			})
		}
	}
}

func TestMemberServerFlushExcludesRevocation(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			f := newMemberFixture(t)
			server, credentials := memberServer(t, f, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			live, client := memberHTTP(t, f, http2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
				defer cancel()
				server.Handler().ServeHTTP(heldMemberFlush{ResponseWriter: w, entered: entered, release: release, ctx: ctx}, r)
			}))
			request := memberHTTPRequest(t, f, credentials, live.URL, http.MethodGet, replica.PathImage, nil)
			request.Header.Set("If-None-Match", image(t, f.s).Generation().ETag())
			done := make(chan error, 1)
			go func() {
				r, err := client.Do(request)
				if err == nil {
					_, err = io.Copy(io.Discard, r.Body)
					_ = r.Body.Close()
					if r.StatusCode != 304 {
						err = errors.New("missing original304")
					}
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("mount never entered flush")
			}
			revoked := make(chan error, 1)
			go func() { revoked <- f.revoke(t) }()
			select {
			case err := <-revoked:
				t.Fatal("revocation crossed actual mounted flush", err)
			case <-time.After(25 * time.Millisecond):
			}
			once.Do(func() { close(release) })
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-revoked; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMemberServerRejectsUnsupportedDeadlineBeforeBodyOrCommand(t *testing.T) {
	f := newMemberFixture(t)
	domain := &memberServerDomain{}
	server, credentials := memberServer(t, f, memberCommandHandler(f, domain, nil))
	requestBody, _ := json.Marshal(memberCommand(f))
	request := memberHTTPRequest(t, f, credentials, "https://example.test", http.MethodPost, replica.PathWrite, requestBody)
	request.TLS = &f.tls
	read := false
	request.Body = &memberBodyInterposition{ReadCloser: request.Body, before: func() { read = true }}
	response := httptest.NewRecorder() // deliberately has no socket deadline support
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || read || domain.executions.Load() != 0 {
		t.Fatal("unsupported transport executed work", response.Code, read)
	}
}

type memberDeadlineWriter struct {
	http.ResponseWriter
	readErr, writeErr error
	deadlines         *[]time.Time
}

func (w memberDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w memberDeadlineWriter) SetReadDeadline(at time.Time) error {
	if w.readErr != nil {
		return w.readErr
	}
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(at)
}
func (w memberDeadlineWriter) SetWriteDeadline(at time.Time) error {
	if w.deadlines != nil {
		*w.deadlines = append(*w.deadlines, at)
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(at)
}

func TestMemberServerOriginalAdmissionPrecedesBody(t *testing.T) {
	for _, mode := range []string{"revoked", "wrong-member", "wrong-cluster", "missing-authorization", "duplicate-authorization", "read-deadline", "write-deadline", "no-command-owner"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			domain := &memberServerDomain{}
			handler := memberCommandHandler(f, domain, nil)
			if mode == "no-command-owner" {
				handler = nil
			}
			server, credentials := memberServer(t, f, handler)
			var reads atomic.Int32
			live, client := memberHTTP(t, f, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Body = &memberBodyInterposition{ReadCloser: r.Body, before: func() { reads.Add(1) }}
				if mode == "read-deadline" {
					w = memberDeadlineWriter{ResponseWriter: w, readErr: errors.New("read deadline unavailable")}
				}
				if mode == "write-deadline" {
					w = memberDeadlineWriter{ResponseWriter: w, writeErr: errors.New("write deadline unavailable")}
				}
				server.Handler().ServeHTTP(w, r)
			}))
			if mode == "revoked" {
				if err := f.revoke(t); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(memberCommand(f))
			req := memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body)
			switch mode {
			case "wrong-member":
				req.Header.Set(replica.HeaderMember, f.local.Trust().MemberID())
			case "wrong-cluster":
				req.Header.Set(replica.HeaderCluster, "foreign-cluster")
			case "missing-authorization":
				req.Header.Del("Authorization")
			case "duplicate-authorization":
				req.Header.Add("Authorization", "Bearer "+credentials.Token())
			}
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode < 400 || reads.Load() != 0 || domain.executions.Load() != 0 || response.Header.Get(replica.HeaderTerm) != "" || response.Header.Get(replica.HeaderMember) != "" {
				t.Fatal("refusal read body, ran command or emitted authority", response.StatusCode, reads.Load())
			}
		})
	}
}

func TestMemberServerCommandExpiryAndCancellationCannotEmitCommittedResult(t *testing.T) {
	for _, mode := range []string{"expired-before-commit", "expired-after-commit", "canceled-after-commit", "output-short-write", "output-flush-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			domain := &memberServerDomain{}
			command := memberCommand(f)
			original := make(chan context.CancelFunc, 1)
			attempts := make(chan error, 1)
			execute := func(ctx context.Context, r *sqlite.MemberRequest, c replica.Command, emit func(replica.Outcome) error) error {
				if mode == "expired-before-commit" {
					<-ctx.Done()
				}
				service := replica.CommandService{Backend: replica.SQLiteMemberAtomic{Store: f.s, Request: r}, Domain: domain}
				out, err := service.Execute(ctx, c)
				if err == nil {
					if mode == "expired-after-commit" {
						<-ctx.Done()
					}
					if mode == "canceled-after-commit" {
						(<-original)()
					}
					err = emit(out)
				}
				attempts <- err
				return err
			}
			server, credentials := memberServer(t, f, execute)
			live, client := memberHTTP(t, f, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				original <- cancel
				if mode == "output-short-write" || mode == "output-flush-error" {
					w = memberFailingOutput{ResponseWriter: w, short: mode == "output-short-write"}
				}
				server.Handler().ServeHTTP(w, r.WithContext(ctx))
			}))
			if mode == "expired-before-commit" || mode == "expired-after-commit" {
				command.NotAfter = time.Now().Add(time.Second)
			}
			body, _ := json.Marshal(command)
			response, err := client.Do(memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body))
			if err == nil && response != nil {
				_, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			if err == nil {
				t.Fatal("failed final output produced clean success")
			}
			if err := <-attempts; err == nil {
				t.Fatal("no actual command/output refusal")
			}
			want := int32(1)
			if mode == "expired-before-commit" {
				want = 0
			}
			if domain.executions.Load() != want {
				t.Fatal("wrong actual command count", domain.executions.Load())
			}
		})
	}
}

type memberFailingOutput struct {
	http.ResponseWriter
	short bool
}

func (w memberFailingOutput) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w memberFailingOutput) Write(b []byte) (int, error) {
	if w.short {
		return w.ResponseWriter.Write(b[:1])
	}
	return w.ResponseWriter.Write(b)
}
func (w memberFailingOutput) FlushError() error { return errors.New("original transport flush failed") }

func TestMemberServerOutputDeadlineCannotExtendCommandOrRequest(t *testing.T) {
	f := newMemberFixture(t)
	domain := &memberServerDomain{}
	command := memberCommand(f)
	command.NotAfter = time.Now().Add(2 * time.Second)
	server, credentials := memberServer(t, f, memberCommandHandler(f, domain, nil))
	observed := make(chan []time.Time, 1)
	live, client := memberHTTP(t, f, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var deadlines []time.Time
		server.Handler().ServeHTTP(memberDeadlineWriter{ResponseWriter: w, deadlines: &deadlines}, r)
		observed <- deadlines
	}))
	body, _ := json.Marshal(command)
	response, err := client.Do(memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatal("actual original command failed", err, response.StatusCode)
	}
	deadlines := <-observed
	if len(deadlines) != 3 || deadlines[1].After(deadlines[0]) || deadlines[2].After(deadlines[1]) || deadlines[2].After(command.NotAfter) {
		t.Fatal("transport renewed an original bound", deadlines)
	}
}

func TestMemberServerActualMemberClientImageDedupAndUnknownCompletion(t *testing.T) {
	for _, mode := range []string{"success", "revoked-after-commit"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			domain := &memberServerDomain{}
			var after func() error
			if mode == "revoked-after-commit" {
				after = func() error { return f.revoke(t) }
			}
			server, credentials := memberServer(t, f, memberCommandHandler(f, domain, after))
			live, _ := memberHTTP(t, f, true, server.Handler())
			peer := replica.Peer{MemberID: f.local.Trust().MemberID(), ClusterID: f.s.Authority().Term.ClusterID, Address: strings.TrimPrefix(live.URL, "https://"), Trust: f.local.Trust()}
			client, err := replica.NewMemberClient(replica.ClientOptions{Credentials: credentials, Observer: f.s, Self: func() replica.Peer {
				return replica.Peer{MemberID: f.remote.Trust().MemberID(), ClusterID: peer.ClusterID}
			}}, f.remote)
			if err != nil {
				t.Fatal(err)
			}
			before := image(t, f.s)
			source, err := replica.NewSource(client, replica.SourceOptions{Resolve: func(context.Context) (replica.Peer, error) { return peer, nil }, Validation: replication.Validation{SchemaVersion: before.StoreSchemaVersion, Kinds: before.KindManifest}})
			if err != nil {
				t.Fatal(err)
			}
			first, err := source.Fetch(t.Context(), "")
			if err != nil || first.Image == nil || !first.Image.Generation().Same(before.Generation()) {
				t.Fatal("actual member client could not accept enrolled image", err)
			}
			second, err := source.Fetch(t.Context(), first.ETag)
			if err != nil || !second.NotModified {
				t.Fatal("actual member client could not accept original304", err)
			}
			command := memberCommand(f)
			out, err := client.Write(t.Context(), peer, command)
			if domain.executions.Load() != 1 {
				t.Fatal("original command not executed once")
			}
			if mode == "revoked-after-commit" {
				if !errors.Is(err, replica.ErrOutcomeUnknown) || out.Status != 0 {
					t.Fatal("lost acknowledgment erased actual unknown completion", err)
				}
				return
			}
			if err != nil || out.Status != 200 {
				t.Fatal("actual member client refused original successful command", err)
			}
			again, err := client.Write(t.Context(), peer, command)
			if err != nil || again.Status != 200 || domain.executions.Load() != 1 {
				t.Fatal("explicit same command did not use actual durable dedup", err)
			}
		})
	}
}

func TestMemberServerOneSlotCoversBodyThroughFlush(t *testing.T) {
	f := newMemberFixture(t)
	_, credentials := memberServer(t, f, nil)
	domain := &memberServerDomain{}
	server, err := replica.NewMemberServer(replica.MemberServerOptions{Store: f.s, Credentials: credentials, Execute: memberCommandHandler(f, domain, nil), Timeout: 4 * time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var requestCount, reads atomic.Int32
	live, client := memberHTTP(t, f, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		r.Body = &memberBodyInterposition{ReadCloser: r.Body, before: func() { reads.Add(1) }}
		if n == 1 {
			w = heldMemberFlush{ResponseWriter: w, entered: entered, release: release, ctx: r.Context()}
		}
		server.Handler().ServeHTTP(w, r)
	}))
	first := memberHTTPRequest(t, f, credentials, live.URL, http.MethodGet, replica.PathImage, nil)
	first.Header.Set("If-None-Match", image(t, f.s).Generation().ETag())
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(first)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("original request did not reach flush")
	}
	body, _ := json.Marshal(memberCommand(f))
	second, err := client.Do(memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body))
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable || reads.Load() != 0 || domain.executions.Load() != 0 {
		t.Fatal("active emission released concurrency slot", second.StatusCode, reads.Load())
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	third, err := client.Do(memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, third.Body)
	_ = third.Body.Close()
	if err != nil || third.StatusCode != 200 || domain.executions.Load() != 1 {
		t.Fatal("finished emission did not release slot", err)
	}
}
