//go:build unix

package internalapi

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

func TestInvitationTLSIsolatesUnknownMembersBeforeHTTP(t *testing.T) {
	create := func(id string) *memberidentity.Identity {
		t.Helper()
		i, err := memberidentity.Create(t.TempDir()+"/identity.json", id)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	primary, member, outsider := create(uuid.NewString()), create(uuid.NewString()), create(uuid.NewString())
	var legacyCalls, memberCalls, joinCalls atomic.Int32
	handler := func(c *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { c.Add(1); w.WriteHeader(204) })
	}
	listener, err := NewEnrollingListener(primary, func(context.Context) ([]memberidentity.Trust, error) {
		return []memberidentity.Trust{member.Trust()}, nil
	}, handler(&memberCalls), handler(&joinCalls))
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(CredentialOptions{Key: make([]byte, 32), NodeName: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(listener.routes(handler(&legacyCalls)))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = listener.tlsConfig(credentials.ServerTLS())
	server.StartTLS()
	defer server.Close()
	request := func(config *tls.Config, host string, want bool) {
		t.Helper()
		transport := &http.Transport{TLSClientConfig: config}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/internal/replica/image", nil)
		r.Host = host
		response, err := client.Do(r)
		if err != nil {
			if want {
				t.Fatal(err)
			}
			return
		}
		defer func() { _ = response.Body.Close() }()
		if !want || response.StatusCode != 204 {
			t.Fatalf("unexpected HTTP response: %d", response.StatusCode)
		}
	}
	join, err := primary.Trust().EnrollmentClientTLS()
	if err != nil {
		t.Fatal(err)
	}
	request(join, primary.Trust().MemberID()+memberNameSuffix, true)
	if joinCalls.Load() != 1 || memberCalls.Load() != 0 || legacyCalls.Load() != 0 {
		t.Fatal("Host selected a different privilege route")
	}
	known, _ := member.ClientTLS(primary.Trust())
	request(known, primary.Trust().EnrollmentServerName(), true)
	unknown, _ := outsider.ClientTLS(primary.Trust())
	request(unknown, "ignored", false)
	wrong, _ := outsider.Trust().EnrollmentClientTLS()
	request(wrong, "ignored", false)
	replacement := create(primary.Trust().MemberID())
	wrongCA, _ := replacement.Trust().EnrollmentClientTLS()
	request(wrongCA, "ignored", false)
	wrongName := join.Clone()
	wrongName.ServerName = outsider.Trust().EnrollmentServerName()
	request(wrongName, "ignored", false)
	request(credentials.ClientTLS(), primary.Trust().EnrollmentServerName(), true)
	if joinCalls.Load() != 1 || memberCalls.Load() != 1 || legacyCalls.Load() != 1 {
		t.Fatal("TLS refusal reached application handler")
	}

	// Retained listeners without a join handler refuse the join SNI even when
	// the independently pinned primary is otherwise correct.
	closed, err := NewEnrolledListener(primary, listener.peers, handler(&memberCalls))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closed.tlsConfig(credentials.ServerTLS()).GetConfigForClient(&tls.ClientHelloInfo{ServerName: primary.Trust().EnrollmentServerName()}); err == nil {
		t.Fatal("join enabled implicitly")
	}
}
