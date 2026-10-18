package stack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/oidc"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

type recoverySecretAPI struct {
	server      *httptest.Server
	status      atomic.Int32
	storedReads atomic.Int32
}

func newRecoverySecretAPI(t *testing.T) *recoverySecretAPI {
	t.Helper()
	a := &recoverySecretAPI{}
	a.status.Store(http.StatusServiceUnavailable)
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/configured-secret") {
			if status := int(a.status.Load()); status != http.StatusOK {
				reason := http.StatusText(status)
				switch status {
				case http.StatusServiceUnavailable:
					reason = "ServiceUnavailable"
				case http.StatusNotFound:
					reason = "NotFound"
				}
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":%q,"message":"test Secret failure","code":%d}`, reason, status)
				return
			}
		} else {
			a.storedReads.Add(1)
		}
		_, _ = w.Write([]byte(`{"kind":"Secret","apiVersion":"v1","metadata":{"namespace":"default"},"data":{"secret":"dGVzdC1vbmx5"}}`))
	}))
	t.Cleanup(a.server.Close)
	return a
}

func recoveryProviderEntry(issuer, secret string) map[string]any {
	return map[string]any{
		"id": "corp", "issuer": issuer, "clientID": "kudurru",
		"clientSecretRef": map[string]any{"name": secret, "key": "secret"},
		"redirectURL":     "http://localhost/api/v1/auth/oidc/corp/callback",
	}
}

func recoveryOptions(t *testing.T, st store.Store, wire string) Options {
	t.Helper()
	return Options{
		Config: loadConfig(t, testConfig), Store: st, Keys: keys.Keys{Current: make([]byte, 32)},
		RetryUnavailable: kube.IsUnavailable, Out: io.Discard, Logger: slog.New(slog.DiscardHandler),
		Getenv: func(k string) string {
			switch k {
			case users.EnvAdminPassword:
				return "test-password-1"
			case tokens.EnvBootstrapToken:
				return wire
			}
			return ""
		},
	}
}

func TestConfiguredOIDCUnavailableCannotShadowAndRecovers(t *testing.T) {
	api := newRecoverySecretAPI(t)
	st := authtest.Memory(t)
	shadow, err := oidc.ParseConfigEntry(recoveryProviderEntry("https://stored-issuer.example.test", "stored-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(t.Context(), shadow, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	opts := recoveryOptions(t, st, "")
	opts.Kube = &rest.Config{Host: api.server.URL}
	opts.Config.Auth.IdentityProviders = []map[string]any{recoveryProviderEntry("https://configured-issuer.example.test", "configured-secret")}
	s, err := New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s.registryPending || !s.initializePending {
		t.Fatal("configured Secret outage must defer recovery")
	}
	if len(s.activeRegistry().Providers()) != 0 || api.storedReads.Load() != 0 {
		t.Fatal("unavailable configured provider must reserve its ID without building stored trust")
	}
	if err := s.initialize(t.Context()); !kube.IsUnavailable(err) {
		t.Fatalf("expected recovery still pending: %v", err)
	}
	if (registryVerifier{s}).AcceptsIssuer(t.Context(), "https://stored-issuer.example.test") {
		t.Fatal("stored issuer became trusted during a retry")
	}
	api.status.Store(http.StatusOK)
	if err := s.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.registryPending || !(registryVerifier{s}).AcceptsIssuer(t.Context(), "https://configured-issuer.example.test") {
		t.Fatal("configured provider did not recover")
	}
	if (registryVerifier{s}).AcceptsIssuer(t.Context(), "https://stored-issuer.example.test") || api.storedReads.Load() != 0 {
		t.Fatal("stored provider overrode configured trust after recovery")
	}
}

func TestDeferredIdentityRecoveryPreservesRevocations(t *testing.T) {
	api := newRecoverySecretAPI(t)
	st := &recoveringIdentityStore{Store: authtest.Memory(t)}
	wire, id := tokens.Generate()
	opts := recoveryOptions(t, st, wire)
	opts.Kube = &rest.Config{Host: api.server.URL}
	opts.Config.Auth.IdentityProviders = []map[string]any{recoveryProviderEntry("https://configured-issuer.example.test", "configured-secret")}
	s, err := New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s.initializePending {
		t.Fatal("expected Secret recovery pending")
	}
	if err := s.Tokens().Revoke(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(t.Context(), store.Key{Kind: internalv1.KindLocalUser, Name: users.DefaultAdminName}, store.DeleteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.initialize(t.Context()); !kube.IsUnavailable(err) {
			t.Fatalf("expected configured provider still unavailable: %v", err)
		}
		assertRecoveryCredentialsAbsent(t, s, st, wire, id)
	}
	if st.watches.Load() != 0 {
		t.Fatal("watches started before initialization completed")
	}
	api.status.Store(http.StatusOK)
	runRecoveredStack(t, s, st)
	assertRecoveryCredentialsAbsent(t, s, st, wire, id)
}

func assertRecoveryCredentialsAbsent(t *testing.T, s *Stack, st store.Store, wire, id string) {
	t.Helper()
	var token internalv1.APIToken
	if err := st.Get(t.Context(), store.Key{Kind: internalv1.KindAPIToken, Name: id}, &token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token recreated: %v", err)
	}
	var user internalv1.LocalUser
	if err := st.Get(t.Context(), store.Key{Kind: internalv1.KindLocalUser, Name: users.DefaultAdminName}, &user); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted admin recreated: %v", err)
	}
	if _, ok := s.Users().Get(users.DefaultAdminName); ok {
		t.Fatal("recovery priming retained a deleted user")
	}
	if _, err := s.Tokens().Verify(t.Context(), wire, "192.0.2.1"); err == nil {
		t.Fatal("revoked credential authenticated")
	}
}

// Fail one initialization stage until recovery, and detect Prime after any
// identity watch has started. Both control fields are fixed before Run.
type recoveringIdentityStore struct {
	store.Store
	failStage       string
	available       atomic.Bool
	watches         atomic.Int32
	primeAfterWatch atomic.Bool
}

func (s *recoveringIdentityStore) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	if opts.Limit == 500 && s.watches.Load() != 0 {
		s.primeAfterWatch.Store(true)
	}
	if !s.available.Load() && ((s.failStage == "prime" && kind == internalv1.KindAPIToken && opts.Limit == 500) || (s.failStage == "user bootstrap" && kind == internalv1.KindLocalUser && opts.Limit == 1)) {
		return store.ObjectList{}, syscall.ECONNREFUSED
	}
	return s.Store.List(ctx, kind, opts)
}

func (s *recoveringIdentityStore) Get(ctx context.Context, key store.Key, into store.Object) error {
	if !s.available.Load() && s.failStage == "token bootstrap" && key.Kind == internalv1.KindServiceAccount {
		return syscall.ECONNREFUSED
	}
	return s.Store.Get(ctx, key, into)
}

func (s *recoveringIdentityStore) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (store.Watcher, error) {
	s.watches.Add(1)
	return s.Store.Watch(ctx, kind, opts)
}

func runRecoveredStack(t *testing.T, s *Stack, st *recoveringIdentityStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("identity watches did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for st.watches.Load() < 5 {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("identity exited before starting watches: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("identity recovery never started all watches")
		}
		time.Sleep(time.Millisecond)
	}
	if st.primeAfterWatch.Load() {
		t.Fatal("recovery Prime raced identity watches")
	}
}

func TestDeferredIdentityRecoveryResumesUnfinishedSteps(t *testing.T) {
	for _, stage := range []string{"prime", "user bootstrap", "token bootstrap"} {
		t.Run(stage, func(t *testing.T) {
			st := &recoveringIdentityStore{Store: authtest.Memory(t), failStage: stage}
			wire, id := tokens.Generate()
			s, err := New(t.Context(), recoveryOptions(t, st, wire))
			if err != nil {
				t.Fatal(err)
			}
			if !s.initializePending || st.watches.Load() != 0 {
				t.Fatal("unfinished initialization must defer without starting watches")
			}
			if stage == "token bootstrap" {
				// User bootstrap already succeeded, but token bootstrap did not.
				if err := st.Delete(t.Context(), store.Key{Kind: internalv1.KindLocalUser, Name: users.DefaultAdminName}, store.DeleteOptions{Force: true}); err != nil {
					t.Fatal(err)
				}
			}
			st.available.Store(true)
			runRecoveredStack(t, s, st)
			if _, ok := s.Tokens().Get(id); !ok {
				t.Fatal("unfinished token bootstrap never completed")
			}
			principal, err := s.Tokens().Verify(t.Context(), wire, "192.0.2.1")
			if err != nil || !principal.BuiltinAdmin {
				t.Fatalf("bootstrap credential did not recover: %v", err)
			}
			_, hasAdmin := s.Users().Get(users.DefaultAdminName)
			if hasAdmin != (stage != "token bootstrap") {
				t.Fatal("user bootstrap failed to resume or replayed after successful completion")
			}
		})
	}
}

func TestConfiguredOIDCRecoveryPreservesStartupErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int32
		retry  bool
	}{
		{"synchronous availability error", http.StatusServiceUnavailable, false},
		{"forbidden", http.StatusForbidden, true},
		{"missing Secret", http.StatusNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRecoverySecretAPI(t)
			api.status.Store(tc.status)
			opts := recoveryOptions(t, authtest.Memory(t), "")
			opts.Kube = &rest.Config{Host: api.server.URL}
			opts.Config.Auth.IdentityProviders = []map[string]any{recoveryProviderEntry("https://configured-issuer.example.test", "configured-secret")}
			if !tc.retry {
				opts.RetryUnavailable = nil
			}
			if _, err := New(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "clientSecretRef") {
				t.Fatalf("startup error was hidden: %v", err)
			}
			if tc.retry {
				api.status.Store(http.StatusServiceUnavailable)
				s, err := New(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				api.status.Store(tc.status)
				if err := s.Run(t.Context()); err == nil || kube.IsUnavailable(err) || !strings.Contains(err.Error(), "clientSecretRef") {
					t.Fatalf("recovery hid a permanent Secret error: %v", err)
				}
			}
		})
	}
}
