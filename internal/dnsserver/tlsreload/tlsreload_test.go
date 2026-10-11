package tlsreload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
)

func gen(t *testing.T, dir, prefix string) *testcert.Pair {
	t.Helper()
	p, err := testcert.Write(dir, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// install atomically replaces dst with src's content (write + rename), the
// way cert-manager and most tooling rotate files.
func install(t *testing.T, content []byte, dst string) {
	t.Helper()
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func serial(r *Reloader) string { return r.Certificate().Leaf.SerialNumber.String() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	a := gen(t, dir, "")
	r, err := New(a.CertFile, a.KeyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if serial(r) != a.Cert.SerialNumber.String() || r.NotAfter().IsZero() || r.LoadedAt().IsZero() {
		t.Fatal("initial load wrong")
	}
	cfg := r.TLSConfig("dot")
	if cfg.MinVersion != tls.VersionTLS12 || len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "dot" {
		t.Fatalf("TLSConfig wrong: %+v", cfg)
	}
	if c, _ := cfg.GetCertificate(nil); c != r.Certificate() {
		t.Fatal("GetCertificate must return the current certificate")
	}

	// Unchanged content: no swap.
	if swapped, err := r.Reload(); swapped || err != nil {
		t.Fatalf("unchanged reload swapped=%v err=%v", swapped, err)
	}

	var swaps int
	r.OnSwap(func(*tls.Certificate) { swaps++ })

	// Rotation.
	b := gen(t, t.TempDir(), "")
	install(t, b.CertPEM, a.CertFile)
	install(t, b.KeyPEM, a.KeyFile)
	if swapped, err := r.Reload(); !swapped || err != nil {
		t.Fatalf("rotation swapped=%v err=%v", swapped, err)
	}
	if serial(r) != b.Cert.SerialNumber.String() || swaps != 1 {
		t.Fatal("rotation not applied")
	}

	// Half-rotated pair: keep serving b.
	c := gen(t, t.TempDir(), "")
	install(t, c.CertPEM, a.CertFile)
	if _, err := r.Reload(); err == nil || r.LastError() == nil {
		t.Fatal("mismatched pair must fail")
	}
	if serial(r) != b.Cert.SerialNumber.String() {
		t.Fatal("failed reload must keep the previous certificate")
	}
	install(t, c.KeyPEM, a.KeyFile)
	if swapped, err := r.Reload(); !swapped || err != nil || r.LastError() != nil {
		t.Fatalf("completed rotation swapped=%v err=%v", swapped, err)
	}
	if reloads, failures := r.Stats(); reloads != 2 || failures != 1 {
		t.Fatalf("stats reloads=%d failures=%d", reloads, failures)
	}
}

func TestNewErrors(t *testing.T) {
	dir := t.TempDir()
	a := gen(t, dir, "a")
	b := gen(t, dir, "b")
	for name, files := range map[string][2]string{
		"empty":    {"", ""},
		"missing":  {filepath.Join(dir, "nope.pem"), a.KeyFile},
		"mismatch": {a.CertFile, b.KeyFile},
	} {
		if _, err := New(files[0], files[1], nil); err == nil {
			t.Errorf("%s: New should fail", name)
		}
	}
}

func TestStoreDedup(t *testing.T) {
	dir := t.TempDir()
	a := gen(t, dir, "")
	s := NewStore(nil)
	r1, err := s.Get(a.CertFile, a.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Get(filepath.Join(dir, ".", "cert.pem"), filepath.Join(dir, "x", "..", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 || len(s.Reloaders()) != 1 {
		t.Fatal("one file pair must map to one Reloader")
	}
	if cf, kf := r1.Files(); cf != a.CertFile || kf != a.KeyFile {
		t.Fatalf("Files() = %s %s", cf, kf)
	}
	if _, err := s.Get(a.CertFile, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("bad pair must fail")
	}
}

func runStore(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	// Give the watcher a moment to register.
	time.Sleep(50 * time.Millisecond)
}

func TestWatchFsnotify(t *testing.T) {
	dir := t.TempDir()
	a := gen(t, dir, "")
	s := NewStore(nil)
	s.PollInterval = -1 // events only
	r, err := s.Get(a.CertFile, a.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	runStore(t, s)

	b := gen(t, t.TempDir(), "")
	install(t, b.CertPEM, a.CertFile)
	install(t, b.KeyPEM, a.KeyFile)
	waitFor(t, "fsnotify reload", func() bool { return serial(r) == b.Cert.SerialNumber.String() })
}

func TestWatchPoll(t *testing.T) {
	dir := t.TempDir()
	a := gen(t, dir, "")
	s := NewStore(nil)
	s.PollInterval = 20 * time.Millisecond
	s.noWatch = true // poller only, as on filesystems without inotify
	r, err := s.Get(a.CertFile, a.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	runStore(t, s)
	b := gen(t, t.TempDir(), "")
	if err := os.WriteFile(a.KeyFile, b.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.CertFile, b.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "poll reload", func() bool { return serial(r) == b.Cert.SerialNumber.String() })
}

// TestKubernetesSecretSwap reproduces the projected-volume layout: the
// visible files are symlinks into "..data", itself a symlink that kubelet
// swaps atomically to a new timestamped directory.
func TestKubernetesSecretSwap(t *testing.T) {
	dir := t.TempDir()
	v1 := filepath.Join(dir, "..2026_10_11_v1")
	if err := os.Mkdir(v1, 0o700); err != nil {
		t.Fatal(err)
	}
	a := gen(t, v1, "tls.")
	mustSymlink(t, filepath.Base(v1), filepath.Join(dir, "..data"))
	mustSymlink(t, filepath.Join("..data", filepath.Base(a.CertFile)), filepath.Join(dir, "tls.crt"))
	mustSymlink(t, filepath.Join("..data", filepath.Base(a.KeyFile)), filepath.Join(dir, "tls.key"))

	s := NewStore(nil)
	s.PollInterval = -1
	r, err := s.Get(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	runStore(t, s)

	v2 := filepath.Join(dir, "..2026_10_11_v2")
	if err := os.Mkdir(v2, 0o700); err != nil {
		t.Fatal(err)
	}
	b := gen(t, v2, "tls.")
	mustSymlink(t, filepath.Base(v2), filepath.Join(dir, "..data_tmp"))
	if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "symlink swap reload", func() bool { return serial(r) == b.Cert.SerialNumber.String() })
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestHandshakeSeesRotation proves the property listeners rely on: a TLS
// listener built once from TLSConfig serves the new certificate on the next
// handshake after a reload.
func TestHandshakeSeesRotation(t *testing.T) {
	dir := t.TempDir()
	a := gen(t, dir, "")
	r, err := New(a.CertFile, a.KeyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", r.TLSConfig("dot"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}()
		}
	}()
	b := gen(t, t.TempDir(), "")
	pool := x509.NewCertPool()
	pool.AddCert(a.Cert)
	pool.AddCert(b.Cert)
	peer := func() string {
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"dot"}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		st := c.ConnectionState()
		if st.NegotiatedProtocol != "dot" {
			t.Fatalf("ALPN = %q", st.NegotiatedProtocol)
		}
		return st.PeerCertificates[0].SerialNumber.String()
	}
	if got := peer(); got != a.Cert.SerialNumber.String() {
		t.Fatalf("first handshake served %s", got)
	}
	install(t, b.CertPEM, a.CertFile)
	install(t, b.KeyPEM, a.KeyFile)
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := peer(); got != b.Cert.SerialNumber.String() {
		t.Fatalf("handshake after rotation served %s", got)
	}
	if !strings.Contains(a.Cert.Subject.CommonName, "kudurru test") {
		t.Fatal("unexpected test certificate")
	}
}
