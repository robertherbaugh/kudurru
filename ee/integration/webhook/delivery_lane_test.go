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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/credentials"
)

// A peer that holds its answer past the configured timeout is a slow peer,
// not a refusal: the attempt is temporary while the host's caller is live,
// and the next host call sends the same ID, body and signature (D379).
func TestLifecycleHeldResponsePastTimeoutIsRetriedWithSameDelivery(t *testing.T) {
	key := strings.Repeat("separate-hmac-key", 3)
	id := strings.Repeat("b", 64)
	event := integration.Event{Topic: "zone.changed", Time: time.Now().UTC(), Payload: json.RawMessage(`{"zone":"example.test"}`)}
	var mu sync.Mutex
	var bodies [][]byte
	var ids, signatures []string
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
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h := &lifecycleHost{}
	p := newProvider()
	t.Cleanup(func() { _ = p.Stop(t.Context()) })
	values := map[string]string{"bearerToken": "explicit-test-token", "hmacKey": key, "ca": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
	raw, _ := json.Marshal(map[string]string{"endpoint": server.URL + "/provider", "timeout": "300ms"})
	if err := p.Provision(t.Context(), h, raw, namedResolver(func(_ context.Context, name string) (string, error) {
		if v, ok := values[name]; ok {
			return v, nil
		}
		return "", credentials.ErrDenied
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	err := p.HandleDelivery(t.Context(), id, event)
	var failure *host.Failure
	if !errors.As(err, &failure) || failure.Kind != host.FailureTemporary || t.Context().Err() != nil {
		t.Fatalf("held response classification: %v", err)
	}
	if err := p.HandleDelivery(t.Context(), id, event); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(event)
	// The frozen wire format of the signed body (D379).
	if exact := `{"topic":"zone.changed","time":"` + event.Time.Format(time.RFC3339Nano) + `","payload":{"zone":"example.test"}}`; string(want) != exact {
		t.Fatalf("event body %s, want %s", want, exact)
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(want)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatal("delivery count", len(bodies))
	}
	for i := range bodies {
		if string(bodies[i]) != string(want) || ids[i] != id || signatures[i] != signature {
			t.Fatal("retry changed the body, ID or signature")
		}
	}
	// Every attempt of a provisioned instance charges its own account.
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, account := range h.accounts {
		if account != "original" {
			t.Fatal("instance charged a shared budget account", account)
		}
	}
}

func TestTransportBudgetAndAttemptTimeoutClassification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	exhausted := &requestHost{before: func(context.Context) error { return ledger.ErrBudget }}
	tr := serverTransport(t, exhausted, server)
	_, err := tr.request(t.Context(), http.MethodPost, "/events", integrationMedia, []byte("{}"), nil)
	var failure *host.Failure
	if !errors.As(err, &failure) || failure.Kind != host.FailureRateLimited || failure.RetryAfter != 5*time.Minute {
		t.Fatalf("budget refusal: %v", err)
	}
	// serverTransport uses a one second attempt timeout; the caller lives on.
	tr = serverTransport(t, &requestHost{}, server)
	_, err = tr.request(t.Context(), http.MethodPost, "/events", integrationMedia, []byte("{}"), nil)
	if !errors.As(err, &failure) || failure.Kind != host.FailureTemporary {
		t.Fatalf("attempt timeout: %v", err)
	}
	// The caller's own deadline stays the caller's.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err = tr.request(ctx, http.MethodPost, "/events", integrationMedia, []byte("{}"), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline: %v", err)
	}
}
