package retirement

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"
	"time"
)

// Every label below is synthetic mechanism input, not signed/durable evidence.
func testHistory() History {
	return History{ClusterID: "mechanism-test", HistoryID: "0123456789abcdef0123456789abcdef"}
}

func testMark(pure uint64) Mark {
	m := Mark{PureGeneration: pure, EnvelopeGeneration: pure + 1}
	m.DomainID[0] = 1
	binary.LittleEndian.PutUint64(m.EnvelopeDigest[:8], pure+1)
	binary.LittleEndian.PutUint64(m.PureDigest[:8], pure)
	return m
}

func testFrame(m Mark) Frame { f := Frame{Mark: m}; f.OwnerID[0] = 2; return f }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testGate(t *testing.T) *Gate {
	t.Helper()
	g, err := New(testHistory(), [32]byte{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func writeAt(ctx context.Context, t *testing.T, g *Gate, expected Mark, fn func(*WriteScope) error) error {
	t.Helper()
	a, err := g.CaptureWrite(ctx, expected)
	if err != nil {
		return err
	}
	return g.WithWrite(a, fn)
}

func testInstall(t *testing.T, g *Gate, expected, next Mark) {
	t.Helper()
	err := writeAt(testContext(t), t, g, expected, func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		if err := c.Stage(next); err != nil {
			return err
		}
		if err := c.CheckPrepared(); err != nil {
			return err
		}
		return c.Resolve(ConfirmedInstalled)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testBind(t *testing.T, g *Gate, f Frame) *Snapshot {
	t.Helper()
	var snapshot *Snapshot
	err := writeAt(testContext(t), t, g, f.Mark, func(s *WriteScope) error {
		b, err := s.PrepareBinding(f)
		if err != nil {
			return err
		}
		snapshot = b.Snapshot()
		if err := b.Apply(); err != nil {
			return err
		}
		return b.Resolve(ConfirmedPublished)
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func boundGate(t *testing.T) (*Gate, *Snapshot) {
	t.Helper()
	g := testGate(t)
	m := testMark(1)
	testInstall(t, g, Mark{}, m)
	return g, testBind(t, g, testFrame(m))
}

func readAt(ctx context.Context, t *testing.T, g *Gate, snapshot *Snapshot, fn func(ReadScope) error) error {
	t.Helper()
	a, err := g.CaptureRead(ctx, snapshot)
	if err != nil {
		return err
	}
	return g.WithRead(a, fn)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("bounded synchronization expired")
		var zero T
		return zero
	}
}

// Observe only bounded private counters under their real mutex. No production
// hook, clock override, sleep, request retry, or widened operation deadline.
func waitQueue(t *testing.T, g *Gate, readers, writers int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		g.mu.Lock()
		match := g.readN == readers && g.writeN == writers
		g.mu.Unlock()
		if match {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("bounded queue observation expired")
}

func holdReader(t *testing.T, g *Gate, snapshot *Snapshot) (chan struct{}, <-chan error) {
	t.Helper()
	ctx := testContext(t)
	a, err := g.CaptureRead(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	entered, leave, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- g.WithRead(a, func(ReadScope) error { close(entered); <-leave; return nil }) }()
	receive(t, entered)
	return leave, done
}

func TestGateWriterPreferenceFIFO(t *testing.T) {
	g, snapshot := boundGate(t)
	leave, readerDone := holdReader(t, g, snapshot)
	writes := make(chan int, 2)
	release1, release2 := make(chan struct{}), make(chan struct{})
	done1, done2 := make(chan error, 1), make(chan error, 1)
	a1, _ := g.CaptureWrite(testContext(t), testMark(1))
	a2, _ := g.CaptureWrite(testContext(t), testMark(1))
	go func() { done1 <- g.WithWrite(a1, func(*WriteScope) error { writes <- 1; <-release1; return nil }) }()
	waitQueue(t, g, 0, 1)
	go func() { done2 <- g.WithWrite(a2, func(*WriteScope) error { writes <- 2; <-release2; return nil }) }()
	waitQueue(t, g, 0, 2)
	read, _ := g.CaptureRead(testContext(t), snapshot)
	readEntered, readDone := make(chan struct{}), make(chan error, 1)
	go func() { readDone <- g.WithRead(read, func(ReadScope) error { close(readEntered); return nil }) }()
	waitQueue(t, g, 1, 2)
	close(leave)
	if err := receive(t, readerDone); err != nil {
		t.Fatal(err)
	}
	if n := receive(t, writes); n != 1 {
		t.Fatal("writer FIFO changed")
	}
	select {
	case <-readEntered:
		t.Fatal("reader overtook queued writer")
	default:
	}
	close(release1)
	if err := receive(t, done1); err != nil {
		t.Fatal(err)
	}
	if n := receive(t, writes); n != 2 {
		t.Fatal("writer FIFO changed")
	}
	select {
	case <-readEntered:
		t.Fatal("reader overtook active writer")
	default:
	}
	close(release2)
	if err := receive(t, done2); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, readDone); err != nil {
		t.Fatal(err)
	}
	receive(t, readEntered)
}

func TestGateReservedCapacityAndClose(t *testing.T) {
	g := testGate(t)
	life, err := captureLifetime(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	for range MaxReaders {
		if err := g.enter(false, life); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, MaxPendingReaders+MaxPendingWriters)
	for i := range MaxPendingReaders {
		go func() { results <- g.enter(false, life) }()
		waitQueue(t, g, i+1, 0)
	}
	if err := g.enter(false, life); !errors.Is(err, ErrCapacity) {
		t.Fatal("reader lane overflow not refused")
	}
	for i := range MaxPendingWriters {
		go func() { results <- g.enter(true, life) }()
		waitQueue(t, g, MaxPendingReaders, i+1)
	}
	if err := g.enter(true, life); !errors.Is(err, ErrCapacity) {
		t.Fatal("writer lane overflow not refused")
	}
	attempt, err := g.CaptureWrite(testContext(t), Mark{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.WithWrite(attempt, func(*WriteScope) error { t.Error("capacity refusal invoked callback"); return nil }); !errors.Is(err, ErrCapacity) {
		t.Fatal("full writer lane did not refuse original attempt")
	}
	if err := g.WithWrite(attempt, func(*WriteScope) error { return nil }); !errors.Is(err, ErrUsed) {
		t.Fatal("capacity refusal reset attempt")
	}
	g.Close()
	for range MaxPendingReaders + MaxPendingWriters {
		if err := receive(t, results); !errors.Is(err, ErrClosed) {
			t.Fatal("close did not refuse pending slot")
		}
	}
	g.mu.Lock()
	readers, readN, writeN := g.readers, g.readN, g.writeN
	g.mu.Unlock()
	if readers != MaxReaders || readN != 0 || writeN != 0 {
		t.Fatal("close forced active unlock or retained pending entries")
	}
	for range MaxReaders {
		g.release(false)
	}
	g.Close()
}

func TestGateCancelPendingAndNoCallback(t *testing.T) {
	for _, head := range []bool{true, false} {
		t.Run(map[bool]string{true: "head", false: "nonhead"}[head], func(t *testing.T) {
			g, snapshot := boundGate(t)
			leave, held := holdReader(t, g, snapshot)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			cancelled, survivor := make(chan error, 1), make(chan error, 1)
			a, _ := g.CaptureWrite(ctx, testMark(1))
			b, _ := g.CaptureWrite(testContext(t), testMark(1))
			startCancelled := func() {
				go func() {
					cancelled <- g.WithWrite(a, func(*WriteScope) error { return errors.New("canceled callback invoked") })
				}()
			}
			startSurvivor := func() { go func() { survivor <- g.WithWrite(b, func(*WriteScope) error { return nil }) }() }
			if head {
				startCancelled()
			} else {
				startSurvivor()
			}
			waitQueue(t, g, 0, 1)
			if head {
				startSurvivor()
			} else {
				startCancelled()
			}
			waitQueue(t, g, 0, 2)
			cancel()
			if err := receive(t, cancelled); !errors.Is(err, context.Canceled) {
				t.Fatal("original cancellation lost")
			}
			waitQueue(t, g, 0, 1)
			close(leave)
			if err := receive(t, held); err != nil {
				t.Fatal(err)
			}
			if err := receive(t, survivor); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGateGrantCancellationArbitration(t *testing.T) {
	for range 32 {
		g := testGate(t)
		ctx, cancel := context.WithCancel(testContext(t))
		life, err := captureLifetime(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.enter(true, life); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			err := g.enter(false, life)
			if err == nil {
				g.release(false)
			}
			done <- err
		}()
		waitQueue(t, g, 1, 0)
		ready := make(chan struct{})
		go func() { <-ready; cancel() }()
		close(ready)
		g.release(true)
		err = stopped(receive(t, done), life)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		g.mu.Lock()
		balanced := !g.writer && g.readers == 0 && g.readN == 0 && g.writeN == 0
		g.mu.Unlock()
		if !balanced {
			t.Fatal("grant/cancel double release or leak")
		}
		cancel()
	}
}

func TestGateDeadlineQueuedAndSelected(t *testing.T) {
	g, snapshot := boundGate(t)
	leave, held := holdReader(t, g, snapshot)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	a, err := g.CaptureWrite(ctx, testMark(1))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- g.WithWrite(a, func(*WriteScope) error { t.Error("expired queued callback invoked"); return nil })
	}()
	<-ctx.Done()
	if err := receive(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("original deadline category lost")
	}
	close(leave)
	if err := receive(t, held); err != nil {
		t.Fatal(err)
	}
	if err := g.WithWrite(a, func(*WriteScope) error { return nil }); !errors.Is(err, ErrUsed) {
		t.Fatal("deadline refusal reset attempt")
	}
}

func TestGateCancellationDoesNotUnlockActiveCallback(t *testing.T) {
	g, snapshot := boundGate(t)
	ctx, cancel := context.WithCancel(testContext(t))
	a, _ := g.CaptureRead(ctx, snapshot)
	entered, leave, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- g.WithRead(a, func(s ReadScope) error { close(entered); <-leave; return s.Check() }) }()
	receive(t, entered)
	cancel()
	w, _ := g.CaptureWrite(testContext(t), testMark(1))
	writerEntered, writerDone := make(chan struct{}), make(chan error, 1)
	go func() { writerDone <- g.WithWrite(w, func(*WriteScope) error { close(writerEntered); return nil }) }()
	waitQueue(t, g, 0, 1)
	select {
	case <-writerEntered:
		t.Fatal("cancellation force-unlocked callback")
	default:
	}
	close(leave)
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := receive(t, writerDone); err != nil {
		t.Fatal(err)
	}
}
