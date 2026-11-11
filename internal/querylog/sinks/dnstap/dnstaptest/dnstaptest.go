// Package dnstaptest is a dnstap collector for tests: it listens on a Unix
// socket, TCP or TLS, completes the bidirectional Frame Streams handshake as
// the reader side, reads the data frames and decodes each dnstap message
// with a strict protocol buffers parser that refuses unknown fields, wrong
// wire types and missing required fields. It can stall (stop reading) and
// drop its connections on demand.
package dnstaptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// Message is a decoded dnstap.Message.
type Message struct {
	Type            int
	SocketFamily    int
	SocketProtocol  int
	QueryAddress    netip.Addr
	ResponseAddress netip.Addr
	QueryPort       int
	ResponsePort    int
	QueryTime       time.Time
	QueryMessage    []byte
	QueryZone       []byte
	ResponseTime    time.Time
	ResponseMessage []byte
}

// Frame is a decoded dnstap.Dnstap.
type Frame struct {
	Identity, Version, Extra string
	Message                  Message
}

// Decode parses one dnstap.Dnstap. It accepts exactly the fields of
// dnstap.proto that kudurru sends, each with its declared wire type, and
// requires the two required fields (Dnstap.type MESSAGE and Message.type).
func Decode(b []byte) (Frame, error) {
	var f Frame
	sawType, sawMessage := false, false
	err := fields(b, func(num protowire.Number, typ protowire.Type, v uint64, raw []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			f.Identity = string(raw)
		case num == 2 && typ == protowire.BytesType:
			f.Version = string(raw)
		case num == 3 && typ == protowire.BytesType:
			f.Extra = string(raw)
		case num == 15 && typ == protowire.VarintType:
			if v != 1 {
				return fmt.Errorf("Dnstap.type %d is not MESSAGE", v)
			}
			sawType = true
		case num == 14 && typ == protowire.BytesType:
			m, err := decodeMessage(raw)
			if err != nil {
				return err
			}
			f.Message, sawMessage = m, true
		default:
			return fmt.Errorf("Dnstap: unexpected field %d of wire type %d", num, typ)
		}
		return nil
	})
	switch {
	case err != nil:
		return Frame{}, err
	case !sawType:
		return Frame{}, errors.New("Dnstap.type is missing")
	case !sawMessage:
		return Frame{}, errors.New("Dnstap.message is missing")
	}
	return f, nil
}

func decodeMessage(b []byte) (Message, error) {
	var m Message
	var querySec, responseSec uint64
	var queryNsec, responseNsec uint32
	var hasQuerySec, hasResponseSec bool
	addr := func(raw []byte) (netip.Addr, error) {
		a, ok := netip.AddrFromSlice(raw)
		if !ok {
			return netip.Addr{}, fmt.Errorf("an address of %d bytes", len(raw))
		}
		return a, nil
	}
	err := fields(b, func(num protowire.Number, typ protowire.Type, v uint64, raw []byte) error {
		var err error
		want := map[protowire.Number]protowire.Type{
			1: protowire.VarintType, 2: protowire.VarintType, 3: protowire.VarintType, 4: protowire.BytesType, 5: protowire.BytesType,
			6: protowire.VarintType, 7: protowire.VarintType, 8: protowire.VarintType, 9: protowire.Fixed32Type, 10: protowire.BytesType,
			11: protowire.BytesType, 12: protowire.VarintType, 13: protowire.Fixed32Type, 14: protowire.BytesType,
		}
		if w, ok := want[num]; !ok || w != typ {
			return fmt.Errorf("Message: unexpected field %d of wire type %d", num, typ)
		}
		switch num {
		case 1:
			m.Type = int(v) //nolint:gosec // G115: test decoder of small enums
		case 2:
			m.SocketFamily = int(v) //nolint:gosec // G115: test decoder
		case 3:
			m.SocketProtocol = int(v) //nolint:gosec // G115: test decoder
		case 4:
			m.QueryAddress, err = addr(raw)
		case 5:
			m.ResponseAddress, err = addr(raw)
		case 6:
			m.QueryPort = int(v) //nolint:gosec // G115: test decoder
		case 7:
			m.ResponsePort = int(v) //nolint:gosec // G115: test decoder
		case 8:
			querySec, hasQuerySec = v, true
		case 9:
			queryNsec = uint32(v) //nolint:gosec // G115: fixed32
		case 10:
			m.QueryMessage = append([]byte(nil), raw...)
		case 11:
			m.QueryZone = append([]byte(nil), raw...)
		case 12:
			responseSec, hasResponseSec = v, true
		case 13:
			responseNsec = uint32(v) //nolint:gosec // G115: fixed32
		case 14:
			m.ResponseMessage = append([]byte(nil), raw...)
		}
		return err
	})
	if err != nil {
		return Message{}, err
	}
	if m.Type == 0 {
		return Message{}, errors.New("Message.type is missing")
	}
	if hasQuerySec {
		m.QueryTime = time.Unix(int64(querySec), int64(queryNsec)) //nolint:gosec // G115: test decoder
	}
	if hasResponseSec {
		m.ResponseTime = time.Unix(int64(responseSec), int64(responseNsec)) //nolint:gosec // G115: test decoder
	}
	return m, nil
}

// fields walks the fields of one message.
func fields(b []byte, fn func(protowire.Number, protowire.Type, uint64, []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var v uint64
		var raw []byte
		switch typ {
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			var x uint32
			x, n = protowire.ConsumeFixed32(b)
			v = uint64(x)
		case protowire.BytesType:
			raw, n = protowire.ConsumeBytes(b)
		default:
			return fmt.Errorf("field %d: wire type %d is not used by dnstap", num, typ)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(num, typ, v, raw); err != nil {
			return err
		}
	}
	return nil
}

// Collector is a dnstap reader on one listener.
type Collector struct {
	t        testing.TB
	listener net.Listener
	target   string
	certPEM  []byte

	mu       sync.Mutex
	frames   []Frame
	errs     []error
	conns    map[net.Conn]struct{}
	accepted int
	finished int
	stalled  chan struct{} // closed while reading; nil when stalled
	resume   chan struct{}
}

// Listen starts a collector: network is unix, tcp or tls.
func Listen(t testing.TB, network string) *Collector {
	t.Helper()
	c := &Collector{t: t, conns: map[net.Conn]struct{}{}}
	var err error
	switch network {
	case "unix":
		// A short path: socket paths are limited to about 100 bytes.
		dir, mkErr := shortDir(t)
		if mkErr != nil {
			t.Fatal(mkErr)
		}
		path := filepath.Join(dir, "dnstap.sock")
		c.listener, err = net.Listen("unix", path)
		c.target = "unix:" + path
	case "tcp":
		c.listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			c.target = "tcp:" + c.listener.Addr().String()
		}
	case "tls":
		var cert tls.Certificate
		cert, c.certPEM = selfSigned(t)
		c.listener, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err == nil {
			c.target = "tls:" + c.listener.Addr().String()
		}
	default:
		t.Fatalf("dnstaptest: unknown network %q", network)
	}
	if err != nil {
		t.Fatal(err)
	}
	go c.accept()
	t.Cleanup(c.Close)
	return c
}

// Target is the querylog.sinks[].dnstap.target of the collector.
func (c *Collector) Target() string { return c.target }

// CertPEM is the certificate of a tls collector (its own CA), for caRef.
func (c *Collector) CertPEM() []byte { return c.certPEM }

// Close stops the listener and every connection.
func (c *Collector) Close() {
	_ = c.listener.Close()
	c.DropConnections()
	c.Resume()
}

func (c *Collector) accept() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		c.mu.Lock()
		c.conns[conn] = struct{}{}
		c.accepted++
		c.mu.Unlock()
		go c.serve(conn)
	}
}

func (c *Collector) fail(err error) {
	c.mu.Lock()
	c.errs = append(c.errs, err)
	c.mu.Unlock()
}

// waitReading blocks while the collector is stalled.
func (c *Collector) waitReading() {
	c.mu.Lock()
	resume := c.resume
	c.mu.Unlock()
	if resume != nil {
		<-resume
	}
}

func (c *Collector) serve(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		c.mu.Lock()
		delete(c.conns, conn)
		c.mu.Unlock()
	}()
	c.waitReading()
	r := dnstap.NewReader(conn)
	ready, err := r.ReadControl(dnstap.ControlReady)
	if err != nil {
		c.fail(fmt.Errorf("READY: %w", err))
		return
	}
	if !ready.HasContentType(dnstap.ContentType) {
		c.fail(fmt.Errorf("READY offers %v", ready.ContentTypes))
		return
	}
	if _, err := conn.Write(dnstap.AppendControl(nil, dnstap.ControlAccept, dnstap.ContentType)); err != nil {
		return
	}
	start, err := r.ReadControl(dnstap.ControlStart)
	if err != nil {
		c.fail(fmt.Errorf("START: %w", err))
		return
	}
	if len(start.ContentTypes) != 1 || start.ContentTypes[0] != dnstap.ContentType {
		c.fail(fmt.Errorf("START names %v", start.ContentTypes))
		return
	}
	var buf []byte
	for {
		c.waitReading()
		data, control, err := r.Next(buf)
		if err != nil {
			return // a closed connection; the sink reconnects
		}
		if control != nil {
			if control.Type != dnstap.ControlStop {
				c.fail(fmt.Errorf("unexpected control frame %d", control.Type))
				return
			}
			_, _ = conn.Write(dnstap.AppendControl(nil, dnstap.ControlFinish))
			c.mu.Lock()
			c.finished++
			c.mu.Unlock()
			return
		}
		buf = data
		frame, err := Decode(data)
		if err != nil {
			c.fail(err)
			continue
		}
		c.mu.Lock()
		c.frames = append(c.frames, frame)
		c.mu.Unlock()
	}
}

// Frames returns the frames decoded so far.
func (c *Collector) Frames() []Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Frame(nil), c.frames...)
}

// Errors returns every protocol or decoding error the collector saw.
func (c *Collector) Errors() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errs...)
}

// Accepted is the number of connections accepted so far.
func (c *Collector) Accepted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accepted
}

// Finished is the number of streams that ended with STOP and FINISH.
func (c *Collector) Finished() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finished
}

// WaitFrames waits until at least n frames arrived and returns them.
func (c *Collector) WaitFrames(n int, timeout time.Duration) []Frame {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		frames := c.Frames()
		if len(frames) >= n {
			return frames
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("dnstaptest: %d of %d frames after %s (errors %v)", len(frames), n, timeout, c.Errors())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Stall stops reading on every connection, current and future, until
// Resume. A writer then blocks once the socket buffers are full.
func (c *Collector) Stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resume == nil {
		c.resume = make(chan struct{})
	}
}

// Resume reads again.
func (c *Collector) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resume != nil {
		close(c.resume)
		c.resume = nil
	}
}

// DropConnections closes every open connection without FINISH.
func (c *Collector) DropConnections() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for conn := range c.conns {
		_ = conn.Close()
	}
}

func selfSigned(t testing.TB) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dnstap collector"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM
}

// shortDir is a temporary directory with a short path: a Unix socket path
// is limited to about 100 bytes, and test names make t.TempDir long.
func shortDir(t testing.TB) (string, error) {
	dir, err := os.MkdirTemp("", "dnstap")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, nil
}
