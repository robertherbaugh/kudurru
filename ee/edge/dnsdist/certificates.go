package dnsdist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

var errCertificate = errors.New("dnsdist: certificate update failed")
var errCertificateRecovery = errors.New("dnsdist: accepted certificate recovery failed")

const maxCertificateFile = 2 << 20

type certificateState struct {
	root, accepted string
	uid            int
	digest         [32]byte
	leaf           []byte
}

type certificatePair struct {
	cert, key, leaf []byte
	digest          [32]byte
}

func readCertificateFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // operator-configured certificate source
	if err != nil {
		return nil, errCertificate
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxCertificateFile {
		return nil, errCertificate
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCertificateFile+1))
	if err != nil || len(data) > maxCertificateFile {
		return nil, errCertificate
	}
	return data, nil
}

func (s *Supervisor) readCertificate() (*certificatePair, error) {
	cert, err := readCertificateFile(s.cfg.Options.CertFile)
	if err != nil {
		return nil, err
	}
	key, err := readCertificateFile(s.cfg.Options.KeyFile)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil || pair.Leaf == nil {
		return nil, errCertificate
	}
	now := time.Now()
	if now.Before(pair.Leaf.NotBefore) || !now.Before(pair.Leaf.NotAfter) {
		return nil, errCertificate
	}
	h := sha256.New()
	_, _ = h.Write(cert)
	_, _ = h.Write(key)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return &certificatePair{cert: cert, key: key, leaf: pair.Certificate[0], digest: digest}, nil
}

func certificateUID(configured string) (int, error) {
	if configured == "" {
		return os.Geteuid(), nil
	}
	name, _, _ := strings.Cut(configured, ":")
	if id, err := strconv.Atoi(name); err == nil && id >= 0 {
		return id, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, errCertificate
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, errCertificate
	}
	return uid, nil
}

func (c *certificateState) own(path string) error {
	if c.uid == os.Geteuid() {
		return nil
	}
	if err := os.Chown(path, c.uid, -1); err != nil {
		return errCertificate
	}
	return nil
}

func (c *certificateState) snapshot(pair *certificatePair) (string, error) {
	dir, err := os.MkdirTemp(c.root, "pair-")
	if err != nil {
		return "", errCertificate
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, f := range []struct {
		name string
		data []byte
	}{{"cert.pem", pair.cert}, {"key.pem", pair.key}} {
		path := filepath.Join(dir, f.name)
		if err := edge.WriteFileAtomic(path, f.data, 0o600); err != nil {
			return "", errCertificate
		}
		if err := c.own(path); err != nil {
			return "", err
		}
	}
	if err := c.own(dir); err != nil {
		return "", err
	}
	ok = true
	return dir, nil
}

func (c *certificateState) selectPair(dir string) error {
	temporary := filepath.Join(c.root, "next")
	_ = os.Remove(temporary)
	if err := os.Symlink(filepath.Base(dir), temporary); err != nil {
		return errCertificate
	}
	if err := os.Rename(temporary, filepath.Join(c.root, "current")); err != nil {
		return errCertificate
	}
	return nil
}

func (s *Supervisor) initCertificates() error {
	if s.cfg.Options.CertFile == "" {
		return nil
	}
	pair, err := s.readCertificate()
	if err != nil {
		return err
	}
	uid, err := certificateUID(s.cfg.User)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "kudurru-edge-tls-")
	if err != nil {
		return errCertificate
	}
	state := &certificateState{root: root, uid: uid}
	s.tlsState = state
	if err := state.own(root); err != nil {
		s.closeCertificates()
		return err
	}
	dir, err := state.snapshot(pair)
	if err == nil {
		err = state.selectPair(dir)
	}
	if err != nil {
		s.closeCertificates()
		return err
	}
	state.accepted, state.digest, state.leaf = dir, pair.digest, pair.leaf
	return nil
}

func (s *Supervisor) closeCertificates() {
	if s.tlsState != nil {
		_ = os.RemoveAll(s.tlsState.root)
		s.tlsState = nil
	}
}

// checkCertificate validates against immutable candidate material without
// changing the accepted symlink or the running child's restart configuration.
func (s *Supervisor) checkCertificate(ctx context.Context, dir string) error {
	s.mu.Lock()
	eps, zones := s.endpoints, s.zones
	s.mu.Unlock()
	top, err := s.topology(eps, zones)
	if err != nil {
		return errCertificate
	}
	for i := range top.Binds {
		if top.Binds[i].Protocol.TLS() {
			top.Binds[i].CertFile, top.Binds[i].KeyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
		}
	}
	files, err := s.cfg.Renderer.Render(top)
	if err != nil {
		return errCertificate
	}
	var main string
	for _, f := range files {
		path := filepath.Join(s.cfg.ConfigDir, "certcheck."+f.Name)
		if err := edge.WriteFileAtomic(path, f.Data, 0o600); err != nil {
			return errCertificate
		}
		defer func() { _ = os.Remove(path) }()
		if f.Name == s.cfg.Renderer.Main() {
			main = path
		}
	}
	return s.check(ctx, main)
}

// verifyCertificate performs new handshakes only; it never replays DNS queries.
// Verification pins the exact configured leaf, permitting private deployment
// names/CAs without turning this into a second certificate trust policy.
func (s *Supervisor) verifyCertificate(ctx context.Context, leaf []byte) error {
	s.mu.Lock()
	eps, zones := s.endpoints, s.zones
	s.mu.Unlock()
	top, err := s.topology(eps, zones)
	if err != nil {
		return errCertificate
	}
	for _, bind := range top.Binds {
		if !bind.Protocol.TLS() {
			continue
		}
		host, port, err := net.SplitHostPort(bind.Address)
		if err != nil {
			return errCertificate
		}
		if host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		if host == "::" {
			host = "::1"
		}
		addr := net.JoinHostPort(host, port)
		// nosemgrep: problem-based-packs.insecure-transport.go-stdlib.bypass-tls-verification.bypass-tls-verification -- Local reload probe authenticates the exact configured leaf in VerifyConnection; deployment names/private CAs need not match the loopback probe address.
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, //nolint:gosec // exact configured leaf pin below, no DNS request or external endpoint
			VerifyConnection: func(st tls.ConnectionState) error {
				if len(st.PeerCertificates) == 0 || !bytes.Equal(st.PeerCertificates[0].Raw, leaf) {
					return errCertificate
				}
				return nil
			}}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		switch bind.Protocol {
		case edge.DoQ, edge.DoH3:
			cfg.NextProtos = []string{"doq"}
			if bind.Protocol == edge.DoH3 {
				cfg.NextProtos = []string{"h3"}
			}
			conn, dialErr := quic.DialAddr(probeCtx, addr, cfg, &quic.Config{MaxIdleTimeout: 2 * time.Second})
			if dialErr == nil {
				dialErr = conn.CloseWithError(0, "")
			}
			err = dialErr
		default:
			dialer := tls.Dialer{Config: cfg}
			var conn net.Conn
			conn, err = dialer.DialContext(probeCtx, "tcp", addr)
			if err == nil {
				err = conn.Close()
			}
		}
		cancel()
		if err != nil {
			return errCertificate
		}
	}
	return nil
}

func (s *Supervisor) reloadCertificates(ctx context.Context) error {
	state := s.tlsState
	if state == nil {
		return nil
	}
	pair, err := s.readCertificate()
	if err != nil {
		return err
	}
	if pair.digest == state.digest {
		return nil
	}
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, s.cfg.UpdateTimeout)
	defer cancel()
	dir, err := state.snapshot(pair)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := s.checkCertificate(ctx, dir); err != nil {
		return errCertificate
	}
	if err := state.selectPair(dir); err != nil {
		return err
	}
	s.markUncertain()
	err = s.console(ctx, "reloadAllCertificates()")
	if err == nil {
		err = s.verifyCertificate(ctx, pair.leaf)
	}
	if err == nil {
		old := state.accepted
		state.accepted, state.digest, state.leaf = dir, pair.digest, pair.leaf
		accepted = true
		_ = os.RemoveAll(old)
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
		s.invalidateStatus()
		s.setUpdateError("")
		return nil
	}
	// A console timeout can happen after execution. Restore and verify every
	// frontend; when uncertainty remains, restart only from the accepted pair.
	if restoreErr := state.selectPair(state.accepted); restoreErr != nil {
		accepted = true // retain the selected snapshot until Run stops the child
		return errCertificateRecovery
	}
	recoverCtx, recoverCancel := context.WithTimeout(parentCtx, s.cfg.UpdateTimeout)
	defer recoverCancel()
	if reloadErr := s.console(recoverCtx, "reloadAllCertificates()"); reloadErr != nil || s.verifyCertificate(recoverCtx, state.leaf) != nil {
		if restartErr := s.restart(recoverCtx, "certificates"); restartErr != nil {
			return errCertificateRecovery
		}
	} else {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
		s.invalidateStatus()
	}
	return errCertificate
}
