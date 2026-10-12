package internalapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// memLists is an in-memory ListOpener.
type memLists struct {
	mu     sync.Mutex
	bodies map[string][]byte // digest -> body (may deliberately not match)
	opened int
}

func (m *memLists) OpenList(digest string) (ListBody, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bodies[digest]
	if !ok {
		return ListBody{}, ErrListNotFound
	}
	m.opened++
	return ListBody{Body: io.NopCloser(bytes.NewReader(b)), Size: int64(len(b)), Source: "https://lists.example/" + digest[:8]}, nil
}

// node is one in-process kudurru node: credentials, a server on a loopback
// port and a client.
type node struct {
	creds  *Credentials
	server *Server
	addr   string
	client *Client
}

func startNode(t *testing.T, opts CredentialOptions, srv ServerOptions) *node {
	t.Helper()
	creds, err := NewCredentials(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Credentials = creds
	s, err := NewServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return &node{creds: creds, server: s, addr: ln.Addr().String(), client: NewClient(creds, ClientOptions{Timeout: 5 * time.Second})}
}

func TestDerivation(t *testing.T) {
	a1, _ := DeriveToken(key(1))
	a2, _ := DeriveToken(key(1))
	b, _ := DeriveToken(key(2))
	if a1 != a2 || a1 == b || len(a1) != 43 {
		t.Fatalf("tokens: %q %q %q", a1, a2, b)
	}
	c1, err := DeriveCACertificate(key(1))
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := DeriveCACertificate(key(1))
	c3, _ := DeriveCACertificate(key(2))
	if !bytes.Equal(c1.Raw, c2.Raw) {
		t.Error("the derived CA differs between derivations of the same key")
	}
	if bytes.Equal(c1.Raw, c3.Raw) {
		t.Error("different keys derived the same CA")
	}
	if !c1.IsCA || c1.PublicKeyAlgorithm != x509.Ed25519 || c1.Subject.CommonName != "kudurru internal CA" {
		t.Errorf("CA = %+v", c1.Subject)
	}
	// The token and the CA seed come from different HKDF infos.
	if strings.Contains(hex.EncodeToString(c1.RawSubjectPublicKeyInfo), hex.EncodeToString([]byte(a1))) {
		t.Error("token leaks into the CA")
	}
	if _, err := NewCredentials(CredentialOptions{}); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no key, no token: %v", err)
	}
	if _, err := NewCredentials(CredentialOptions{Key: []byte("short")}); err == nil {
		t.Error("short key accepted")
	}
}

func TestTwoNodesStatusAndListPull(t *testing.T) {
	body := []byte("0.0.0.0 ads.example\n0.0.0.0 tracker.example\n")
	d := digestOf(body)
	lists := &memLists{bodies: map[string][]byte{d: body}}
	leader := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-a"}, ServerOptions{
		Lists: lists,
		Status: func(context.Context) Status {
			return Status{Node: "node-a", Version: "test", Leader: "node-a", State: "Live", Lists: []ListStatus{{Name: "default/ads", Digests: []string{d}}}}
		},
	})
	replica := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-b"}, ServerOptions{})
	ctx := context.Background()

	st, err := replica.client.Status(ctx, leader.addr)
	if err != nil {
		t.Fatal(err)
	}
	if st.Node != "node-a" || st.Leader != "node-a" || len(st.Lists) != 1 || st.Lists[0].Digests[0] != d {
		t.Fatalf("status = %+v", st)
	}
	// The replica's own status lists no lists but is valid JSON.
	if st, err := leader.client.Status(ctx, replica.addr); err != nil || st.Lists == nil {
		t.Fatalf("replica status = %+v, %v", st, err)
	}

	rc, info, err := replica.client.OpenList(ctx, leader.addr, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, %v", got, err)
	}
	if info.Size != int64(len(body)) || !strings.HasPrefix(info.Source, "https://lists.example/") {
		t.Errorf("info = %+v", info)
	}

	if _, _, err := replica.client.OpenList(ctx, leader.addr, digestOf([]byte("other"))); !errors.Is(err, ErrListNotFound) {
		t.Errorf("unknown digest: %v", err)
	}
	if _, _, err := replica.client.OpenList(ctx, leader.addr, "nothex"); err == nil {
		t.Error("invalid digest accepted by the client")
	}
}

func TestDigestMismatchAndSizeCap(t *testing.T) {
	body := []byte("good.example\n")
	d := digestOf(body)
	lists := &memLists{bodies: map[string][]byte{d: []byte("evil.example\n")}}
	leader := startNode(t, CredentialOptions{Key: key(3)}, ServerOptions{Lists: lists})
	creds, _ := NewCredentials(CredentialOptions{Key: key(3)})
	ctx := context.Background()

	rc, _, err := NewClient(creds, ClientOptions{}).OpenList(ctx, leader.addr, d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered body: %v", err)
	}

	big := bytes.Repeat([]byte("a.example\n"), 1000)
	bd := digestOf(big)
	lists.mu.Lock()
	lists.bodies[bd] = big
	lists.mu.Unlock()
	if _, _, err := NewClient(creds, ClientOptions{MaxListBytes: 100}).OpenList(ctx, leader.addr, bd); !errors.Is(err, ErrTooLarge) {
		t.Errorf("client cap: %v", err)
	}
	small := startNode(t, CredentialOptions{Key: key(3)}, ServerOptions{Lists: lists, MaxListBytes: 100})
	if _, _, err := NewClient(creds, ClientOptions{}).OpenList(ctx, small.addr, bd); err == nil || !strings.Contains(err.Error(), "413") {
		t.Errorf("server cap: %v", err)
	}
}

func TestAuthentication(t *testing.T) {
	leader := startNode(t, CredentialOptions{Key: key(1)}, ServerOptions{})
	ctx := context.Background()

	// A node with another key does not trust the leader's certificate.
	other, _ := NewCredentials(CredentialOptions{Key: key(2)})
	_, err := NewClient(other, ClientOptions{}).Status(ctx, leader.addr)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("foreign key: %v", err)
	}

	// The right CA but a wrong token (token file override).
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("not-the-derived-token-at-all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongTok, err := NewCredentials(CredentialOptions{Key: key(1), TokenFile: tokFile})
	if err != nil {
		t.Fatal(err)
	}
	if wrongTok.Token() != "not-the-derived-token-at-all" {
		t.Fatalf("token file not trimmed: %q", wrongTok.Token())
	}
	if _, err := NewClient(wrongTok, ClientOptions{}).Status(ctx, leader.addr); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong token: %v", err)
	}

	// Plain HTTP and missing tokens are refused by the handler.
	rec := httptest.NewRecorder()
	leader.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathStatus, nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, PathStatus, strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+leader.creds.Token())
	rec = httptest.NewRecorder()
	leader.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("request body: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, PathLists+"ZZ", nil)
	req.Header.Set("Authorization", "bearer "+leader.creds.Token())
	rec = httptest.NewRecorder()
	leader.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad digest: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, PathStatus, nil)
	req.Header.Set("Authorization", "Bearer "+leader.creds.Token())
	rec = httptest.NewRecorder()
	leader.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestKeyRotation(t *testing.T) {
	ctx := context.Background()
	oldNode := startNode(t, CredentialOptions{Key: key(1)}, ServerOptions{})
	newNode := startNode(t, CredentialOptions{Key: key(2), PreviousKey: key(1)}, ServerOptions{})
	// old -> new: the new node's certificate is issued by the previous CA
	// and it accepts the previous token.
	if _, err := oldNode.client.Status(ctx, newNode.addr); err != nil {
		t.Errorf("old to new: %v", err)
	}
	// new -> old: the client trusts both CAs and retries with the previous
	// token after a 401.
	if _, err := newNode.client.Status(ctx, oldNode.addr); err != nil {
		t.Errorf("new to old: %v", err)
	}
	// After the rotation the old key is gone.
	done := startNode(t, CredentialOptions{Key: key(2)}, ServerOptions{})
	if _, err := done.client.Status(ctx, newNode.addr); err == nil {
		t.Error("a node without the previous key trusted a certificate from the previous CA")
	}
}

func TestRateLimitAndDownloadSlots(t *testing.T) {
	body := []byte("a.example\n")
	d := digestOf(body)
	srv := startNode(t, CredentialOptions{Key: key(4)}, ServerOptions{RateLimit: 1, Burst: 2, Lists: &memLists{bodies: map[string][]byte{d: body}}})
	ctx := context.Background()
	var limited int
	for range 5 {
		if _, err := srv.client.Status(ctx, srv.addr); err != nil {
			if !strings.Contains(err.Error(), "429") {
				t.Fatalf("unexpected error %v", err)
			}
			limited++
		}
	}
	if limited < 2 {
		t.Errorf("rate limit let %d of 5 requests through", 5-limited)
	}

	// Download slots: with every slot taken a further download gets 503.
	full, err := NewServer(ServerOptions{Credentials: srv.creds, MaxDownloads: 1, Lists: &memLists{bodies: map[string][]byte{d: body}}})
	if err != nil {
		t.Fatal(err)
	}
	full.downloads <- struct{}{}
	req := httptest.NewRequest(http.MethodGet, PathLists+d, nil)
	req.Header.Set("Authorization", "Bearer "+srv.creds.Token())
	rec := httptest.NewRecorder()
	full.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("full slots: %d", rec.Code)
	}
	<-full.downloads
	rec = httptest.NewRecorder()
	full.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) || rec.Header().Get(HeaderDigest) != d {
		t.Errorf("free slot: %d %q", rec.Code, rec.Body.String())
	}
}

// TestFileOverrides covers listeners.internal.tokenFile and tls files.
func TestFileOverrides(t *testing.T) {
	dir := t.TempDir()
	caPub, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ops CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafPub, leafPriv, _ := ed25519.GenerateKey(rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kudurru-0"},
		DNSNames:  []string{"kudurru-internal.dns.svc"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, leafPub, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafPriv)
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	opts := CredentialOptions{
		CertFile:   write("tls.crt", "CERTIFICATE", leafDER),
		KeyFile:    write("tls.key", "PRIVATE KEY", keyDER),
		CAFile:     write("ca.crt", "CERTIFICATE", caDER),
		ServerName: "kudurru-internal.dns.svc",
		TokenFile:  filepath.Join(dir, "token"),
	}
	if err := os.WriteFile(opts.TokenFile, []byte("a-token-from-a-chart-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := startNode(t, opts, ServerOptions{})
	if _, err := n.client.Status(context.Background(), n.addr); err != nil {
		t.Fatalf("file-configured node: %v", err)
	}
	if _, err := NewCredentials(CredentialOptions{CertFile: opts.CertFile, TokenFile: opts.TokenFile}); err == nil {
		t.Error("certFile without keyFile accepted")
	}
	if _, err := NewCredentials(CredentialOptions{TokenFile: opts.TokenFile}); err == nil {
		t.Error("a token file without any TLS material accepted (the listener is always TLS)")
	}
}

func TestNodeCertificateRenewal(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c, err := NewCredentials(CredentialOptions{Key: key(9), NodeName: "n1", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := c.certificate()
	if got := first.Leaf.DNSNames; len(got) != 2 || got[0] != DefaultServerName || got[1] != "n1" {
		t.Errorf("SANs = %v", got)
	}
	now = now.Add(30 * 24 * time.Hour)
	if same, _ := c.certificate(); same != first {
		t.Error("renewed too early")
	}
	now = now.Add(31 * 24 * time.Hour)
	if renewed, _ := c.certificate(); renewed == first || !renewed.Leaf.NotAfter.After(first.Leaf.NotAfter) {
		t.Error("not renewed within 30 days of expiry")
	}
}
