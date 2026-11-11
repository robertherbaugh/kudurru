package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// emfileListener fails every Accept the way a process out of file
// descriptors does.
type emfileListener struct{ net.Listener }

func (emfileListener) Accept() (net.Conn, error) {
	return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type discardCollector struct{}

func (discardCollector) Enqueue(*querylog.Event) bool { return true }

// TestServeDnstapIsNeverFatal: the runner shuts the node down when an
// always-on runnable returns an error, so querylog-dnstap must not. A
// listener whose every Accept fails (EMFILE) keeps retrying and logging;
// one that stops before shutdown is logged at once while the other keeps
// serving; serveDnstap returns nil when its context ends.
func TestServeDnstapIsNeverFatal(t *testing.T) {
	logs := &lockedBuffer{}
	r := &querylogRuntime{logger: slog.New(slog.NewTextHandler(logs, nil))}
	var stopped *dnstap.Listener
	for i := range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		use := net.Listener(ln)
		if i == 0 {
			use = emfileListener{ln}
		}
		l, err := dnstap.Listen(dnstap.ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + ln.Addr().String(), AllowFrom: []string{"127.0.0.1"}}, Listener: use, Collector: discardCollector{}, Logger: r.logger})
		if err != nil {
			t.Fatal(err)
		}
		r.dnstap = append(r.dnstap, l)
		stopped = l
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- r.serveDnstap(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "too many open files") {
		if time.Now().After(deadline) {
			t.Fatalf("the accept error was not logged: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = stopped.Close()
	for !strings.Contains(logs.String(), "dnstap listener stopped before shutdown") {
		if time.Now().After(deadline) {
			t.Fatalf("the stopped listener was not logged: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-served:
		t.Fatalf("serveDnstap returned before shutdown: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveDnstap returned %v; the runner would shut the node down", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveDnstap did not return at shutdown")
	}
}
