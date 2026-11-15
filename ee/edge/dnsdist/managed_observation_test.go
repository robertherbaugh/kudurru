package dnsdist

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func TestManagedBypassAuthenticatedSupervisorObservation(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Options.ManagedCacheBypass = true })
	h.waitFor("installed original child", h.ready)
	h.sup.mu.Lock()
	id := h.sup.configState.bypassID
	h.sup.mu.Unlock()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "edge-b"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, EdgeBypass: h.sup.ManagedBypass})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	client := internalapi.NewClient(creds, internalapi.ClientOptions{})
	first, err := client.EdgeBypass(t.Context(), ln.Addr().String(), "edge-b", id)
	if err != nil || len(first.Backends) != 1 || first.Backends[0].Address != h.src.initial[0].Address || first.Run == "" {
		t.Fatal("actual observer lost installed membership", err)
	}
	first.Backends[0].Address = "foreign.example:53"
	second, err := client.EdgeBypass(t.Context(), ln.Addr().String(), "edge-b", id)
	if err != nil || second.Backends[0].Address != h.src.initial[0].Address || second.Challenge == first.Challenge {
		t.Fatal("observation shared mutable membership or receipt", err)
	}
	if _, err := client.EdgeBypass(t.Context(), ln.Addr().String(), "edge-b", strings.Repeat("f", 64)); err == nil {
		t.Fatal("foreign deployment entered original observer")
	}
	h.sup.noteResolverSource(nil, true)
	if _, err := client.EdgeBypass(t.Context(), ln.Addr().String(), "edge-b", id); err == nil {
		t.Fatal("lost source retained installed evidence")
	}
	h.sup.noteResolverSource(h.src.initial, false)
	h.waitFor("new source acknowledgment", h.ready)
	third, err := client.EdgeBypass(t.Context(), ln.Addr().String(), "edge-b", id)
	if err != nil || third.Run != second.Run || third.SourceEpoch <= second.SourceEpoch {
		t.Fatal("source ABA reused old evidence", err)
	}
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if _, err := h.sup.ManagedBypass(expired, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("observer renewed original caller lifetime", err)
	}
	h.stop()
	if _, err := client.EdgeBypass(t.Context(), ln.Addr().String(), "edge-b", id); err == nil {
		t.Fatal("stopped child retained remote observation")
	}
}
