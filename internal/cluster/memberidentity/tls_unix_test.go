//go:build unix

package memberidentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func memberServer(t *testing.T, identity *Identity, peers []Trust, handler http.Handler) *httptest.Server {
	t.Helper()
	config, err := identity.ServerTLS(peers)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = config
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func memberClient(t *testing.T, identity *Identity, peer Trust) *http.Client {
	t.Helper()
	config, err := identity.ClientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: config}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestMemberTLSAuthenticatesBothIndependentMembersBeforeHTTP(t *testing.T) {
	primary, primaryPath := testIdentity(t)
	follower, _ := testIdentity(t)
	outsider, _ := testIdentity(t)
	var requests atomic.Int32
	server := memberServer(t, primary, []Trust{follower.Trust()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := follower.Trust().VerifyPeer(r.TLS); err != nil {
			t.Error("actual mutual TLS peer was not recognized", err)
			w.WriteHeader(403)
			return
		}
		if outsider.Trust().VerifyPeer(r.TLS) == nil {
			t.Error("a different claimed UUID acquired the connection")
		}
		w.WriteHeader(204)
	}))
	client := memberClient(t, follower, primary.Trust())
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 204 || primary.Trust().VerifyPeer(response.TLS) != nil || requests.Load() != 1 {
		t.Fatal("original enrolled peers failed actual HTTP")
	}
	wrongServer := memberClient(t, follower, outsider.Trust())
	if response, err := wrongServer.Post(server.URL, "text/plain", nil); err == nil {
		_ = response.Body.Close()
		t.Fatal("wrong original target trust sent an application request")
	}
	wrongClient := memberClient(t, outsider, primary.Trust())
	if response, err := wrongClient.Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("unenrolled client reached HTTP")
	}
	if requests.Load() != 1 {
		t.Fatal("TLS refusal occurred after application bytes", requests.Load())
	}
	// Reopening the actual private identity issues a fresh leaf trusted by the
	// original enrollment, without adopting a new CA or peer UUID.
	restarted, err := Load(primaryPath, primary.Trust().MemberID())
	if err != nil {
		t.Fatal(err)
	}
	second := memberServer(t, restarted, []Trust{follower.Trust()}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	response, err = client.Get(second.URL)
	if err != nil {
		t.Fatal("restart lost original trust", err)
	}
	_ = response.Body.Close()
	if primary.Trust().VerifyPeer(response.TLS) != nil {
		t.Fatal("restarted node did not preserve its original member identity")
	}
}

func TestMemberTLSRejectsSameUUIDReplacementBeforeSending(t *testing.T) {
	primary, _ := testIdentity(t)
	follower, _ := testIdentity(t)
	replacement, err := Create(t.TempDir()+"/replacement.json", primary.Trust().MemberID())
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := memberServer(t, replacement, []Trust{follower.Trust()}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	client := memberClient(t, follower, primary.Trust())
	if response, err := client.Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("matching claimed UUID replaced the original enrolled key")
	}
	if requests.Load() != 0 {
		t.Fatal("replacement peer received an application request")
	}
}

func TestMemberTLSHeldConnectionCannotRenewExpiredLeaf(t *testing.T) {
	primary, _ := testIdentity(t)
	follower, _ := testIdentity(t)
	n, err := serial()
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(3 * time.Second).Truncate(time.Second)
	template := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: memberName(follower.trust.member)}, DNSNames: []string{memberName(follower.trust.member)}, NotBefore: time.Now().Add(-time.Minute), NotAfter: end, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, follower.trust.root, pub, follower.key)
	if err != nil {
		t.Fatal(err)
	}
	short := &tls.Certificate{Certificate: [][]byte{der, follower.trust.root.Raw}, PrivateKey: key}
	captured := make(chan *tls.ConnectionState, 1)
	server := memberServer(t, primary, []Trust{follower.Trust()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.TLS
		w.WriteHeader(204)
	}))
	client := memberClient(t, follower, primary.Trust())
	client.Transport.(*http.Transport).TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return short, nil }
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	state := <-captured
	if follower.Trust().VerifyPeer(state) != nil {
		t.Fatal("initial original certificate was not valid")
	}
	time.Sleep(time.Until(end) + time.Millisecond)
	if follower.Trust().VerifyPeer(state) == nil {
		t.Fatal("held TLS state renewed an expired original certificate")
	}
}

func TestMemberTLSCannotSubstituteAnotherEnrolledMember(t *testing.T) {
	primary, _ := testIdentity(t)
	follower, _ := testIdentity(t)
	other, _ := testIdentity(t)
	var requests atomic.Int32
	server := memberServer(t, primary, []Trust{follower.Trust(), other.Trust()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Claimed-Member") != follower.Trust().MemberID() || follower.Trust().VerifyPeer(r.TLS) != nil {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(204)
	}))
	client := memberClient(t, other, primary.Trust())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Claimed-Member", follower.Trust().MemberID())
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if requests.Load() != 1 || response.StatusCode != 403 {
		t.Fatal("another enrolled member impersonated the claimed UUID")
	}
}

func TestMemberTLSRejectsAnonymousAndOlderProtocol(t *testing.T) {
	primary, _ := testIdentity(t)
	follower, _ := testIdentity(t)
	var requests atomic.Int32
	server := memberServer(t, primary, []Trust{follower.Trust()}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	for _, mode := range []string{"anonymous", "tls12"} {
		t.Run(mode, func(t *testing.T) {
			client := memberClient(t, follower, primary.Trust())
			config := client.Transport.(*http.Transport).TLSClientConfig
			if mode == "anonymous" {
				config.GetClientCertificate = nil
			} else {
				config.MinVersion, config.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
			}
			if response, err := client.Get(server.URL); err == nil {
				_ = response.Body.Close()
				t.Fatal("missing member proof or older protocol reached HTTP")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("unverified peer sent application bytes")
	}
}
