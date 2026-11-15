package replica

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// These tests exercise transport interruption with a real blocked socket.
// Enrolled Store/HTTP composition is covered in enrollment/persistence.
func TestMemberEmitInterruptsCanceledSocket(t *testing.T) {
	for _, parent := range []string{"command", "output"} {
		for _, stage := range []string{"write", "flush"} {
			t.Run(parent+"/"+stage, func(t *testing.T) {
				command, cancelCommand := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancelCommand()
				output, cancelOutput := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancelOutput()
				server, client := net.Pipe()
				defer func() { _ = server.Close() }()
				defer func() { _ = client.Close() }()
				w := &memberPipeWriter{conn: server, header: make(http.Header), flush: stage == "flush",
					entered: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{})}
				finished := make(chan error, 1)
				go func() {
					started := false
					finished <- memberEmit(command, w, nil, func(fn func(context.Context) error) error {
						return fn(output)
					}, http.StatusOK, "", []byte("acknowledgment"), &started)
				}()
				select {
				case <-w.entered:
				case <-time.After(time.Second):
					_ = client.Close()
					close(w.release)
					err := <-finished
					t.Fatalf("output did not enter socket write: %v", err)
				}
				if parent == "command" {
					cancelCommand()
				} else {
					cancelOutput()
				}
				select {
				case <-w.interrupted:
				case <-time.After(time.Second):
					_ = client.Close()
					close(w.release)
					err := <-finished
					t.Fatalf("cancellation did not interrupt active socket output: %v", err)
				}
				// Both cancellation parents must share one interruption, and no
				// callback may outlive the output guard even after I/O unblocks.
				cancelCommand()
				cancelOutput()
				select {
				case err := <-finished:
					close(w.release)
					t.Fatalf("output guard escaped its cancellation callback: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				close(w.release)
				select {
				case err := <-finished:
					if err == nil {
						t.Fatal("canceled acknowledgment succeeded")
					}
				case <-time.After(time.Second):
					_ = client.Close()
					err := <-finished
					t.Fatalf("output did not finish after interrupt callback completed: %v", err)
				}
				if got := w.deadlines.Load(); got != 2 {
					t.Fatalf("deadline calls = %d, want original deadline and one interruption", got)
				}
			})
		}
	}
}

type memberPipeWriter struct {
	conn                          net.Conn
	header                        http.Header
	flush                         bool
	entered, interrupted, release chan struct{}
	deadlines                     atomic.Int32
}

func TestMemberEmitCancellationBeforeFlush(t *testing.T) {
	for _, parent := range []string{"command", "output"} {
		t.Run(parent, func(t *testing.T) {
			command, cancelCommand := context.WithTimeout(t.Context(), time.Second)
			defer cancelCommand()
			output, cancelOutput := context.WithTimeout(t.Context(), time.Second)
			defer cancelOutput()
			cancel := cancelCommand
			if parent == "output" {
				cancel = cancelOutput
			}
			w := &memberCancelWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
			started := false
			err := memberEmit(command, w, nil, func(fn func(context.Context) error) error {
				return fn(output)
			}, http.StatusOK, "", []byte("buffered acknowledgment"), &started)
			if err != context.Canceled || !started || w.Flushed {
				t.Fatalf("canceled buffered output: error=%v started=%t flushed=%t", err, started, w.Flushed)
			}
		})
	}
}

type memberCancelWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *memberCancelWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	w.cancel()
	return n, err
}

func (*memberCancelWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *memberPipeWriter) Header() http.Header { return w.header }
func (w *memberPipeWriter) WriteHeader(int)     {}
func (w *memberPipeWriter) Write(p []byte) (int, error) {
	if w.flush {
		return len(p), nil
	}
	close(w.entered)
	return w.conn.Write(p)
}
func (w *memberPipeWriter) FlushError() error {
	if !w.flush {
		return nil
	}
	close(w.entered)
	_, err := w.conn.Write([]byte("buffered acknowledgment"))
	return err
}
func (w *memberPipeWriter) SetWriteDeadline(at time.Time) error {
	n := w.deadlines.Add(1)
	err := w.conn.SetWriteDeadline(at)
	if n == 2 {
		close(w.interrupted)
		<-w.release
	}
	return err
}
