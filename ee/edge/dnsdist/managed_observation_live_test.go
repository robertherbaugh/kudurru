package dnsdist

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

// Called only from the opt-in pinned daemon acceptance, after actual console
// acknowledgment. This adds the authenticated exchange to that real child cut.
func verifyManagedLiveObservation(t *testing.T, sup *Supervisor, backend cluster.Endpoint) {
	t.Helper()
	sup.mu.Lock()
	id, run := sup.configState.bypassID, sup.configState.runID
	sup.mu.Unlock()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{9}, 32), NodeName: "edge-live"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, EdgeBypass: sup.ManagedBypass})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("actual edge observation server did not join")
		}
	}()
	observed, err := internalapi.NewClient(creds, internalapi.ClientOptions{}).EdgeBypass(ctx, ln.Addr().String(), "edge-live", id)
	if err != nil || observed.Run != run || len(observed.Backends) != 1 ||
		observed.Backends[0] != (internalapi.EdgeBypassBackend{Name: backend.Name, Address: backend.Address, InternalAddress: backend.InternalAddress}) {
		t.Fatal("authenticated actual-child observation lost original deployment/membership", err)
	}
}
