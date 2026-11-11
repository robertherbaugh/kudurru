// Package dnstap is the dnstap sink of querylog.sinks (D372): every logged
// query becomes dnstap messages (https://dnstap.info) sent in Frame Streams
// over a Unix socket, TCP or TLS to a collector such as DNS-collector,
// Vector or the dnstap command. It runs on the sink host of
// internal/querylog behind its own bounded queue, like every additional
// sink, so a collector that stalls or goes away loses only its own
// messages.
//
// By default a message follows querylog.privacy exactly as the local query
// log does: it is rebuilt from the logged event, after the transform, and
// carries the question and the response code, never the answer records.
// With raw: true the sink is a querylog.RawSink: it receives the events
// before the transform and sends the DNS messages as they were received and
// answered, with the real client address and port. Raw is independent of
// the privacy mode; the exclusions (querylog.exclude) still apply. Raw is
// writable only with querylog.raw, and a raw sink records an audit event
// through Options.Audit before it opens; it does not open without one.
package dnstap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

const (
	// maxCABytes and maxKeyBytes bound the PEM material a sink reads.
	maxCABytes  = 64 << 10
	maxKeyBytes = 16 << 10
	// defaultWriteTimeout bounds a write without a context deadline.
	defaultWriteTimeout = 30 * time.Second
	// closeWait bounds STOP and FINISH when the sink closes.
	closeWait = time.Second
)

// Resolver reads the current value of a credential reference (the node's
// resolver, internal/querylog/sinks/httpbatch.Resolver).
type Resolver func(ctx context.Context, ref config.CredentialRef) ([]byte, error)

// RawStart is what a raw sink records before it opens.
type RawStart struct {
	// Sink is the name of the querylog.sinks entry.
	Sink string `json:"sink"`
	// Target is where the raw messages go.
	Target string `json:"target"`
	// Types are the message types it sends.
	Types []string `json:"types"`
}

// Options are the node's own inputs to every dnstap sink.
type Options struct {
	// Resolve reads caRef, certRef and keyRef.
	Resolve Resolver
	Logger  *slog.Logger
	// Node is the identity of a sink without one.
	Node string
	// Version is the dnstap version, "kudurru/<version>".
	Version string
	// Audit records that a raw sink starts sending. Required for raw: a
	// sink whose audit event cannot be recorded does not open, and the host
	// tries again with backoff.
	Audit func(ctx context.Context, start RawStart) error
	// Dial replaces net.Dialer for tests; nil dials normally.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Sink sends dnstap to one collector. It is used from the sink's own
// goroutine only.
type Sink struct {
	name     string
	settings config.QuerylogDnstap
	target   config.DnstapTarget
	o        Options
	identity []byte
	version  []byte
	types    map[MessageType]bool
	raw      bool
	tls      *tls.Config

	conn   net.Conn
	reader *Reader

	out, scratch, extra []byte
	frame               Frame
}

// New builds the sink of one querylog.sinks entry from its dnstap object.
// It validates the settings and does no I/O.
func New(spec querylog.SinkSpec, o Options) (*Sink, error) {
	var settings config.QuerylogDnstap
	if len(spec.Config) == 0 {
		return nil, errors.New("the dnstap settings are missing")
	}
	if err := json.Unmarshal(spec.Config, &settings); err != nil {
		return nil, errors.New("the dnstap settings are malformed")
	}
	if settings.Target == "" {
		return nil, errors.New("dnstap.target is required")
	}
	if errs := settings.Validate(config.QuerylogSinkDnstap); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	target, err := config.ParseDnstapTarget(settings.Target)
	if err != nil {
		return nil, fmt.Errorf("dnstap.target %w", err)
	}
	if settings.Raw && o.Audit == nil {
		return nil, errors.New("dnstap.raw needs the node's audit trail, which this server does not provide")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	identity := settings.Identity
	if identity == "" {
		identity = o.Node
	}
	s := &Sink{name: spec.Name, settings: settings, target: target, o: o, identity: []byte(identity), version: []byte(o.Version), raw: settings.Raw, types: map[MessageType]bool{}}
	for _, t := range settings.MessageTypes() {
		s.types[messageTypes[t]] = true
	}
	return s, nil
}

// messageTypes maps the settings names onto dnstap types.
var messageTypes = map[string]MessageType{
	config.DnstapClientQuery: ClientQuery, config.DnstapClientResponse: ClientResponse,
	config.DnstapAuthQuery: AuthQuery, config.DnstapAuthResponse: AuthResponse,
	config.DnstapForwarderQuery: ForwarderQuery, config.DnstapForwarderResponse: ForwarderResponse,
}

// Name implements querylog.Sink.
func (s *Sink) Name() string { return config.QuerylogSinkDnstap }

// Raw implements querylog.RawSink.
func (s *Sink) Raw() bool { return s.raw }

// Open reads the TLS material and, for a raw sink, records the audit event.
// It does not contact the collector: the first batch connects, so a
// collector that is down never delays the sink host.
func (s *Sink) Open(ctx context.Context, _ json.RawMessage, _ prometheus.Registerer) error {
	if s.target.Scheme == config.DnstapSchemeTLS {
		cfg, err := s.tlsConfig(ctx)
		if err != nil {
			return err
		}
		s.tls = cfg
	}
	if s.raw {
		start := RawStart{Sink: s.name, Target: s.settings.Target, Types: s.settings.MessageTypes()}
		if err := s.o.Audit(ctx, start); err != nil {
			return fmt.Errorf("dnstap: raw messages are not sent before their audit event is recorded: %w", err)
		}
	}
	return nil
}

func (s *Sink) tlsConfig(ctx context.Context) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.settings.ServerName}
	if cfg.ServerName == "" {
		cfg.ServerName = s.target.Host()
	}
	read := func(what string, ref *config.CredentialRef, limit int) ([]byte, error) {
		if s.o.Resolve == nil {
			return nil, fmt.Errorf("dnstap: %s: this server cannot read credential references", what)
		}
		value, err := s.o.Resolve(ctx, *ref)
		if err != nil {
			return nil, fmt.Errorf("dnstap: %s: %w", what, err)
		}
		if len(value) > limit {
			return nil, fmt.Errorf("dnstap: %s: the reference holds more than %d KiB", what, limit>>10)
		}
		return value, nil
	}
	if s.settings.CARef != nil {
		pem, err := read("CA certificates", s.settings.CARef, maxCABytes)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("dnstap: CA certificates: the reference holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if s.settings.CertRef != nil && s.settings.KeyRef != nil {
		certPEM, err := read("client certificate", s.settings.CertRef, maxCABytes)
		if err != nil {
			return nil, err
		}
		keyPEM, err := read("client key", s.settings.KeyRef, maxKeyBytes)
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			// The error of X509KeyPair names no key material.
			return nil, fmt.Errorf("dnstap: client certificate and key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// Write implements querylog.Sink.
func (s *Sink) Write(batch []*querylog.Event) error {
	return s.WriteBatch(context.Background(), batch, querylog.PrivacyFull)
}

// WriteBatch sends one batch on the connection, connecting first when there
// is none. A batch of which nothing was written (no connection, a refused
// handshake, a write that failed before its first byte) is marked
// retryable. A batch that was written in part has an unknown outcome at the
// collector and is dropped; the next batch connects again. The events
// arrive after the privacy transform, or before it for a raw sink.
func (s *Sink) WriteBatch(ctx context.Context, batch []*querylog.Event, _ querylog.Mode) error {
	s.out = s.out[:0]
	for _, ev := range batch {
		s.out = s.AppendEvent(s.out, ev)
	}
	if len(s.out) == 0 {
		return nil // no message of the configured types
	}
	if s.conn != nil && !s.alive() {
		s.disconnect()
	}
	if s.conn == nil {
		if err := s.connect(ctx); err != nil {
			return querylog.Retryable(fmt.Errorf("dnstap: connect to %s: %w", s.settings.Target, err))
		}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultWriteTimeout)
	}
	conn := s.conn
	_ = conn.SetWriteDeadline(deadline)
	// A canceled context (the drain bound of a retired sink) ends the write.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Unix(1, 0)) })
	n, err := conn.Write(s.out)
	stop()
	if err != nil {
		s.disconnect()
		if n == 0 {
			return querylog.Retryable(fmt.Errorf("dnstap: write to %s: %w", s.settings.Target, err))
		}
		return fmt.Errorf("dnstap: write to %s: %w (%d of %d bytes were sent, so the batch is not sent again)", s.settings.Target, err, n, len(s.out))
	}
	return nil
}

// alive reports whether the connection still looks open. The collector
// sends nothing after ACCEPT until it answers STOP, so anything readable,
// including the end of the stream, means the connection is gone.
func (s *Sink) alive() bool {
	var one [1]byte
	_ = s.conn.SetReadDeadline(time.Now())
	_, err := s.conn.Read(one[:])
	_ = s.conn.SetReadDeadline(time.Time{})
	var timeout net.Error
	return err != nil && errors.As(err, &timeout) && timeout.Timeout()
}

// connect dials and completes the bidirectional handshake within ctx.
func (s *Sink) connect(ctx context.Context) (err error) {
	dial := s.o.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, s.target.Network(), s.target.Address)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(defaultWriteTimeout))
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if s.tls != nil {
		tc := tls.Client(conn, s.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tc
	}
	if _, err := conn.Write(AppendControl(nil, ControlReady, ContentType)); err != nil {
		return err
	}
	reader := NewReader(conn)
	accept, err := reader.ReadControl(ControlAccept)
	if err != nil {
		return err
	}
	if !accept.HasContentType(ContentType) {
		return fmt.Errorf("the collector does not accept %s", ContentType)
	}
	if _, err := conn.Write(AppendControl(nil, ControlStart, ContentType)); err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	s.conn, s.reader = conn, reader
	return nil
}

func (s *Sink) disconnect() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn, s.reader = nil, nil
}

// Flush implements querylog.Sink: every batch is written whole.
func (s *Sink) Flush(context.Context) error { return nil }

// Close ends the stream with STOP and waits briefly for FINISH.
func (s *Sink) Close() error {
	if s.conn == nil {
		return nil
	}
	defer s.disconnect()
	_ = s.conn.SetDeadline(time.Now().Add(closeWait))
	if _, err := s.conn.Write(AppendControl(nil, ControlStop)); err != nil {
		return nil // the collector is gone; nothing is pending
	}
	_, _ = s.reader.ReadControl(ControlFinish)
	return nil
}

// role is how kudurru took part in a query.
type role uint8

const (
	roleClient role = iota
	roleAuth
)

// AppendEvent appends the Frame Streams data frames of one event to out,
// exactly as WriteBatch sends them: none, one or two client or auth
// messages and none, one or two forwarder messages, by the types.
func (s *Sink) AppendEvent(out []byte, ev *querylog.Event) []byte {
	r := roleClient
	if ev.Status == pipeline.StatusAuthoritative || ev.Status == pipeline.StatusLocalRecord {
		r = roleAuth
	}
	queryType, responseType := ClientQuery, ClientResponse
	if r == roleAuth {
		queryType, responseType = AuthQuery, AuthResponse
	}
	// A prefetch refresh is kudurru's own query: it has no client exchange.
	client := !ev.Flags.Prefetch
	upstream := ev.Upstream != "" && !ev.Flags.Cached
	answered := ev.ReplyType != querylog.ReplyNone
	wantClient := client && (s.types[queryType] || s.types[responseType] && answered)
	wantForward := upstream && (s.types[ForwarderQuery] || s.types[ForwarderResponse])
	if !wantClient && !wantForward {
		return out
	}
	question, response := s.messages(ev, r)
	s.extra = appendExtra(s.extra[:0], ev)
	f := &s.frame
	*f = Frame{Identity: s.identity, Version: s.version, Extra: s.extra}
	sent := ev.Time.Add(ev.Latency)
	if wantClient {
		base := Message{SocketProtocol: socketProtocol(ev.Proto), QueryTime: ev.Time}
		// The real address for a raw sink, else the one the privacy
		// transform left: a /24 or /48 prefix, 0.0.0.0, or none for a
		// pseudonym.
		if addr := ev.Client.Unmap(); addr.IsValid() {
			base.SocketFamily = FamilyINET
			if addr.Is6() {
				base.SocketFamily = FamilyINET6
			}
			base.QueryAddress = addr.AsSlice()
			if s.raw && ev.Wire != nil {
				base.QueryPort = ev.Wire.ClientPort
			}
		}
		if r == roleAuth && ev.Zone != "" && ev.Zone != querylog.HiddenName {
			base.QueryZone = wireName(ev.Zone)
		}
		if s.types[queryType] {
			f.Message = base
			f.Message.Type, f.Message.QueryMessage = queryType, question
			out = s.appendFrame(out, f)
		}
		if s.types[responseType] && answered {
			f.Message = base
			f.Message.Type, f.Message.ResponseTime, f.Message.ResponseMessage = responseType, sent, response
			out = s.appendFrame(out, f)
		}
	}
	if wantForward {
		// kudurru does not record its upstream exchanges: these carry the
		// question and the outcome rebuilt from the event, the client's
		// times and, in extra, the upstream's name.
		forwardQuestion, forwardResponse := synthesize(ev, roleClient)
		if s.types[ForwarderQuery] {
			f.Message = Message{Type: ForwarderQuery, QueryTime: ev.Time, QueryMessage: forwardQuestion}
			out = s.appendFrame(out, f)
		}
		if s.types[ForwarderResponse] {
			f.Message = Message{Type: ForwarderResponse, QueryTime: ev.Time, ResponseTime: sent, ResponseMessage: forwardResponse}
			out = s.appendFrame(out, f)
		}
	}
	return out
}

func (s *Sink) appendFrame(out []byte, f *Frame) []byte {
	start := len(out)
	out = append(out, 0, 0, 0, 0) // the length, set below
	out, s.scratch = AppendFrame(out, s.scratch, f)
	n := len(out) - start - 4
	// A frame holds one event: two DNS messages of at most 64 KiB and
	// short strings, far below 4 GiB.
	binary.BigEndian.PutUint32(out[start:], uint32(n)) //nolint:gosec // G115: bounded as above
	return out
}

// messages returns the query and the response a message carries: for a raw
// sink the DNS messages as they were, else rebuilt from the event.
func (s *Sink) messages(ev *querylog.Event, r role) (question, response []byte) {
	if s.raw && ev.Wire != nil {
		question, response = ev.Wire.Query, ev.Wire.Response
	}
	if question == nil || response == nil {
		q, a := synthesize(ev, r)
		if question == nil {
			question = q
		}
		if response == nil {
			response = a
		}
	}
	return question, response
}

// synthesize rebuilds the query and the response from the logged event:
// the question (name, type and class as logged, so "hidden" where the mode
// hides names), the header flags and the response code. Answer records are
// never rebuilt. A name that does not pack leaves both out.
func synthesize(ev *querylog.Event, r role) (question, response []byte) {
	name := ev.Name
	if name == "" {
		return nil, nil
	}
	q := new(dns.Msg)
	q.Question = []dns.Question{{Name: dns.Fqdn(name), Qtype: ev.QType, Qclass: ev.QClass}}
	q.RecursionDesired, q.CheckingDisabled = ev.Flags.RD, ev.Flags.CD
	if ev.Flags.DO {
		q.SetEdns0(dns.DefaultMsgSize, true)
	}
	question, err := q.Pack()
	if err != nil {
		return nil, nil
	}
	if ev.ReplyType == querylog.ReplyNone {
		return question, nil
	}
	a := new(dns.Msg)
	a.Question = q.Question
	a.Response = true
	a.Rcode = ev.Rcode
	a.RecursionDesired, a.CheckingDisabled = ev.Flags.RD, ev.Flags.CD
	a.Authoritative = r == roleAuth
	a.RecursionAvailable = r != roleAuth
	a.AuthenticatedData, a.Truncated = ev.Flags.AD, ev.Flags.TC
	if ev.Rcode > 0xF || ev.Flags.DO {
		a.SetEdns0(dns.DefaultMsgSize, ev.Flags.DO)
	}
	response, err = a.Pack()
	if err != nil {
		response = nil
	}
	return question, response
}

// wireName packs a zone name, nil when it does not pack.
func wireName(name string) []byte {
	buf := make([]byte, 256)
	n, err := dns.PackDomainName(dns.Fqdn(name), buf, 0, nil, false)
	if err != nil {
		return nil
	}
	return buf[:n]
}

// appendExtra writes the event's facts the dnstap message has no field for
// as space-separated key=value pairs: status, list, upstream, client (the
// client name as logged) and trace. A value with a space, a quote or an
// equals sign is quoted.
func appendExtra(b []byte, ev *querylog.Event) []byte {
	add := func(key, value string) {
		if value == "" {
			return
		}
		if len(b) > 0 {
			b = append(b, ' ')
		}
		b = append(b, key...)
		b = append(b, '=')
		if strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == '"' || r == '=' || r == 0x7f }) {
			b = strconv.AppendQuote(b, value)
		} else {
			b = append(b, value...)
		}
	}
	add("status", ev.Status.String())
	add("list", ev.List)
	add("upstream", ev.Upstream)
	add("client", ev.ClientName)
	if ev.Traced() {
		add("trace", hex.EncodeToString(ev.TraceID[:]))
	}
	return b
}

func socketProtocol(p pipeline.Proto) SocketProtocol {
	switch p {
	case pipeline.ProtoUDP:
		return ProtocolUDP
	case pipeline.ProtoTCP:
		return ProtocolTCP
	case pipeline.ProtoDoT:
		return ProtocolDOT
	case pipeline.ProtoDoH, pipeline.ProtoDoH3:
		return ProtocolDOH
	case pipeline.ProtoDoQ:
		return ProtocolDOQ
	}
	return 0
}
