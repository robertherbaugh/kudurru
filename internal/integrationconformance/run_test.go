package integrationconformance

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func inputFile(t *testing.T, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tlsTarget(t *testing.T, h http.Handler) (*httptest.Server, Config) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	ca := inputFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	return s, Config{URL: s.URL, CAFile: ca, Timeout: 5 * time.Second, RequestTimeout: time.Second}
}

func externalRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", ExternalDNSMedia)
	switch r.URL.Path {
	case "/":
		_, _ = io.WriteString(w, `{"include":["example.test"]}`)
	case "/records", "/adjustendpoints":
		_, _ = io.WriteString(w, `[]`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestConformanceActualExternalDNS(t *testing.T) {
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	zone := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: "example.test.", Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns.example.test."}}}
	if _, err := st.Apply(t.Context(), zone, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	token := inputFile(t, []byte("explicit-test-token"))
	server, err := externaldns.New(externaldns.Options{Store: st, Validator: v, Namespace: "external-dns", TokenFile: token, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var applies atomic.Int32
	_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/records" {
			applies.Add(1)
		}
		server.ServeHTTP(w, r)
	}))
	c.BearerFile = token
	report, err := Run(t.Context(), c)
	if err != nil || !report.Passed || report.Complete || applies.Load() != 0 {
		t.Fatal("read qualification failed or mutation was invented", err)
	}
	c.ChangesFile = inputFile(t, []byte(`{"create":[{"dnsName":"probe.example.test","targets":["192.0.2.5"],"recordType":"A","recordTTL":60}]}`))
	if _, err := Run(t.Context(), c); err == nil || applies.Load() != 0 {
		t.Fatal("missing mutation opt-in did not refuse")
	}
	c.AllowMutations = true
	report, err = Run(t.Context(), c)
	if err != nil || !report.Passed || !report.Complete || applies.Load() != 1 {
		t.Fatal("explicit apply failed", err)
	}
	eps, err := server.Records(t.Context())
	if err != nil || len(eps) != 1 || eps[0].DNSName != "probe.example.test" {
		t.Fatal("actual store effect missing")
	}
	// This is a separately selected explicit probe, not automatic cleanup.
	c.ChangesFile = inputFile(t, []byte(`{"delete":[{"dnsName":"probe.example.test","recordType":"A"}]}`))
	report, err = Run(t.Context(), c)
	if err != nil || !report.Complete || applies.Load() != 2 {
		t.Fatal("identity-only explicit deletion failed", err)
	}
	eps, err = server.Records(t.Context())
	if err != nil || len(eps) != 0 {
		t.Fatal("explicit deletion effect missing")
	}
}

const tierDeclaration = `{"filters":[],"capabilities":["records","clients","hooks","actions"],"manifest":{"type":"test","displayName":"Test","version":"1","capabilities":["RecordSink","ClientSource","Hook","Actions"],"ioClass":"local_push","configSchema":{}}}`

func TestConformanceTierBOriginalInputs(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	// The provider's own signed body shape, frozen for v0.2 (D379).
	event, err := json.Marshal(integration.Event{Topic: "zone.changed", Time: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"public":true}`)})
	if err != nil || string(event) != `{"topic":"zone.changed","time":"2026-11-02T00:00:00Z","payload":{"public":true}}` {
		t.Fatalf("event wire format changed: %s %v", event, err)
	}
	tokenFile := inputFile(t, []byte("original-test-token"))
	eventFile := inputFile(t, event)
	var events, actions, changes atomic.Int32
	_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer original-test-token" {
			t.Error("captured explicit token replaced")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", IntegrationMedia)
		switch r.URL.Path {
		case "/":
			if err := os.WriteFile(tokenFile, []byte("replacement-token"), 0600); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(eventFile, []byte(`{}`), 0600); err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, tierDeclaration)
		case "/records":
			if r.Method == http.MethodPost {
				changes.Add(1)
				w.WriteHeader(http.StatusNoContent)
			} else {
				_, _ = io.WriteString(w, `[]`)
			}
		case "/adjustendpoints", "/clients", "/networks":
			_, _ = io.WriteString(w, `[]`)
		case "/actions":
			_, _ = io.WriteString(w, `[{"name":"probe","verb":"integrations.act","params":{"type":"object"}}]`)
		case "/events":
			events.Add(1)
			b, _ := io.ReadAll(r.Body)
			mac := hmac.New(sha256.New, key)
			_, _ = mac.Write(event)
			id, err := hex.DecodeString(r.Header.Get("X-Kudurru-Delivery"))
			if !bytes.Equal(b, event) || r.Header.Get("X-Kudurru-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || err != nil || len(id) != 16 {
				t.Error("original event/signature/delivery mismatch")
			}
			w.WriteHeader(http.StatusNoContent)
		case "/actions/probe":
			actions.Add(1)
			b, _ := io.ReadAll(r.Body)
			if string(b) != `{"probe":true}` {
				t.Error("parameters changed")
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	c.BearerFile = tokenFile
	c.AllowMutations = true
	c.EventFile = eventFile
	c.HMACFile = inputFile(t, key)
	c.ChangesFile = inputFile(t, []byte(`{}`))
	c.Action = "probe"
	c.ParamsFile = inputFile(t, []byte(`{"probe":true}`))
	report, err := Run(t.Context(), c)
	if err != nil || !report.Passed || !report.Complete || events.Load() != 1 || actions.Load() != 1 || changes.Load() != 1 {
		t.Fatal("Tier-B explicit probes failed", err)
	}
	b, _ := json.Marshal(report)
	for _, secret := range []string{"original-test-token", "replacement-token", "example.test", "probe\":true"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("report disclosed peer/input data")
		}
	}
}

func TestConformanceNoMutationReplay(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusServiceUnavailable, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var writes, redirects atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirects.Add(1); w.WriteHeader(http.StatusNoContent) }))
			defer destination.Close()
			_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/records" {
					externalRead(w, r)
					return
				}
				writes.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
			}))
			c.AllowMutations = true
			c.ChangesFile = inputFile(t, []byte(`{}`))
			report, err := Run(t.Context(), c)
			if err == nil || report.Passed || report.Complete || writes.Load() != 1 || redirects.Load() != 0 || report.Checks[len(report.Checks)-1].Reason != "effect_uncertain" {
				t.Fatal("mutation replay or false completion")
			}
		})
	}
}

func TestConformanceRefusesProtocol(t *testing.T) {
	for _, test := range []struct{ name, body, media string }{
		{"duplicate", `{"include":[],"Include":[]}`, ExternalDNSMedia},
		{"unknown", `{"include":[],"extra":true}`, ExternalDNSMedia},
		{"trailing", `{} {}`, ExternalDNSMedia},
		{"version", `{}`, "application/external.dns.webhook+json;version=2"},
		{"charset", `{}`, ExternalDNSMedia + ";charset=utf-8"},
		{"unknown-capability", strings.Replace(tierDeclaration, `"hooks"`, `"unknown"`, 1), IntegrationMedia},
		{"mismatch-manifest", strings.Replace(tierDeclaration, `"Hook"`, `"ClientSource"`, 1), IntegrationMedia},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", test.media)
				_, _ = io.WriteString(w, test.body)
			}))
			report, err := Run(t.Context(), c)
			if err == nil || report.Passed || calls.Load() != 1 {
				t.Fatal("malformed declaration admitted")
			}
		})
	}
}

func TestConformanceDeadlinesAndCancellation(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancel"}[cancelCaller], func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				w.Header().Set("Content-Type", ExternalDNSMedia)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer close(release)
			c.Timeout = 100 * time.Millisecond
			c.RequestTimeout = c.Timeout
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelCaller {
				go func() {
					select {
					case <-entered:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			report, err := Run(ctx, c)
			if err == nil || report.Passed || report.Complete {
				t.Fatal("incomplete body outlived deadline")
			}
			if cancelCaller && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation lost")
			}
		})
	}
}

func TestConformanceTrustBoundsAndInputs(t *testing.T) {
	var calls atomic.Int32
	s, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); externalRead(w, r) }))
	c.CAFile = ""
	if _, err := Run(t.Context(), c); err == nil || calls.Load() != 0 {
		t.Fatal("untrusted TLS accepted")
	}
	for _, raw := range []string{"http://example.test", "https://user@example.test", "https://example.test/?query=1", "https://example.test/#fragment", "https://example.test/%2fescape", "https://example.test/../path"} {
		c.URL = raw
		if _, err := Run(t.Context(), c); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
	c.URL = s.URL
	c.Timeout = 10 * time.Minute
	if _, err := Run(t.Context(), c); err == nil {
		t.Fatal("unbounded duration accepted")
	}
	if validJSON([]byte(strings.Repeat("[", 34)+"0"+strings.Repeat("]", 34))) || validEndpoints([]byte(`null`)) {
		t.Fatal("depth or null list accepted")
	}
	var array strings.Builder
	array.WriteByte('[')
	for i := 0; i <= MaxItems; i++ {
		if i > 0 {
			array.WriteByte(',')
		}
		array.WriteString(`{}`)
	}
	array.WriteByte(']')
	if validJSON([]byte(array.String())) {
		t.Fatal("collection bound ignored")
	}
	if validChanges([]byte(`{"create":[],"Create":[]}`)) || validChanges([]byte(`{"updateOld":[{"dnsName":"x","recordType":42}]}`)) {
		t.Fatal("ambiguous change accepted")
	}
}

func TestConformanceBodyLimit(t *testing.T) {
	_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "33554433")
		w.Header().Set("Content-Type", ExternalDNSMedia)
		w.WriteHeader(http.StatusOK)
	}))
	report, err := Run(t.Context(), c)
	if err == nil || report.Passed || report.Checks[0].Reason != "body_limit" {
		t.Fatal("body bound ignored")
	}
}

func TestConformanceMTLSAndPrefix(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "explicit-client"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("fixture roots")
	}
	var calls atomic.Int32
	s := httptest.NewUnstartedServer(http.StripPrefix("/webhook", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("unverified client")
		}
		externalRead(w, r)
	})))
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	s.StartTLS()
	defer s.Close()
	c := Config{URL: s.URL + "/webhook", CAFile: inputFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))}
	if _, err := Run(t.Context(), c); err == nil || calls.Load() != 0 {
		t.Fatal("missing client identity accepted")
	}
	c.CertFile = inputFile(t, certPEM)
	c.KeyFile = inputFile(t, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	report, err := Run(t.Context(), c)
	if err != nil || !report.Passed || report.Complete || calls.Load() != 3 {
		t.Fatal("explicit mTLS/prefix failed", err)
	}
}

func TestConformanceStopsEffectsAfterUncertainty(t *testing.T) {
	var mutations atomic.Int32
	_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", IntegrationMedia)
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, tierDeclaration)
		case "/actions":
			_, _ = io.WriteString(w, `[{"name":"probe","verb":"integrations.act","params":{}}]`)
		default:
			if r.Method == http.MethodPost && r.URL.Path != "/adjustendpoints" {
				mutations.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				_, _ = io.WriteString(w, `[]`)
			}
		}
	}))
	c.AllowMutations = true
	c.ChangesFile = inputFile(t, []byte(`{}`))
	c.Action = "probe"
	c.ParamsFile = inputFile(t, []byte(`{}`))
	c.EventFile = inputFile(t, []byte(`{"Topic":"probe","Time":"2026-11-02T00:00:00Z","Payload":{}}`))
	c.HMACFile = inputFile(t, bytes.Repeat([]byte{0x21}, 32))
	report, err := Run(t.Context(), c)
	if err == nil || report.Passed || mutations.Load() != 1 {
		t.Fatal("later effect continued after uncertainty")
	}
}

func TestConformanceNullInventory(t *testing.T) {
	const declaration = `{"filters":[],"capabilities":["clients"],"manifest":{"type":"test","displayName":"Test","version":"1","capabilities":["ClientSource"],"ioClass":"local_polling","configSchema":{}}}`
	for _, path := range []string{"/clients", "/networks"} {
		t.Run(path, func(t *testing.T) {
			_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", IntegrationMedia)
				switch r.URL.Path {
				case "/":
					_, _ = io.WriteString(w, declaration)
				case path:
					_, _ = io.WriteString(w, `[null]`)
				default:
					_, _ = io.WriteString(w, `[]`)
				}
			}))
			report, err := Run(t.Context(), c)
			if err == nil || report.Passed || report.Complete {
				t.Fatal("null inventory was certified")
			}
		})
	}
}

func TestConformanceOriginalCallerDeadlineAcrossRequests(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	_, c := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/adjustendpoints" {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		externalRead(w, r)
	}))
	defer close(release)
	c.Timeout = time.Minute
	c.RequestTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	report, err := Run(ctx, c)
	if !errors.Is(err, context.DeadlineExceeded) || report.Passed || calls.Load() != 3 {
		t.Fatal("earlier original deadline or successful prerequisites lost")
	}
}

func TestConformanceCaptureCancellationBoundsOutstandingWork(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := capture(ctx, func() (*runner, error) { close(entered); <-release; return nil, nil })
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("blocked capture did not return cancellation")
	}
	second, cancelSecond := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelSecond()
	var enteredAgain bool
	if _, err := capture(second, func() (*runner, error) { enteredAgain = true; return nil, nil }); !errors.Is(err, context.DeadlineExceeded) || enteredAgain {
		t.Fatal("another blocked capture was admitted")
	}
	close(release)
	third, cancelThird := context.WithTimeout(t.Context(), time.Second)
	defer cancelThird()
	if _, err := capture(third, func() (*runner, error) { return nil, nil }); err != nil {
		t.Fatal("finished capture retained admission", err)
	}
}
