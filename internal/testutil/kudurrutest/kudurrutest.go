// Package kudurrutest runs the real kudurru binary for the end-to-end and
// interoperability tests under test/ (docs/m5/e2e.md): it builds the binary
// once per test package, renders a node configuration with free loopback
// ports, starts `kudurru serve` as a child process, waits for readiness and
// keeps the process log for failure reports.
//
// The end-to-end tests are opt-in: they skip unless KUDURRU_E2E is set
// (`make e2e`, `make interop`), so `go test ./...` stays fast.
package kudurrutest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// EnvE2E enables the end-to-end tests.
const EnvE2E = "KUDURRU_E2E"

// EnvBinary names a prebuilt kudurru binary to use instead of building one.
const EnvBinary = "KUDURRU_E2E_BINARY"

// RequireE2E skips the test unless KUDURRU_E2E is set.
func RequireE2E(t testing.TB) {
	t.Helper()
	if !Enabled() {
		t.Skipf("end-to-end test: set %s=1 (make e2e) to run it", EnvE2E)
	}
}

// EnvKnownBugs makes KnownBug run the assertions it would skip, to
// reproduce a recorded bug (docs/m5/e2e.md, docs/followups.md).
const EnvKnownBugs = "KUDURRU_E2E_KNOWN_BUGS"

// KnownBug skips the rest of the test because of a recorded bug unless
// KUDURRU_E2E_KNOWN_BUGS is set, in which case the test runs (and fails
// until the bug is fixed). id names the bug in docs/m5/e2e.md.
func KnownBug(t testing.TB, id, summary string) {
	t.Helper()
	if os.Getenv(EnvKnownBugs) == "" {
		t.Skipf("known bug %s: %s (set %s=1 to reproduce)", id, summary, EnvKnownBugs)
	}
	t.Logf("known bug %s: %s (reproducing)", id, summary)
}

// Enabled reports whether KUDURRU_E2E is set.
func Enabled() bool { return os.Getenv(EnvE2E) != "" }

// ModuleRoot returns the directory holding go.mod.
func ModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("kudurrutest: no caller information")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("kudurrutest: go.mod not found")
		}
		dir = parent
	}
}

// Build compiles ./cmd/kudurru into dir (CGO_ENABLED=0, as shipped) and
// returns the binary path. KUDURRU_E2E_BINARY short-circuits the build.
func Build(dir string) (string, error) {
	if p := os.Getenv(EnvBinary); p != "" {
		return filepath.Abs(p)
	}
	root, err := ModuleRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "kudurru")
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/kudurru") //nolint:gosec // fixed arguments
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("kudurrutest: go build: %w\n%s", err, b)
	}
	return out, nil
}

// MainBuild is the TestMain helper of the end-to-end packages: when
// KUDURRU_E2E is set it builds the binary into a temporary directory and
// stores its path in *bin; it then runs the tests and removes the
// directory.
func MainBuild(m *testing.M, bin *string) int {
	if !Enabled() {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "kudurru-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	start := time.Now()
	p, err := Build(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "kudurrutest: built %s in %s\n", p, time.Since(start).Round(time.Millisecond))
	*bin = p
	return m.Run()
}

// FreePort returns a loopback port that is free for both TCP and UDP at the
// time of the call.
func FreePort(t testing.TB) int {
	t.Helper()
	for range 50 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("free port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		u, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		_ = l.Close()
		if err != nil {
			continue
		}
		_ = u.Close()
		return port
	}
	t.Fatal("free port: no port free for tcp and udp")
	return 0
}

// Ports are the listener addresses of one node.
type Ports struct {
	DNS, API, Ops, Metrics, Internal string
}

// NewPorts allocates loopback addresses for every listener.
func NewPorts(t testing.TB) Ports {
	t.Helper()
	addr := func() string { return fmt.Sprintf("127.0.0.1:%d", FreePort(t)) }
	return Ports{DNS: addr(), API: addr(), Ops: addr(), Metrics: addr(), Internal: addr()}
}

// BaseConfig returns a node configuration with the given name, data
// directory and ports: sqlite store, udp+tcp DNS, plain-HTTP API, one
// socket per listener, the default upstream group pointing at the discard
// port (nothing leaves the host) and text logs. Callers add keys before
// WriteConfig.
func BaseConfig(name, dataDir string, p Ports) map[string]any {
	return map[string]any{
		"node": map[string]any{"name": name, "dataDir": dataDir},
		"listeners": map[string]any{
			"dns":      []any{map[string]any{"address": p.DNS, "protocols": []any{"udp", "tcp"}}},
			"api":      map[string]any{"address": p.API, "tls": map[string]any{"auto": "off"}},
			"ops":      map[string]any{"address": p.Ops},
			"metrics":  map[string]any{"address": p.Metrics},
			"internal": map[string]any{"address": p.Internal},
			"sockets":  1,
		},
		"upstreams": map[string]any{
			"groups": map[string]any{
				"default": map[string]any{"servers": []any{"127.0.0.1:9"}, "timeout": "1s"},
			},
		},
		"log": map[string]any{"level": "info", "format": "text"},
	}
}

// WriteConfig writes cfg as YAML to path.
func WriteConfig(t testing.TB, path string, cfg map[string]any) {
	t.Helper()
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("config: %v", err)
	}
}

// NodeSpec describes a node to start: its name, ports, data directory and
// the configuration file written for it.
type NodeSpec struct {
	Name    string
	Ports   Ports
	DataDir string
	Config  string
}

// NewNodeSpec allocates ports and a data directory under dir and returns
// the spec with the base configuration (BaseConfig) for the caller to
// extend and write with WriteConfig(t, spec.Config, cfg).
func NewNodeSpec(t testing.TB, dir, name string) (NodeSpec, map[string]any) {
	t.Helper()
	s := NodeSpec{
		Name:    name,
		Ports:   NewPorts(t),
		DataDir: filepath.Join(dir, name, "data"),
		Config:  filepath.Join(dir, name, "kudurru.yaml"),
	}
	if err := os.MkdirAll(filepath.Dir(s.Config), 0o750); err != nil {
		t.Fatal(err)
	}
	return s, BaseConfig(name, s.DataDir, s.Ports)
}

// Node is a running `kudurru serve`.
type Node struct {
	NodeSpec

	cmd  *exec.Cmd
	log  *syncBuffer
	done chan struct{}
	err  error
}

// Options configures Start.
type Options struct {
	// Env is added to the process environment (KUDURRU_ENCRYPTION_KEY, ...).
	Env []string
	// Args replaces the default arguments ("serve", "--config", <config>).
	Args []string
}

// Start runs `kudurru serve --config <spec.Config>` and registers a
// cleanup that stops it and, when the test failed, prints its log.
func Start(t testing.TB, bin string, spec NodeSpec, opts Options) *Node {
	t.Helper()
	args := opts.Args
	if args == nil {
		args = []string{"serve", "--config", spec.Config}
	}
	n := &Node{NodeSpec: spec, log: &syncBuffer{}, done: make(chan struct{})}
	n.cmd = exec.Command(bin, args...) //nolint:gosec // the test names the binary
	n.cmd.Env = append(cleanEnv(), opts.Env...)
	n.cmd.Stdout, n.cmd.Stderr = n.log, n.log
	if err := n.cmd.Start(); err != nil {
		t.Fatalf("start kudurru: %v", err)
	}
	go func() {
		n.err = n.cmd.Wait()
		close(n.done)
	}()
	t.Cleanup(func() {
		n.Stop(15 * time.Second)
		if t.Failed() {
			t.Logf("kudurru log (%s):\n%s", spec.Name, n.Log())
		}
	})
	return n
}

// StartReady starts a node and waits up to 60 s for readiness.
func StartReady(t testing.TB, bin string, spec NodeSpec, opts Options) *Node {
	t.Helper()
	n := Start(t, bin, spec, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := n.WaitReady(ctx); err != nil {
		t.Fatalf("%s: %v", spec.Name, err)
	}
	return n
}

// cleanEnv drops KUDURRU_* variables of the calling environment so that a
// developer's settings never leak into a test node.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUDURRU_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Log returns everything the process wrote so far.
func (n *Node) Log() string { return n.log.String() }

// Done is closed when the process exited.
func (n *Node) Done() <-chan struct{} { return n.done }

// Signal sends sig to the process.
func (n *Node) Signal(sig os.Signal) error { return n.cmd.Process.Signal(sig) }

// Wait waits up to d for the process to exit and returns its exit code
// (-1 when it was killed by a signal).
func (n *Node) Wait(d time.Duration) (int, error) {
	select {
	case <-n.done:
	case <-time.After(d):
		return 0, fmt.Errorf("kudurru did not exit within %s", d)
	}
	return n.cmd.ProcessState.ExitCode(), nil
}

// Stop sends SIGTERM, waits up to d and then kills the process.
func (n *Node) Stop(d time.Duration) {
	select {
	case <-n.done:
		return
	default:
	}
	_ = n.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-n.done:
	case <-time.After(d):
		_ = n.cmd.Process.Kill()
		<-n.done
	}
}

// WaitReady polls GET /healthz/ready on the ops listener until it answers
// 200, the process exits or ctx ends.
func (n *Node) WaitReady(ctx context.Context) error {
	url := "http://" + n.Ports.Ops + "/healthz/ready"
	var last string
	for {
		select {
		case <-n.done:
			return fmt.Errorf("kudurru exited before it was ready: %v\n%s", n.err, n.Log())
		default:
		}
		code, body, err := Get(ctx, url)
		if err == nil && code == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("%d %s", code, strings.TrimSpace(body))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("kudurru not ready: %s\n%s", last, n.Log())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Get performs a GET with a 2 s timeout and returns the status and body.
func Get(ctx context.Context, url string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), nil
}

// Eventually polls cond every interval until it returns true or d passes.
func Eventually(d, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
