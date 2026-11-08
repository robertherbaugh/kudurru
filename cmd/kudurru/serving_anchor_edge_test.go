package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func anchorEdgePeer(t *testing.T, name, identity string, observe func(context.Context, string) (internalapi.EdgeBypassState, error)) (anchorEdgeRequirement, *internalapi.Client) {
	t.Helper()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{5}, 32), NodeName: name})
	if err != nil {
		t.Fatal(err)
	}
	s, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, EdgeBypass: observe})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return anchorEdgeRequirement{node: name, address: ln.Addr().String(), deployment: identity, backend: anchorEdgeBackend()}, internalapi.NewClient(creds, internalapi.ClientOptions{})
}

func anchorEdgeBackend() internalapi.EdgeBypassBackend {
	return internalapi.EdgeBypassBackend{Name: "backend-a", Address: "192.0.2.1:5353", InternalAddress: "192.0.2.1:8082"}
}

func anchorEdgeState(id string) internalapi.EdgeBypassState {
	return internalapi.EdgeBypassState{Deployment: id, Run: strings.Repeat("c", 64), ChildEpoch: 1, SourceEpoch: 1, Backends: []internalapi.EdgeBypassBackend{anchorEdgeBackend()}}
}

func TestAnchorEdgeFleetOriginalAndComplete(t *testing.T) {
	var calls atomic.Int32
	observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
		calls.Add(1)
		return anchorEdgeState(id), nil
	}
	first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
	second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), observe)
	fleet := []anchorEdgeRequirement{first, second}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, err := captureAnchorEdgeAttempt(ctx, t.Context(), client, "backend-a", fleet)
	if err != nil {
		t.Fatal(err)
	}
	fleet[1].address = "192.0.2.99:8085" // caller mutation cannot redirect acquisition
	result, err := a.observe()
	if err != nil || result.check() != nil || calls.Load() != 2 || len(result.states) != 2 {
		t.Fatal("complete original fleet was not retained", err, calls.Load())
	}
	if _, err := a.observe(); err == nil || calls.Load() != 2 {
		t.Fatal("original attempt replayed")
	}
	copied := *result
	if copied.check() == nil {
		t.Fatal("equal copied fields substituted issued observation")
	}
	cancel()
	if !errors.Is(result.check(), context.Canceled) {
		t.Fatal("observation outlived original attempt")
	}
}

func TestAnchorEdgeFleetRefusesBeforeAnyAcquisition(t *testing.T) {
	var calls atomic.Int32
	first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
		calls.Add(1)
		return anchorEdgeState(id), nil
	})
	for _, name := range []string{"empty", "duplicate-node", "duplicate-address", "duplicate-deployment", "wrong-backend", "invalid-address", "oversized", "unbounded", "overlong"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			other := first
			other.node, other.address, other.deployment = "edge-b", "192.0.2.2:8085", strings.Repeat("b", 64)
			fleet := []anchorEdgeRequirement{first, other}
			switch name {
			case "empty":
				fleet = nil
			case "duplicate-node":
				fleet[1].node = first.node
			case "duplicate-address":
				fleet[1].address = first.address
			case "duplicate-deployment":
				fleet[1].deployment = first.deployment
			case "wrong-backend":
				fleet[1].backend.Name = "other-backend"
			case "invalid-address":
				fleet[1].address = "https://other/path"
			case "oversized":
				fleet = make([]anchorEdgeRequirement, 33)
			case "unbounded":
				ctx = t.Context()
			case "overlong":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(t.Context(), time.Minute)
				defer stop()
			}
			if _, err := captureAnchorEdgeAttempt(ctx, t.Context(), client, "backend-a", fleet); err == nil {
				t.Fatal("invalid fleet captured")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid fleet contacted a partial set of peers")
	}
}

func TestAnchorEdgeFleetPartialAndMembershipFailures(t *testing.T) {
	for _, name := range []string{"unavailable", "wrong-dns", "wrong-internal", "wrong-name"} {
		t.Run(name, func(t *testing.T) {
			var firstCalls, lastCalls atomic.Int32
			first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
				firstCalls.Add(1)
				return anchorEdgeState(id), nil
			})
			second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
				o := anchorEdgeState(id)
				switch name {
				case "unavailable":
					return o, errors.New("original child unavailable")
				case "wrong-dns":
					o.Backends[0].Address = "192.0.2.9:5353"
				case "wrong-internal":
					o.Backends[0].InternalAddress = "192.0.2.9:8082"
				case "wrong-name":
					o.Backends[0].Name = "another-backend"
				}
				return o, nil
			})
			last, _ := anchorEdgePeer(t, "edge-c", strings.Repeat("d", 64), func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
				lastCalls.Add(1)
				return anchorEdgeState(id), nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, err := captureAnchorEdgeAttempt(ctx, t.Context(), client, "backend-a", []anchorEdgeRequirement{first, second, last})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := a.observe(); err == nil || result != nil || a.result.Load() != nil || firstCalls.Load() != 1 || lastCalls.Load() != 0 {
				t.Fatal("partial/foreign fleet supplied evidence", err)
			}
			if _, err := a.observe(); err == nil || firstCalls.Load() != 1 {
				t.Fatal("refusal replayed earlier peers")
			}
		})
	}
}

func TestAnchorEdgeFleetRuntimeCancellationJoinsBody(t *testing.T) {
	entered := make(chan struct{})
	first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), func(ctx context.Context, _ string) (internalapi.EdgeBypassState, error) {
		close(entered)
		<-ctx.Done()
		return internalapi.EdgeBypassState{}, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	life, closeLife := context.WithCancel(t.Context())
	defer closeLife()
	a, err := captureAnchorEdgeAttempt(ctx, life, client, "backend-a", []anchorEdgeRequirement{first})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := a.observe(); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("peer acquisition did not start")
	}
	closeLife()
	select {
	case err := <-done:
		if err == nil || a.result.Load() != nil {
			t.Fatal("runtime close retained fleet evidence")
		}
	case <-time.After(time.Second):
		t.Fatal("runtime close did not interrupt and join original acquisition")
	}
}
