// Package testport hands out loopback ports to tests that must write a
// listener address into a configuration (or onto a child process command
// line) before the server binds it.
//
// The old pattern, listen on port 0, read the port, close, then bind the
// number later, races: between the close and the bind the kernel may hand
// the same number to any other socket in the machine that asks for an
// ephemeral port (a listener on port 0, or the local end of an outgoing
// connection), and another test may get it from the same helper. Mounted
// test processes run about a hundred tests each and several processes run
// at once, so the hosted runs saw `bind: address already in use`.
//
// Reserve removes both halves of that race:
//
//   - It allocates only from a window outside the kernel's ephemeral port
//     range (net.ipv4.ip_local_port_range on Linux), so the kernel never
//     picks a reserved number for a port-0 bind or an outgoing connection.
//   - Every reservation holds an exclusive advisory lock (flock) on a file
//     named after the port in a directory shared by all test processes of
//     the user, plus an in-process table, until the test's cleanup runs. No
//     other reservation, in this process or another, can hand the same port
//     out while the test (and the server it starts) may still bind it.
//
// Before a port is handed out it is probed by binding it for TCP and UDP on
// 127.0.0.1, so a port that something else already holds (a fixed-port
// service, a server that outlived its test) is skipped. This is test-only
// code: production listeners are unchanged.
package testport

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Host is the loopback address the reserved ports are probed on.
const Host = "127.0.0.1"

// EnvDir overrides the shared lock directory. Every process that should
// see the others' reservations must use the same directory.
const EnvDir = "KUDURRU_TESTPORT_DIR"

// windowFloor is the lowest port handed out. The repository's tests use a
// few fixed ports below it (5300, 18080, 19090 and similar).
const windowFloor = 20000

// minWindow is the smallest non-ephemeral window used before falling back.
const minWindow = 1000

var (
	mu     sync.Mutex
	held   = map[int]bool{}
	cursor = -1

	windowOnce       sync.Once
	windowLo         int
	windowHi         int
	windowOutsideEph bool
)

// Reserve returns a loopback port that is free for both TCP and UDP and
// stays reserved for this test until its cleanup runs. Register the
// cleanup that stops the server after calling Reserve (the usual order),
// so the server is gone before the reservation ends.
func Reserve(t testing.TB) int {
	t.Helper()
	port, release, err := reserve()
	if err != nil {
		t.Fatalf("testport: %v", err)
	}
	t.Cleanup(release)
	return port
}

// Address returns "127.0.0.1:<port>" for a port from Reserve.
func Address(t testing.TB) string {
	t.Helper()
	return net.JoinHostPort(Host, strconv.Itoa(Reserve(t)))
}

// Window reports the port window Reserve allocates from and whether it lies
// outside the kernel's ephemeral port range (false only on a host whose
// ephemeral range leaves no usable window; reservations are then still
// exclusive among callers of this package, but the kernel may pick a
// reserved number for an unrelated port-0 socket).
func Window() (lo, hi int, outsideEphemeral bool) {
	windowOnce.Do(func() { windowLo, windowHi, windowOutsideEph = window() })
	return windowLo, windowHi, windowOutsideEph
}

func reserve() (int, func(), error) {
	lo, hi, _ := Window()
	dir := os.Getenv(EnvDir)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "kudurru-testport")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // G703: the test-only lock directory (temp dir or KUDURRU_TESTPORT_DIR)
		return 0, nil, err
	}
	n := hi - lo + 1
	mu.Lock()
	defer mu.Unlock()
	if cursor < 0 {
		// Processes start at independent offsets and then walk the window,
		// so a process reuses a number only after it has cycled through
		// all of them.
		cursor = rand.IntN(n) //nolint:gosec // G404: spreading test ports, not a secret
	}
	for i := range n {
		port := lo + (cursor+i)%n
		if held[port] {
			continue
		}
		unlock, ok, err := lockPort(filepath.Join(dir, strconv.Itoa(port)+".lock"))
		if err != nil {
			return 0, nil, err
		}
		if !ok {
			continue
		}
		if !bindable(port) {
			unlock()
			continue
		}
		held[port] = true
		cursor = (cursor + i + 1) % n
		var once sync.Once
		return port, func() {
			once.Do(func() {
				mu.Lock()
				defer mu.Unlock()
				delete(held, port)
				unlock()
			})
		}, nil
	}
	return 0, nil, fmt.Errorf("no free port in %d-%d", lo, hi)
}

// bindable reports whether both a TCP listener and a UDP socket can bind the
// port on the loopback address right now.
func bindable(port int) bool {
	address := net.JoinHostPort(Host, strconv.Itoa(port))
	l, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	defer func() { _ = l.Close() }()
	c, err := net.ListenPacket("udp", address)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// window picks [windowFloor, ephemeral low - 1] or [ephemeral high + 1,
// 65535], whichever is usable, preferring the lower one.
func window() (int, int, bool) {
	ephLo, ephHi := ephemeralRange()
	if ephLo-1-windowFloor+1 >= minWindow {
		return windowFloor, ephLo - 1, true
	}
	if 65535-(ephHi+1)+1 >= minWindow {
		return max(ephHi+1, windowFloor), 65535, true
	}
	return windowFloor, windowFloor + 9999, false
}

// ephemeralRange returns the kernel's ephemeral port range: the Linux
// sysctl when readable, otherwise the IANA range that macOS and the BSDs
// use by default.
func ephemeralRange() (int, int) {
	if runtime.GOOS == "linux" {
		if lo, hi, err := readRange("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
			return lo, hi
		}
		return 32768, 60999
	}
	return 49152, 65535
}

func readRange(path string) (int, int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: fixed procfs path
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		return 0, 0, errors.New("empty port range")
	}
	fields := strings.Fields(s.Text())
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("port range %q", s.Text())
	}
	lo, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, err
	}
	hi, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, err
	}
	return lo, hi, nil
}
