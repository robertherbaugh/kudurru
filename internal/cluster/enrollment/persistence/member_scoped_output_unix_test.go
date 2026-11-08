//go:build unix

package persistence_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestMemberCommandOutputContextCannotBeSubstitutedOrEscape(t *testing.T) {
	f := newMemberFixture(t)
	ctx := finite(t)
	request := f.capture(ctx, t)
	foreign := f.capture(ctx, t)
	if err := f.s.MemberCommand(ctx, request, func(context.Context, *sqlite.CommandTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var retained context.Context
	if err := request.WithCommandOutput(func(scope context.Context) error {
		retained = scope
		if err := request.CheckCommandOutput(scope); err != nil {
			return err
		}
		for _, other := range []context.Context{nil, ctx, context.WithoutCancel(scope)} {
			if err := request.CheckCommandOutput(other); err == nil {
				t.Fatal("unissued/unbounded context accepted")
			}
		}
		extended, cancel := context.WithDeadline(context.WithoutCancel(scope), request.Deadline().Add(time.Second))
		defer cancel()
		if err := request.CheckCommandOutput(extended); err == nil {
			t.Fatal("extended output bound accepted")
		}
		if err := foreign.CheckCommandOutput(scope); err == nil {
			t.Fatal("foreign request accepted output context")
		}
		canceled, cancelCanceled := context.WithCancel(scope)
		cancelCanceled()
		if err := request.CheckCommandOutput(canceled); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled output accepted", err)
		}
		copyRequest := *request
		return copyRequest.CheckCommandOutput(scope)
	}); err != nil {
		t.Fatal(err)
	}
	if err := request.CheckCommandOutput(retained); err == nil {
		t.Fatal("output capability escaped callback")
	}
	if _, err := foreign.ReadImage(); err != nil {
		t.Fatal(err)
	}
	if err := foreign.WithOutput(func(scope context.Context) error {
		if err := foreign.CheckCommandOutput(scope); err == nil {
			t.Fatal("image output acknowledged a command")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemberServerScopedActualHTTPCompletionAndRefusals(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, mode := range []string{"committed", "plain-context", "image-context", "escaped-context", "rolled-back"} {
			t.Run(fmt.Sprintf("http2=%t/%s", http2, mode), func(t *testing.T) {
				f := newMemberFixture(t)
				domain := &memberServerDomain{}
				_, credentials := memberServer(t, f, nil)
				staged := errors.New("rollback staged domain and dedup")
				handler := func(ctx context.Context, request *sqlite.MemberRequest, command replica.Command, emit func(context.Context, replica.Outcome) error) error {
					out := replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)}
					if mode == "image-context" {
						if _, err := request.ReadImage(); err != nil {
							return err
						}
						return request.WithOutput(func(scope context.Context) error { return emit(scope, out) })
					}
					service := replica.CommandService{Domain: domain}
					var txAfter *sqlite.CommandTx
					err := f.s.MemberCommand(ctx, request, func(ctx context.Context, tx *sqlite.CommandTx) error {
						txAfter = tx
						var err error
						out, err = service.StageSQLite(ctx, tx, command)
						if err == nil && mode == "rolled-back" {
							return staged
						}
						return err
					})
					if _, endedErr := service.StageSQLite(ctx, txAfter, command); endedErr == nil {
						t.Error("ended transaction accepted staging")
					}
					if mode == "rolled-back" {
						if !errors.Is(err, staged) {
							t.Error("rollback target not reached", err)
						}
						// Deliberately present the staged result. Neither data nor a
						// caller context can manufacture output admission.
						return emit(ctx, out)
					}
					if err != nil {
						return err
					}
					if mode == "plain-context" {
						return emit(ctx, out)
					}
					if mode == "escaped-context" {
						var saved context.Context
						if err := request.WithCommandOutput(func(scope context.Context) error { saved = scope; return nil }); err != nil {
							return err
						}
						return emit(saved, out)
					}
					return request.WithCommandOutput(func(scope context.Context) error { return emit(scope, out) })
				}
				server, err := replica.NewMemberServer(replica.MemberServerOptions{Store: f.s, Credentials: credentials, ExecuteScoped: handler, Timeout: 4 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := replica.NewMemberServer(replica.MemberServerOptions{Store: f.s, Credentials: credentials, ExecuteScoped: handler, Execute: memberCommandHandler(f, domain, nil)}); err == nil {
					t.Fatal("ambiguous executor accepted")
				}
				live, client := memberHTTP(t, f, http2, server.Handler())
				body, _ := json.Marshal(memberCommand(f))
				response, err := client.Do(memberHTTPRequest(t, f, credentials, live.URL, http.MethodPost, replica.PathWrite, body))
				if mode == "committed" {
					if err != nil {
						t.Fatal("scoped command failed", err)
					}
					defer func() { _ = response.Body.Close() }()
					var got replica.Outcome
					if err := json.NewDecoder(response.Body).Decode(&got); err != nil || got.Status != 200 || string(got.Body) != `{"changed":true}` {
						t.Fatal("actual scoped response missing", got.Status, err)
					}
					if tail, err := io.ReadAll(response.Body); err != nil || len(tail) != 0 {
						t.Fatal("scoped response did not finish", err)
					}
				} else {
					if response != nil {
						_ = response.Body.Close()
					}
					if err == nil {
						t.Fatal("unadmitted result became an HTTP response")
					}
				}
				effect, dedup := false, false
				for _, row := range image(t, f.s).Payload.Dedup {
					if row.Key == "member-server-effect" {
						effect = true
					} else {
						dedup = true
					}
				}
				want := mode != "image-context" && mode != "rolled-back"
				if effect != want || dedup != want {
					t.Fatal("wrong actual domain/dedup completion", effect, dedup, want)
				}
			})
		}
	}
}
