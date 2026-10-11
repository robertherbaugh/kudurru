package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cli"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

func TestMain(m *testing.M) {
	// Golden output renders local times; pin the zone.
	time.Local = time.UTC
	os.Exit(m.Run())
}

// testNow is the fixed clock of every CLI test.
var testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// memKeyring is an in-memory cli.Keyring; failing makes every call fail
// like a headless Linux box without a Secret Service.
type memKeyring struct {
	mu      sync.Mutex
	m       map[string]string
	failing bool
}

func (k *memKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failing {
		return "", errors.New("no secret service")
	}
	v, ok := k.m[service+"/"+user]
	if !ok {
		return "", cli.ErrNotFound
	}
	return v, nil
}

func (k *memKeyring) Set(service, user, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failing {
		return errors.New("no secret service")
	}
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[service+"/"+user] = secret
	return nil
}

func (k *memKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failing {
		return errors.New("no secret service")
	}
	delete(k.m, service+"/"+user)
	return nil
}

// harness runs CLI commands against a fake API with an isolated contexts
// file, environment and keyring.
type harness struct {
	t       *testing.T
	api     *fakeAPI
	cfgPath string
	env     map[string]string
	keyring *memKeyring
	stdin   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		t:       t,
		api:     newFakeAPI(t),
		cfgPath: filepath.Join(t.TempDir(), "kudurru", "config.yaml"),
		env:     map[string]string{},
		keyring: &memKeyring{},
	}
}

// loggedIn creates a harness with a saved context holding the valid token.
func loggedIn(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("login", "--server", h.api.URL(), "--token", fakeToken, "--name", "test")
	return h
}

func (h *harness) opts() *cliOpts {
	return &cliOpts{
		getenv: func(k string) string { return h.env[k] },
		stdin:  strings.NewReader(h.stdin),
		now:    func() time.Time { return testNow },
		creds:  &cli.Credentials{Keyring: h.keyring},
	}
}

// runCtx executes one command line.
func (h *harness) runCtx(ctx context.Context, stdout, stderr *syncBuffer, args ...string) error {
	root := newRootWith(h.opts())
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(append([]string{"--cli-config", h.cfgPath}, args...))
	return root.ExecuteContext(ctx)
}

func (h *harness) run(args ...string) (string, string, error) {
	var out, errOut syncBuffer
	err := h.runCtx(context.Background(), &out, &errOut, args...)
	return out.String(), errOut.String(), err
}

func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	return h.mustRunT(h.t, args...)
}

// mustRunT is mustRun failing t (a subtest) instead of the harness's test.
func (h *harness) mustRunT(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := h.run(args...)
	if err != nil {
		t.Fatalf("kudurru %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func (h *harness) config() *cli.Config {
	h.t.Helper()
	cfg, err := cli.LoadConfig(h.cfgPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

// syncBuffer is a bytes.Buffer safe for a writer goroutine and a polling
// reader.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// golden compares got with testdata/golden/<name>.txt (rewritten with
// -update). The fake server's URL is replaced by SERVER.
func golden(t *testing.T, h *harness, name, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, h.api.URL(), "SERVER")
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/kudurru -run %s -update)", err, t.Name())
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}
