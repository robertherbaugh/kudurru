package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type operationTLSProbe struct {
	*integrationProbe
	client   *http.Client
	url      string
	requests atomic.Int32
}

func (p *operationTLSProbe) Check(ctx context.Context) (integration.Status, error) {
	for range p.requests.Load() {
		if err := p.host.Request(ctx, "fixture", "original", false, func(ctx context.Context) error {
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
			if err != nil {
				return err
			}
			response, err := p.client.Do(r)
			if err != nil {
				return err
			}
			defer func() { _ = response.Body.Close() }()
			_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
			if response.StatusCode != http.StatusNoContent {
				return ih.ErrProvider
			}
			return err
		}); err != nil {
			return integration.Status{}, err
		}
	}
	return integration.Status{Reachable: true, Authenticated: true}, nil
}

type integrationOperationFixture struct {
	s                    *servingRuntime
	d                    *public.Integration
	path, token, tokenID string
	adapter              *servinghttp.Adapter
	probe                *operationTLSProbe
	calls                atomic.Int32
	offset               atomic.Int64
	remoteAction         atomic.Pointer[func()]
	stop                 func()
}

func newIntegrationOperationFixture(t *testing.T) *integrationOperationFixture {
	t.Helper()
	f := &integrationOperationFixture{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.calls.Add(1)
		if action := f.remoteAction.Load(); action != nil {
			(*action)()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	f.probe = &operationTLSProbe{integrationProbe: &integrationProbe{observe: func(context.Context, ih.OwnerHost) (ih.SourceSnapshot, error) {
		return ih.SourceSnapshot{Complete: true}, nil
	}}, client: server.Client(), url: server.URL}
	f.probe.requests.Store(1)
	f.s, f.path = integrationFixture(t, func(o *servingRuntimeOptions) {
		o.Now = func() time.Time { return time.Now().Add(time.Duration(f.offset.Load())) }
	}, func(string) (integration.Integration, error) { return f.probe, nil })
	integrationOperationCaller(t, f)
	integrationOperationStart(t, f, integrationDefinition("manual", f.path))
	return f
}

func (f *integrationOperationFixture) run(t *testing.T, verb string, fn func(*integrationOperation)) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://kudurru.test/api/v1/integration-operation", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	called := false
	f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		op, err := f.s.integrations.beginOperation(r.Context(), integrationOwner(f.s, f.d), f.d.ResourceVersion, verb)
		if err != nil {
			t.Fatalf("initial operation admission: %v", err)
		}
		defer op.close()
		fn(op)
	})).ServeHTTP(w, r)
	if !called {
		t.Fatalf("original HTTP authentication did not reach operation: %d", w.Code)
	}
}

func TestIntegrationOperationActualTLSRequestsAcrossBudgetPublications(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	f.probe.requests.Store(2)
	before := f.calls.Load()
	var held *integrationOperation
	f.run(t, "check", func(op *integrationOperation) {
		held = op
		checked, err := op.capture.Check(op.ctx)
		if err != nil || !checked.Authenticated || !checked.Reachable {
			t.Fatalf("two original requests: %v", err)
		}
		if err := op.finish(nil); err != nil {
			t.Fatalf("final operation acknowledgment: %v", err)
		}
	})
	if f.calls.Load()-before != 2 {
		t.Fatal("outbound request replay or missing request")
	}
	if err := held.finish(nil); !errors.Is(err, ih.ErrInvalid) {
		t.Fatal("finished operation reused")
	}
	if err := held.check(held.ctx); err == nil {
		t.Fatal("released HTTP operation retained authority")
	}
}

func TestIntegrationOperationOriginalSourceAndProofRefusals(t *testing.T) {
	for _, mode := range []string{"credential-rotation", "revoked-between-requests", "original-expiry-with-extended-row", "final-expiry", "writer-stop"} {
		t.Run(mode, func(t *testing.T) {
			f := newIntegrationOperationFixture(t)
			f.probe.requests.Store(2)
			before := f.calls.Load()
			f.run(t, "check", func(op *integrationOperation) {
				switch mode {
				case "credential-rotation":
					if err := os.WriteFile(f.path, []byte("replaced-test-credential"), 0600); err != nil {
						t.Fatal(err)
					}
				case "writer-stop":
					f.stop()
				case "revoked-between-requests", "original-expiry-with-extended-row":
					remoteAction := func() {
						err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
							key := store.Key{Kind: "APIToken", Name: f.tokenID}
							if mode == "revoked-between-requests" {
								return tx.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem})
							}
							var token internalv1.APIToken
							if err := tx.Get(ctx, key, &token); err != nil {
								return err
							}
							expires := metav1.NewTime(time.Now().Add(24 * time.Hour))
							token.Spec.ExpiresAt = &expires
							_, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: token.ResourceVersion})
							return err
						})
						if err != nil {
							t.Error("remote interleaving failed", err)
						}
						if mode == "original-expiry-with-extended-row" {
							f.offset.Store(int64(2 * time.Hour))
						}
					}
					f.remoteAction.Store(&remoteAction)
				}
				_, err := op.capture.Check(op.ctx)
				want := int32(0)
				if mode == "revoked-between-requests" || mode == "original-expiry-with-extended-row" {
					want = 1
				}
				if mode == "final-expiry" {
					want = 2
					if err != nil {
						t.Fatal("valid outbound check refused", err)
					}
					f.offset.Store(int64(2 * time.Hour))
				} else if err == nil {
					t.Fatal("changed original authority admitted provider check")
				}
				if err := op.finish(nil); err == nil {
					t.Fatal("failed original operation acknowledged")
				}
				if f.calls.Load()-before != want {
					t.Fatal("refused operation sent unexpected outbound requests", f.calls.Load()-before, want)
				}
			})
		})
	}
}

func TestIntegrationOperationSourceCommitAndFinalStatus(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	f.run(t, "sync", func(op *integrationOperation) {
		result, err := op.capture.Sync(op.ctx, "")
		if err != nil || result == nil || result.Source == nil || !result.Source.Committed || !result.Source.Published {
			t.Fatalf("original source sync: %v", err)
		}
		if op.status.Load() == nil {
			t.Fatal("manual status was not retained for final acknowledgment")
		}
		if err := op.finish(nil); err != nil {
			t.Fatalf("final own-status transaction: %v", err)
		}
	})
}

func integrationOperationCaller(t *testing.T, f *integrationOperationFixture) {
	t.Helper()
	f.token, f.tokenID = tokens.Generate()
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "operation-admin"}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		expires := metav1.NewTime(time.Now().Add(time.Hour))
		_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: f.tokenID}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(f.token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:operation-admin", Provider: "local"}, Scopes: []string{"admin"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func integrationOperationStart(t *testing.T, f *integrationOperationFixture, definition *public.Integration) {
	t.Helper()

	f.d = integrationApply(t, f.s, definition)
	f.stop = integrationRun(t, f.s)
	integrationReadyCommand(t, f.s, integrationOwner(f.s, f.d))
	// Status changes the public resource version. Review the settled original
	// before creating the single HTTP request; never retry a refused operation.
	awaitServingRuntime(t, f.s, func() bool {
		var current public.Integration
		if f.s.opts.Store.Get(t.Context(), store.KeyOf(f.d), &current) != nil {
			return false
		}
		if _, err := f.s.integrations.host.CaptureCommand(t.Context(), integrationOwner(f.s, &current), current.ResourceVersion); err != nil {
			return false
		}
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		if lease.Bundle().Identity().Base.Revision != f.s.publisher.Status().Authority.Installed.Revision {
			return false
		}
		f.d = current.DeepCopy()
		return true
	})
	var err error
	f.adapter, err = servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.s.opts.Now})
	if err != nil {
		t.Fatal(err)
	}
}
