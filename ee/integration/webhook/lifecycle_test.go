package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/credentials"
)

type lifecycleHost struct{ requestHost }

func (*lifecycleHost) IntegrationOwner() host.InstanceID {
	return host.InstanceID{ClusterID: "fixture", Namespace: "dns", Name: "webhook", UID: "original"}
}

type namedResolver func(context.Context, string) (string, error)

func (f namedResolver) Resolve(ctx context.Context, name string) (string, error) { return f(ctx, name) }

func lifecycleProvider(t *testing.T, server *httptest.Server, h *lifecycleHost, extra map[string]string) (*provider, map[string]string) {
	t.Helper()
	values := map[string]string{"bearerToken": "explicit-test-token", "ca": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
	for name, value := range extra {
		values[name] = value
	}
	p := newProvider()
	t.Cleanup(func() { _ = p.Stop(t.Context()) })
	raw, err := json.Marshal(map[string]string{"endpoint": server.URL + "/provider"})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Provision(t.Context(), h, raw, namedResolver(func(ctx context.Context, name string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		v, ok := values[name]
		if !ok {
			return "", credentials.ErrDenied
		}
		return v, nil
	}))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Validate(t.Context()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return p, values
}

func TestLifecycleActualTLSCapturedActionAndPermanentDeclarationChange(t *testing.T) {
	var changed atomic.Bool
	var posts, reads atomic.Int32
	var bad atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", integrationMedia)
		if r.Header.Get("Authorization") != "Bearer explicit-test-token" {
			bad.Store(true)
		}
		switch r.URL.Path {
		case "/provider/":
			reads.Add(1)
			_ = json.NewEncoder(w).Encode(actionDeclaration())
		case "/provider/actions":
			reads.Add(1)
			name := "refresh"
			if changed.Load() {
				name = "replace"
			}
			_ = json.NewEncoder(w).Encode([]integration.ActionSpec{{Name: name, Verb: "integrations.act", Params: json.RawMessage(`{"type":"object"}`)}})
		case "/provider/actions/refresh":
			posts.Add(1)
			b, err := io.ReadAll(r.Body)
			if err != nil || string(b) != `{"original":true}` || r.Method != http.MethodPost {
				bad.Store(true)
			}
			_, _ = io.WriteString(w, `{"accepted":true}`)
		default:
			bad.Store(true)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	h := &lifecycleHost{}
	p, values := lifecycleProvider(t, server, h, nil)
	if reads.Load() != 0 || posts.Load() != 0 || len(p.Actions()) != 0 {
		t.Fatal("offline lifecycle performed I/O or exposed unchecked actions")
	}
	values["bearerToken"] = "replacement-token"
	status, err := p.Check(t.Context())
	if err != nil || !status.Authenticated || !status.Reachable {
		t.Fatalf("initial check: %v", err)
	}
	actions := p.Actions()
	actions[0].Name = "replacement"
	actions[0].Params[0] = '['
	if _, err := p.Invoke(t.Context(), "replacement", json.RawMessage(`{}`)); !errors.Is(err, host.ErrUnsupported) {
		t.Fatal("caller changed private action identity")
	}
	result, err := p.Invoke(t.Context(), "refresh", json.RawMessage(`{"original":true}`))
	if err != nil || string(result) != `{"accepted":true}` || posts.Load() != 1 || reads.Load() != 2 || bad.Load() {
		t.Fatalf("captured action: %v", err)
	}
	changed.Store(true)
	if _, err = p.Check(t.Context()); !errors.Is(err, host.ErrFenced) {
		t.Fatal("changed declaration adopted")
	}
	changed.Store(false)
	if _, err = p.Check(t.Context()); !errors.Is(err, host.ErrFenced) {
		t.Fatal("ABA declaration repaired original incarnation")
	}
	if len(p.Actions()) != 0 {
		t.Fatal("retired declaration still exposed")
	}
	if _, err = p.Invoke(t.Context(), "refresh", json.RawMessage(`{}`)); !errors.Is(err, host.ErrFenced) {
		t.Fatal("retired action executed")
	}
	if posts.Load() != 1 || reads.Load() != 4 {
		t.Fatal("retirement triggered extra peer request")
	}
}

func TestLifecycleStableSignedDeliveryIsOneAttemptPerHostCall(t *testing.T) {
	key := strings.Repeat("separate-hmac-key", 3)
	id := strings.Repeat("a", 64)
	event := integration.Event{Topic: "zone.changed", Time: time.Now().UTC(), Payload: json.RawMessage(`{"name":"example.test"}`)}
	want, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	var ids, signatures []string
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/provider/" {
			d := actionDeclaration()
			d.Capabilities = []string{"hooks"}
			d.Manifest.Capabilities = []integration.Capability{integration.CapHook}
			w.Header().Set("Content-Type", integrationMedia)
			_ = json.NewEncoder(w).Encode(d)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		ids = append(ids, r.Header.Get("X-Kudurru-Delivery"))
		signatures = append(signatures, r.Header.Get("X-Kudurru-Signature"))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h := &lifecycleHost{}
	p, values := lifecycleProvider(t, server, h, map[string]string{"hmacKey": key})
	if _, err := p.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	values["hmacKey"] = "replacement-hmac"
	topics := p.Topics()
	topics[0] = "invented"
	if !slices.Contains(p.Topics(), event.Topic) {
		t.Fatal("topics aliased caller storage")
	}
	err = p.HandleDelivery(t.Context(), id, event)
	var failure *host.Failure
	if !errors.As(err, &failure) || failure.Kind != host.FailureTemporary {
		t.Fatalf("first failure classification: %v", err)
	}
	mu.Lock()
	firstAttempts := len(bodies)
	mu.Unlock()
	if firstAttempts != 1 {
		t.Fatal("provider retried failed delivery")
	}
	// A distinct host call represents the explicitly durable at-least-once lane.
	if err := p.HandleDelivery(t.Context(), id, event); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(want)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatal("unexpected delivery count")
	}
	for i := range bodies {
		if string(bodies[i]) != string(want) || ids[i] != id || signatures[i] != wantSignature {
			t.Fatal("delivery replaced original body, ID or named key")
		}
	}
}

func TestLifecycleUnsupportedCapabilitiesAndSecretEchoRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, capability string
		manifest         integration.Capability
		echo             bool
	}{
		{"records", "records", integration.CapRecordSink, false},
		{"missing-hook-key", "hooks", integration.CapHook, false},
		{"named-secret-echo", "hooks", integration.CapHook, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := strings.Repeat("secret-material", 3)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				d := actionDeclaration()
				d.Capabilities = []string{tc.capability}
				d.Manifest.Capabilities = []integration.Capability{tc.manifest}
				if tc.echo {
					d.Manifest.DisplayName = key
				}
				w.Header().Set("Content-Type", integrationMedia)
				_ = json.NewEncoder(w).Encode(d)
			}))
			defer server.Close()
			extra := map[string]string{}
			if tc.echo {
				extra["hmacKey"] = key
			}
			p, _ := lifecycleProvider(t, server, &lifecycleHost{}, extra)
			if _, err := p.Check(t.Context()); err == nil {
				t.Fatal("unsupported or credential-bearing peer activated")
			}
			if len(p.Actions()) != 0 || len(p.Topics()) != 0 {
				t.Fatal("failed setup exposed capability")
			}
		})
	}
}

func TestLifecycleNamedResolutionFailureNeverFallsBack(t *testing.T) {
	p := newProvider()
	defer func() { _ = p.Stop(t.Context()) }()
	err := p.Provision(t.Context(), &lifecycleHost{}, json.RawMessage(`{"endpoint":"https://unused.invalid"}`), namedResolver(func(_ context.Context, name string) (string, error) {
		if name == "bearerToken" {
			return "original-token", nil
		}
		return "", errors.New("resolver unavailable")
	}))
	if !errors.Is(err, host.ErrCredentials) || p.transport != nil {
		t.Fatal("failed optional named reference fell back to another trust configuration")
	}
}

func TestLifecycleStopDuringProvisionAndWaitingCheck(t *testing.T) {
	t.Run("named-resolution", func(t *testing.T) {
		p := newProvider()
		defer func() { _ = p.Stop(t.Context()) }()
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- p.Provision(t.Context(), &lifecycleHost{}, json.RawMessage(`{"endpoint":"https://unused.invalid"}`), namedResolver(func(ctx context.Context, _ string) (string, error) {
				close(entered)
				<-ctx.Done()
				return "", ctx.Err()
			}))
		}()
		<-entered
		if err := p.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("held resolution outlived provider stop")
		}
	})
	t.Run("waiting-check", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		defer close(release)
		p, _ := lifecycleProvider(t, server, &lifecycleHost{}, nil)
		first := make(chan error, 1)
		go func() { _, err := p.Check(t.Context()); first <- err }()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := p.Check(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("queued check did not retain original cancellation")
		}
		if err := p.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-first; err == nil {
			t.Fatal("held check succeeded after stop")
		}
	})
}
