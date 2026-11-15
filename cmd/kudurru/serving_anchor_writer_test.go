package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
)

func waitAnchorWriter(t *testing.T, s *servingRuntime, wantTerminal bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		s.anchorWriter.mu.Lock()
		ready := s.anchorWriter.running != nil && (s.anchorWriter.terminal != nil) == wantTerminal
		s.anchorWriter.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("anchor writer did not reach original worker state")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAnchorWriterTerminalSurvivesCallerReplacement(t *testing.T) {
	seed, _ := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", func([]byte) []byte { return []byte{1} })
	s, original, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	before := anchorOperationOwner(original, t, s).Version()
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithCancel(original)
		done := make(chan error, 1)
		go func() { done <- s.runAnchorWriter(ctx, target, host) }()
		waitAnchorWriter(t, s, true)
		// The terminal child stays alive, so neither enclosing retry layer gets
		// an early error. Unrelated work keeps the original context and host.
		select {
		case err := <-done:
			cancel()
			t.Fatal("terminal child escaped into outer retry", err)
		default:
		}
		if original.Err() != nil || !host.Current() || reads.Load() != 1 || anchorOperationOwner(original, t, s).Version() != before {
			cancel()
			t.Fatal("terminal observation changed state, canceled owner, or reacquired", reads.Load())
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("terminal child lost original cancellation", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("terminal child did not join before host release")
		}
		if reads.Load() != 1 {
			t.Fatal("replacement caller acquired after terminal refusal")
		}
	}
	s.anchorWriter.mu.Lock()
	defer s.anchorWriter.mu.Unlock()
	if s.anchorWriter.running != nil || s.anchorWriter.terminal == nil {
		t.Fatal("caller replacement erased terminal evidence")
	}
}

func TestAnchorWriterCanceledAcquisitionJoinsBeforeNewCaller(t *testing.T) {
	seed, _ := anchorWireSignedReply(t)
	// Unlike anchorWireServer (one exchange), accept both original callers and
	// hold each response until that caller actually closes its socket.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for range 2 {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(size[:]))); err != nil {
					return
				}
				reads.Add(1)
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-joined })
	s, original, target, host := anchorOperationFixture(t, listener.Addr().String(), seed.DS)
	for attempt := int32(1); attempt <= 2; attempt++ {
		ctx, cancel := context.WithCancel(original)
		done := make(chan error, 1)
		go func() { done <- s.runAnchorWriter(ctx, target, host) }()
		deadline := time.NewTimer(3 * time.Second)
		for reads.Load() < attempt {
			select {
			case err := <-done:
				cancel()
				deadline.Stop()
				t.Fatal("acquisition stopped before request", err)
			case <-deadline.C:
				cancel()
				t.Fatal("original request not observed")
			case <-time.After(time.Millisecond):
			}
		}
		deadline.Stop()
		if _, err := s.anchorWriter.begin(ctx, target); err == nil {
			cancel()
			t.Fatal("overlapping caller borrowed unjoined host")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("canceled socket did not join", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("canceled writer retained original socket")
		}
		s.anchorWriter.mu.Lock()
		clean := s.anchorWriter.running == nil && s.anchorWriter.terminal == nil
		s.anchorWriter.mu.Unlock()
		if !clean || reads.Load() != attempt {
			t.Fatal("canceled incomplete request retained work or manufactured terminal input")
		}
	}
}

func TestAnchorWriterCompletedReceiptCancellationRemainsTerminal(t *testing.T) {
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	s, original, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	ctx, cancel := context.WithCancel(original)
	defer cancel()
	before := anchorOperationOwner(original, t, s).Version()
	actualPin := s.anchorPin
	s.anchorPin = func(ctx context.Context, fence primaryruntime.PublicationFence, _ func(currentguard.Pinned) error) error {
		return actualPin(ctx, fence, func(currentguard.Pinned) error {
			// Actual receipt authentication and dependency admission completed;
			// cancel before entering any canonical command.
			cancel()
			return ctx.Err()
		})
	}
	if err := s.runAnchorWriter(ctx, target, host); !errors.Is(err, context.Canceled) {
		t.Fatal("original cancellation lost", err)
	}
	var terminal *anchorTerminalFailure
	if !errors.As(s.anchorWriter.terminal, &terminal) || !errors.Is(terminal, context.Canceled) || reads.Load() != 1 || anchorOperationOwner(original, t, s).Version() != before {
		t.Fatal("completed receipt was converted to fresh acquisition", s.anchorWriter.terminal)
	}
	run, err := s.anchorWriter.begin(original, target)
	if err != nil {
		t.Fatal(err)
	}
	defer run.release()
	if run.acquire {
		t.Fatal("replacement caller can replay canceled completed receipt")
	}
}
