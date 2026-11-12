package testport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestReserveIsUniqueAndOutsideEphemeralRange(t *testing.T) {
	lo, hi, outside := Window()
	ephLo, ephHi := ephemeralRange()
	if outside && hi >= ephLo && lo <= ephHi {
		t.Fatalf("window %d-%d overlaps the ephemeral range %d-%d", lo, hi, ephLo, ephHi)
	}
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				port := Reserve(t)
				mu.Lock()
				if seen[port] {
					t.Errorf("port %d handed out twice", port)
				}
				seen[port] = true
				mu.Unlock()
				if port < lo || port > hi {
					t.Errorf("port %d outside window %d-%d", port, lo, hi)
				}
			}
		})
	}
	wg.Wait()
}

func TestReserveSkipsBoundPort(t *testing.T) {
	// A port that something already binds is never handed out: bind a port
	// of the window, point the cursor at it and check that Reserve passes
	// it by.
	lo, hi, _ := Window()
	var l net.Listener
	port := lo
	for ; port <= hi; port++ {
		var err error
		if l, err = net.Listen("tcp", net.JoinHostPort(Host, strconv.Itoa(port))); err == nil {
			break
		}
	}
	if l == nil {
		t.Fatalf("no bindable port in %d-%d", lo, hi)
	}
	defer func() { _ = l.Close() }()
	mu.Lock()
	saved := cursor
	cursor = port - lo
	mu.Unlock()
	defer func() { mu.Lock(); cursor = saved; mu.Unlock() }()
	if got := Reserve(t); got == port {
		t.Fatalf("Reserve handed out bound port %d", got)
	}
}

func TestReleaseAtCleanup(t *testing.T) {
	var port int
	t.Run("hold", func(t *testing.T) { port = Reserve(t) })
	mu.Lock()
	defer mu.Unlock()
	if held[port] {
		t.Fatalf("port %d still held after the subtest's cleanup", port)
	}
}

const envHolder = "KUDURRU_TESTPORT_HOLDER"

// TestHolderProcess is the child half of TestReserveExcludesOtherProcesses:
// it reserves ports, prints them and holds them until stdin closes.
func TestHolderProcess(t *testing.T) {
	count, err := strconv.Atoi(os.Getenv(envHolder))
	if err != nil {
		return // not the child
	}
	for range count {
		fmt.Println(Reserve(t))
	}
	fmt.Println("held")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// startHolder starts a child process of this test binary that reserves
// count ports and holds them until the returned function closes its stdin.
// extraEnv is appended to the parent's environment.
func startHolder(t *testing.T, count int, extraEnv ...string) (map[int]bool, func()) {
	t.Helper()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHolderProcess$", "-test.count=1") //nolint:gosec // G204: this test binary
	child.Env = append(append(os.Environ(), envHolder+"="+strconv.Itoa(count)), extraEnv...)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	theirs := map[int]bool{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() && scanner.Text() != "held" {
		port, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil {
			t.Fatalf("child output %q", scanner.Text())
		}
		theirs[port] = true
	}
	if len(theirs) != count {
		t.Fatalf("child reserved %d ports, want %d", len(theirs), count)
	}
	return theirs, func() {
		_ = stdin.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("holder process: %v", err)
		}
	}
}

// reserveAcross points the cursor at the lowest port the child holds and
// reserves count ports, failing if any of them is one of the child's.
func reserveAcross(t *testing.T, theirs map[int]bool, count int) {
	t.Helper()
	// Walk the whole neighborhood the child just took: every one of those
	// numbers is free in the kernel (nothing binds them) yet locked.
	lo, hi, _ := Window()
	mu.Lock()
	saved := cursor
	cursor = hi - lo
	for port := range theirs {
		cursor = min(cursor, port-lo)
	}
	mu.Unlock()
	defer func() { mu.Lock(); cursor = saved; mu.Unlock() }()
	for range count {
		if port := Reserve(t); theirs[port] {
			t.Fatalf("port %d reserved by both processes (window %d-%d)", port, lo, hi)
		}
	}
}

func TestReserveExcludesOtherProcesses(t *testing.T) {
	const count = 300
	theirs, stop := startHolder(t, count)
	defer stop()
	reserveAcross(t, theirs, count)
}

// TestReserveExcludesOtherProcessesAcrossTMPDIR covers tests that point
// TMPDIR at a private directory (t.Setenv before reserving, or a child
// started with its own TMPDIR): the lock directory is fixed when the
// process starts and inherited by children, so the exclusion still holds.
func TestReserveExcludesOtherProcessesAcrossTMPDIR(t *testing.T) {
	const count = 100
	theirs, stop := startHolder(t, count, "TMPDIR="+t.TempDir())
	defer stop()
	t.Run("parent TMPDIR changed", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		reserveAcross(t, theirs, count)
	})
}

// TestStress allocates ports in many goroutines, binds each one after a
// delay while other goroutines churn ephemeral sockets (port-0 listeners
// and outgoing connections), and counts `address already in use`. Run it
// in several processes at once to add cross-process pressure:
//
//	KUDURRU_TESTPORT_STRESS_N=500 go test -count=4 -run '^TestStress$' ./internal/testutil/testport
//
// KUDURRU_TESTPORT_STRESS_MODE=legacy measures the old listen-close-rebind
// helper instead; that mode only reports its collisions, because the
// old helper is expected to collide.
func TestStress(t *testing.T) {
	perWorker := 25
	if v := os.Getenv("KUDURRU_TESTPORT_STRESS_N"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		perWorker = n
	}
	legacy := os.Getenv("KUDURRU_TESTPORT_STRESS_MODE") == "legacy"
	const workers = 8
	ctx, cancel := context.WithCancel(t.Context())
	var churn sync.WaitGroup
	defer churn.Wait()
	defer cancel()
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	churn.Go(func() {
		for {
			c, err := sink.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	})
	for range 4 {
		churn.Go(func() {
			for ctx.Err() == nil {
				var open []io.Closer
				for range 32 {
					if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
						open = append(open, l)
					}
					if c, err := net.ListenPacket("udp", "127.0.0.1:0"); err == nil {
						open = append(open, c)
					}
					if c, err := net.Dial("tcp", sink.Addr().String()); err == nil {
						open = append(open, c)
					}
				}
				time.Sleep(5 * time.Millisecond)
				for _, c := range open {
					_ = c.Close()
				}
			}
		})
	}
	var allocations, collisions atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				t.Run(fmt.Sprintf("w%d-%d", w, i), func(t *testing.T) {
					var address string
					if legacy {
						address = legacyAddress(t)
					} else {
						address = Address(t)
					}
					allocations.Add(1)
					time.Sleep(time.Duration(5+(w+i)%20) * time.Millisecond)
					l, err := net.Listen("tcp", address)
					if err == nil {
						defer func() { _ = l.Close() }()
						var c net.PacketConn
						c, err = net.ListenPacket("udp", address)
						if err == nil {
							_ = c.Close()
						}
					}
					if errors.Is(err, syscall.EADDRINUSE) {
						collisions.Add(1)
						if !legacy {
							t.Errorf("collision: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
	wg.Wait()
	mode := "reserve"
	if legacy {
		mode = "legacy"
	}
	t.Logf("testport stress: pid=%d mode=%s allocations=%d collisions=%d", os.Getpid(), mode, allocations.Load(), collisions.Load())
}

// legacyAddress is the helper this package replaces: it releases the port
// before the caller binds it.
func legacyAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}
