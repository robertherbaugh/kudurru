package internalapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolverObservationGuardChallengeAndUnavailable(t *testing.T) {
	var calls atomic.Int32
	var unavailable atomic.Bool
	peer := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-a"}, ServerOptions{
		Zones: func(context.Context) ZoneList { return ZoneList{Node: "node-a"} },
		ServingZones: func(context.Context) (*ZoneList, error) {
			calls.Add(1)
			if unavailable.Load() {
				return nil, errors.New("unavailable")
			}
			return &ZoneList{Node: "node-a", Resolver: &ResolverState{Node: "node-a", Version: 1}}, nil
		},
	})
	if got, err := peer.client.Zones(t.Context(), peer.addr); err != nil || got.Resolver == nil || !ValidDigest(got.ResolverNonce) {
		t.Fatal("authenticated bounded observation failed", err)
	}
	before := calls.Load()
	for _, path := range []string{PathZones, PathZones + "?resolverNonce=" + strings.Repeat("b", 64)} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		peer.server.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("unguarded projection", w.Code)
		}
	}
	bad, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	bad.tokens = [][]byte{[]byte("unaccepted-internal-test-token")}
	if _, err := NewClient(bad, ClientOptions{}).Zones(t.Context(), peer.addr); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("wrong original token accepted")
	}
	if calls.Load() != before {
		t.Fatal("unauthenticated caller reached generation owner")
	}
	unavailable.Store(true)
	if _, err := peer.client.Zones(t.Context(), peer.addr); err == nil {
		t.Fatal("failed source returned eligible metadata")
	}
}

func TestResolverObservationClientRejectsReplayAndOversize(t *testing.T) {
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 2 {
			_, _ = w.Write([]byte(strings.Repeat(" ", (4<<20)+1)))
			return
		}
		body := ZoneList{Node: "node-a", Resolver: &ResolverState{Node: "node-a", Version: 1}, ResolverNonce: r.URL.Query().Get("resolverNonce")}
		if mode.Load() == 1 {
			body.ResolverNonce = strings.Repeat("a", 64)
		}
		_ = json.NewEncoder(w).Encode(body)
		if mode.Load() == 3 {
			_, _ = w.Write([]byte("{}"))
		}
	}))
	server.TLS = creds.ServerTLS()
	server.StartTLS()
	defer server.Close()
	client := NewClient(creds, ClientOptions{})
	for _, m := range []int32{0, 1, 2, 3} {
		mode.Store(m)
		_, err := client.Zones(t.Context(), server.Listener.Addr().String())
		if (m == 0) != (err == nil) {
			t.Fatalf("mode%d bounded decoding/replay refusal incorrect", m)
		}
	}
}
