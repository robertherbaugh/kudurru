package dnsdist

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func TestCertificateSnapshotsPreserveAcceptedPair(t *testing.T) {
	source := t.TempDir()
	cert, key := certificateForTest(t, 1)
	certPath, keyPath := filepath.Join(source, "tls.crt"), filepath.Join(source, "tls.key")
	if err := os.WriteFile(certPath, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Source: &cluster.StaticSource{}, Binary: "must-not-execute", Options: edge.Options{CertFile: certPath, KeyFile: keyPath}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.initCertificates(); err != nil {
		t.Fatal(err)
	}
	defer s.closeCertificates()
	accepted := s.tlsState.accepted
	for _, path := range []string{s.tlsState.root, accepted} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatal("snapshot directory is not private")
		}
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		st, err := os.Stat(filepath.Join(accepted, name))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatal("snapshot material is not private")
		}
	}
	nextCert, _ := certificateForTest(t, 2)
	if err := edge.WriteFileAtomic(certPath, nextCert, 0o600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.reloadCertificates(t.Context()), errCertificate) {
		t.Fatal("half-rotated pair was not rejected before console execution")
	}
	saved, err := os.ReadFile(filepath.Join(s.tlsState.root, "current", "cert.pem"))
	if err != nil || !bytes.Equal(saved, cert) || s.tlsState.accepted != accepted {
		t.Fatal("half rotation changed accepted restart material")
	}
	if err := edge.WriteFileAtomic(keyPath, bytes.Repeat([]byte("x"), maxCertificateFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readCertificate(); !errors.Is(err, errCertificate) {
		t.Fatal("unbounded certificate source accepted")
	}
}

func TestSupervisorLuaStaging(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Renderer = LuaRenderer{} })
	h.waitFor("ready", h.ready)
	h.src.ch <- []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("node-b", "10.0.0.12")}
	h.waitFor("Lua staged membership", func() bool { return h.count("console promote") == 1 && h.ready() })
	config, err := os.ReadFile(h.sup.MainPath())
	if err != nil || !bytes.Contains(config, []byte("s:setHealthCheckResponseValidator(")) {
		t.Fatal("original Lua node validator missing")
	}
}
