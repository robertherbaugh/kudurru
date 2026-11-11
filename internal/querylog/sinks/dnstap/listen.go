package dnstap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	qlog "github.com/robertherbaugh/kudurru/internal/stages/log"
)

// dnstap listen (D372): a listener of listeners.dnstap accepts Frame
// Streams from front ends such as dnsdist or Unbound and turns the client
// queries their dnstap messages describe into query events of this node's
// log. Its work is bounded everywhere: the senders it serves, the size of a
// frame, the messages waiting to be paired and recorded. A connection is
// read as fast as it sends, whatever happens downstream; what does not fit
// is dropped and counted, so a slow collector never holds back a sender,
// and the collector's Enqueue never waits, so ingest never holds back DNS.
//
// A message becomes a row like this:
//   - CLIENT_QUERY/CLIENT_RESPONSE and AUTH_QUERY/AUTH_RESPONSE describe a
//     client's query to the sender. A query waits up to PairWait for its
//     response (same identity, client address and port, DNS ID, name and
//     type) and the two become one row; a response without a query is a
//     row of its own; a query without a response is one after PairWait.
//   - Every other type (FORWARDER_*, RESOLVER_*, ...) describes the
//     sender's own exchange with an upstream for a query its CLIENT_*
//     message already describes, and is counted, never recorded.
//   - A row is held for Hold before it is checked against the queries this
//     node answered itself (querylog.DedupIndex): a front end that forwards
//     to kudurru reports queries kudurru has already logged, and the one
//     with the same client address, port, name and type within the
//     message's time window cancels it.
//   - Messages with this node's own identity, with an identity the listener
//     does not accept or from an ignored client are dropped and counted.
//   - Every recorded event carries Origin, so no dnstap sink sends it again.

// Frame results of kudurru_querylog_dnstap_frames_total.
const (
	ResultRecorded      = "recorded"
	ResultDuplicate     = "duplicate"
	ResultPaired        = "paired"
	ResultSelf          = "self"
	ResultIdentity      = "identity"
	ResultIgnoredClient = "ignored_client"
	ResultType          = "type"
	ResultMalformed     = "malformed"
	ResultOversized     = "oversized"
	ResultOverflow      = "overflow"
	ResultDropped       = "dropped"
)

// Connection results of kudurru_querylog_dnstap_connections_total.
const (
	ConnAccepted  = "accepted"
	ConnPeer      = "refused_peer"
	ConnLimit     = "refused_limit"
	ConnHandshake = "handshake_failed"
	ConnBroken    = "broken"
)

// Defaults of a listener.
const (
	// MaxListenFrame bounds a data frame: a dnstap message holds at most
	// two DNS messages of 64 KiB and some short fields.
	MaxListenFrame = 192 << 10
	// skipLimit is the largest oversized frame that is read past; a larger
	// length ends the connection.
	skipLimit = 16 << 20
	// DefaultHold is how long a row waits before the dedup check, so that
	// the query this node answered reaches the index first.
	DefaultHold = time.Second
	// DefaultPairWait is how long a query message waits for its response.
	DefaultPairWait = 2 * time.Second
	// DefaultListenQueue bounds the messages waiting in one listener.
	DefaultListenQueue = 10000
	// dedupSkew widens the time window of the dedup check for a sender
	// whose clock differs a little.
	dedupSkew = 2 * time.Second
	// handshakeTimeout bounds the Frame Streams (and TLS) handshake.
	handshakeTimeout = 10 * time.Second
	// maxLatency bounds the latency taken from the two times of a message.
	maxLatency = time.Minute
	// noIdentity is the Origin of an event whose sender sent no identity.
	noIdentity = "dnstap"
)

// ListenMetrics are the instruments of every listener of a node.
type ListenMetrics struct {
	Frames      *prometheus.CounterVec
	Connections *prometheus.CounterVec
}

// NewListenMetrics creates the instruments and registers them with reg
// when reg is not nil.
func NewListenMetrics(reg prometheus.Registerer) *ListenMetrics {
	m := &ListenMetrics{
		Frames: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_querylog_dnstap_frames_total",
			Help: "dnstap data frames received by listeners.dnstap, by listener and result (recorded, paired, duplicate, self, identity, ignored_client, type, malformed, oversized, overflow, dropped).",
		}, []string{"listener", "result"}),
		Connections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_querylog_dnstap_connections_total",
			Help: "Connections to listeners.dnstap, by listener and result (accepted, refused_peer, refused_limit, handshake_failed, broken).",
		}, []string{"listener", "result"}),
	}
	if reg != nil {
		reg.MustRegister(m.Frames, m.Connections)
	}
	return m
}

// ListenOptions configure one listener.
type ListenOptions struct {
	Settings config.DnstapListener
	// Collector receives the events; its Enqueue must never block.
	Collector querylog.Collector
	// Dedup holds the queries this node answered; nil checks nothing.
	Dedup *querylog.DedupIndex
	// Self reports whether an identity is one this node sends with.
	Self func(identity string) bool
	// Node is the Node of every event.
	Node    string
	Metrics *ListenMetrics
	Logger  *slog.Logger
	// Hold, PairWait and Queue replace the defaults when positive.
	Hold, PairWait time.Duration
	Queue          int
	// Listener replaces the bind for tests; it must match the address's
	// scheme (TLS is added on top for tls:).
	Listener net.Listener
}

// Listener is one bound entry of listeners.dnstap.
type Listener struct {
	o       ListenOptions
	name    string
	target  config.DnstapTarget
	ln      net.Listener
	tls     *tls.Config
	peers   []netip.Prefix
	ignored []netip.Prefix
	slots   chan struct{}
	items   chan *ingest
	frames  map[string]prometheus.Counter
	conns   map[string]prometheus.Counter
	logger  *slog.Logger

	mu     sync.Mutex
	open   map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Listen validates o and binds the listener; Serve runs it.
func Listen(o ListenOptions) (*Listener, error) {
	target, err := o.Settings.Target()
	if err != nil {
		return nil, fmt.Errorf("listeners.dnstap: address %w", err)
	}
	if o.Collector == nil {
		return nil, errors.New("listeners.dnstap: no collector")
	}
	if o.Hold <= 0 {
		o.Hold = DefaultHold
	}
	if o.PairWait <= 0 {
		o.PairWait = DefaultPairWait
	}
	if o.Queue <= 0 {
		o.Queue = DefaultListenQueue
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Self == nil {
		o.Self = func(string) bool { return false }
	}
	if o.Metrics == nil {
		o.Metrics = NewListenMetrics(nil)
	}
	l := &Listener{o: o, name: o.Settings.Address, target: target, slots: make(chan struct{}, o.Settings.Connections()), items: make(chan *ingest, o.Queue), open: map[net.Conn]struct{}{}, logger: o.Logger.With("listener", o.Settings.Address)}
	if l.peers, err = o.Settings.AllowedPeers(); err != nil {
		return nil, fmt.Errorf("listeners.dnstap: allowFrom: %w", err)
	}
	if l.ignored, err = o.Settings.IgnoredClients(); err != nil {
		return nil, fmt.Errorf("listeners.dnstap: ignoreClients: %w", err)
	}
	l.frames, l.conns = map[string]prometheus.Counter{}, map[string]prometheus.Counter{}
	for _, r := range []string{ResultRecorded, ResultDuplicate, ResultPaired, ResultSelf, ResultIdentity, ResultIgnoredClient, ResultType, ResultMalformed, ResultOversized, ResultOverflow, ResultDropped} {
		l.frames[r] = o.Metrics.Frames.WithLabelValues(l.name, r)
	}
	for _, r := range []string{ConnAccepted, ConnPeer, ConnLimit, ConnHandshake, ConnBroken} {
		l.conns[r] = o.Metrics.Connections.WithLabelValues(l.name, r)
	}
	if target.Scheme == config.DnstapSchemeTLS {
		if l.tls, err = serverTLS(o.Settings); err != nil {
			return nil, err
		}
	}
	l.ln = o.Listener
	if l.ln == nil {
		if l.ln, err = bind(target); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func serverTLS(s config.DnstapListener) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(s.TLS.CertFile, s.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("listeners.dnstap: %s: certificate and key: %w", s.Address, err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	if s.ClientCAFile != "" {
		pem, err := os.ReadFile(s.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("listeners.dnstap: %s: clientCAFile: %w", s.Address, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("listeners.dnstap: %s: clientCAFile holds no PEM certificate", s.Address)
		}
		cfg.ClientCAs, cfg.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// bind listens on target. A Unix socket left behind by an earlier run is
// replaced; any other file at the path is refused.
func bind(target config.DnstapTarget) (net.Listener, error) {
	if target.Scheme != config.DnstapSchemeUnix {
		ln, err := net.Listen("tcp", target.Address)
		if err != nil {
			return nil, fmt.Errorf("listeners.dnstap: %w", err)
		}
		return ln, nil
	}
	path := target.Address
	if st, err := os.Lstat(path); err == nil {
		if st.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("listeners.dnstap: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("listeners.dnstap: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listeners.dnstap: %w", err)
	}
	// The sender needs write access; group membership grants it.
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // G302: a socket the sender's group writes to
		_ = ln.Close()
		return nil, fmt.Errorf("listeners.dnstap: %w", err)
	}
	return ln, nil
}

// Addr is the bound address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Name is the listener's address as configured, its metric label.
func (l *Listener) Name() string { return l.name }

// Serve accepts senders until ctx ends or Close is called, then closes
// every connection and returns once they and the ingest loop are done.
// Rows still held when it stops are not recorded.
func (l *Listener) Serve(ctx context.Context) error {
	ingestCtx, stopIngest := context.WithCancel(context.Background())
	ingestDone := make(chan struct{})
	go func() {
		defer close(ingestDone)
		l.ingest(ingestCtx)
	}()
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	var err error
	for {
		conn, acceptErr := l.ln.Accept()
		if acceptErr != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if !closed {
				var ne net.Error
				if errors.As(acceptErr, &ne) && ne.Timeout() {
					continue
				}
				err = fmt.Errorf("listeners.dnstap: %s: %w", l.name, acceptErr)
			}
			break
		}
		l.accept(conn)
	}
	_ = l.Close()
	l.wg.Wait()
	stopIngest()
	<-ingestDone
	return err
}

// Close stops accepting and closes every connection. It is idempotent.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	conns := make([]net.Conn, 0, len(l.open))
	for c := range l.open {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	err := l.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	if l.target.Scheme == config.DnstapSchemeUnix && l.o.Listener == nil {
		_ = os.Remove(l.target.Address)
	}
	return err
}

func (l *Listener) accept(conn net.Conn) {
	if l.target.Scheme != config.DnstapSchemeUnix && !l.allowed(conn.RemoteAddr()) {
		l.conns[ConnPeer].Inc()
		_ = conn.Close()
		return
	}
	select {
	case l.slots <- struct{}{}:
	default:
		l.conns[ConnLimit].Inc()
		_ = conn.Close()
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		<-l.slots
		_ = conn.Close()
		return
	}
	l.open[conn] = struct{}{}
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		defer func() { <-l.slots }()
		defer func() {
			l.mu.Lock()
			delete(l.open, conn)
			l.mu.Unlock()
			_ = conn.Close()
		}()
		l.serveConn(conn)
	}()
}

// allowed reports whether a tcp or tls peer is in allowFrom. A tls
// listener without allowFrom relies on its client certificates.
func (l *Listener) allowed(addr net.Addr) bool {
	if len(l.peers) == 0 {
		return l.target.Scheme == config.DnstapSchemeTLS && l.tls.ClientAuth == tls.RequireAndVerifyClientCert
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, p := range l.peers {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// serveConn completes the handshake and reads data frames until the
// sender stops or the connection breaks.
func (l *Listener) serveConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if l.tls != nil {
		tc := tls.Server(conn, l.tls)
		if err := tc.Handshake(); err != nil {
			l.conns[ConnHandshake].Inc()
			return
		}
		conn = tc
	}
	reader := NewReader(conn)
	reader.MaxData, reader.SkipOversized = MaxListenFrame, skipLimit
	bidirectional, err := handshake(conn, reader)
	if err != nil {
		l.conns[ConnHandshake].Inc()
		l.logger.Debug("dnstap sender refused at the handshake", "remote", conn.RemoteAddr().String(), "err", err)
		return
	}
	l.conns[ConnAccepted].Inc()
	// Senders stay connected while they have nothing to send.
	_ = conn.SetDeadline(time.Time{})
	var buf []byte
	for {
		data, control, err := reader.Next(buf[:0])
		switch {
		case errors.Is(err, ErrFrameSkipped):
			l.frames[ResultOversized].Inc()
			continue
		case errors.Is(err, ErrFrameTooLarge):
			l.frames[ResultOversized].Inc()
			l.conns[ConnBroken].Inc()
			return
		case errors.Is(err, io.EOF):
			return // the sender went away between frames
		case err != nil:
			l.conns[ConnBroken].Inc()
			return
		}
		if control != nil {
			if control.Type == ControlStop {
				if bidirectional {
					_ = conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
					_, _ = conn.Write(AppendControl(nil, ControlFinish))
				}
				return
			}
			l.conns[ConnBroken].Inc()
			return
		}
		if cap(data) > cap(buf) {
			buf = data
		}
		l.receive(data)
	}
}

// handshake reads READY (bidirectional) or START (unidirectional) and
// answers READY with ACCEPT.
func handshake(conn net.Conn, reader *Reader) (bidirectional bool, err error) {
	_, first, err := reader.Next(nil)
	if err != nil {
		return false, err
	}
	if first == nil {
		return false, errors.New("a data frame before the handshake")
	}
	offers := func(c *Control) bool { return len(c.ContentTypes) == 0 || c.HasContentType(ContentType) }
	switch first.Type {
	case ControlStart:
		if !offers(first) {
			return false, errors.New("the sender does not send dnstap")
		}
		return false, nil
	case ControlReady:
		if !offers(first) {
			return false, errors.New("the sender does not offer dnstap")
		}
		if _, err := conn.Write(AppendControl(nil, ControlAccept, ContentType)); err != nil {
			return false, err
		}
		start, err := reader.ReadControl(ControlStart)
		if err != nil {
			return false, err
		}
		if !offers(&start) {
			return false, errors.New("the sender starts a content type other than dnstap")
		}
		return true, nil
	}
	return false, fmt.Errorf("control frame %d before START", first.Type)
}

// pairKey identifies a query and its response from one sender.
type pairKey struct {
	identity string
	query    querylog.DedupKey
	id       uint16
}

// ingest is one decoded client message on its way to the log.
type ingest struct {
	key      pairKey
	event    *querylog.Event
	query    bool // a query message, waiting for its response
	received time.Time
	// queryTime and responseTime bound the dedup window.
	queryTime, responseTime time.Time
	deadline                time.Time
	done                    bool // paired or flushed out of the FIFO
}

// receive decodes one data frame and hands it to the ingest loop. It runs
// on the connection's goroutine and never waits.
func (l *Listener) receive(payload []byte) {
	var f Frame
	if err := DecodeFrame(payload, &f); err != nil {
		l.frames[ResultMalformed].Inc()
		return
	}
	m := &f.Message
	query := false
	switch m.Type {
	case ClientQuery, AuthQuery:
		query = true
	case ClientResponse, AuthResponse:
	default:
		l.frames[ResultType].Inc()
		return
	}
	identity := string(f.Identity)
	if identity != "" && l.o.Self(identity) {
		l.frames[ResultSelf].Inc()
		return
	}
	if len(l.o.Settings.Identities) > 0 && !slices.Contains(l.o.Settings.Identities, identity) {
		l.frames[ResultIdentity].Inc()
		return
	}
	ev, id, ok := l.event(&f, identity)
	if !ok {
		l.frames[ResultMalformed].Inc()
		return
	}
	for _, p := range l.ignored {
		if ev.Client.IsValid() && p.Contains(ev.Client) {
			l.frames[ResultIgnoredClient].Inc()
			return
		}
	}
	it := &ingest{key: pairKey{identity: identity, query: querylog.NewDedupKey(ev.Client, ev.ClientPort, ev.QType, ev.QClass, ev.Name), id: id}, event: ev, query: query, received: time.Now(), queryTime: m.QueryTime, responseTime: m.ResponseTime}
	select {
	case l.items <- it:
	default:
		l.frames[ResultOverflow].Inc()
	}
}

// event builds the query event of a message: the question and the header
// flags from the query (or the response), the outcome from the response.
// It reports false when neither DNS message holds one question.
func (l *Listener) event(f *Frame, identity string) (*querylog.Event, uint16, bool) {
	m := &f.Message
	var q, r *dns.Msg
	if len(m.QueryMessage) > 0 {
		if q = unpack(m.QueryMessage); q == nil {
			return nil, 0, false
		}
	}
	if len(m.ResponseMessage) > 0 {
		if r = unpack(m.ResponseMessage); r == nil {
			return nil, 0, false
		}
	}
	if m.Type == ClientResponse || m.Type == AuthResponse {
		if r == nil {
			return nil, 0, false
		}
	} else if q == nil {
		return nil, 0, false
	}
	header := q
	if header == nil {
		header = r
	}
	if len(header.Question) != 1 {
		return nil, 0, false
	}
	question := header.Question[0]
	origin := identity
	if origin == "" {
		origin = noIdentity
	}
	ev := &querylog.Event{
		Time: m.QueryTime, Node: l.o.Node, Origin: origin, Proto: proto(m.SocketProtocol),
		Name: strings.ToLower(question.Name), QType: question.Qtype, QClass: question.Qclass,
		Status: pipeline.StatusUnknown, ReplyType: querylog.ReplyUnknown,
	}
	if ev.Time.IsZero() {
		ev.Time = m.ResponseTime
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if a, ok := netip.AddrFromSlice(m.QueryAddress); ok {
		ev.Client = a.Unmap()
		ev.ClientPort = m.QueryPort
	}
	if (m.Type == AuthQuery || m.Type == AuthResponse) && len(m.QueryZone) > 0 {
		if zone, _, err := dns.UnpackDomainName(m.QueryZone, 0); err == nil {
			ev.Zone = strings.TrimSuffix(strings.ToLower(zone), ".")
		}
	}
	applyQuery(ev, header)
	if r != nil {
		applyResponse(ev, r, m)
	}
	return ev, header.Id, true
}

func unpack(b []byte) *dns.Msg {
	m := new(dns.Msg)
	if err := m.Unpack(b); err != nil {
		return nil
	}
	return m
}

func applyQuery(ev *querylog.Event, q *dns.Msg) {
	ev.Flags.RD, ev.Flags.CD = q.RecursionDesired, q.CheckingDisabled
	if opt := q.IsEdns0(); opt != nil {
		ev.Flags.DO = opt.Do()
		for _, o := range opt.Option {
			if sn, ok := o.(*dns.EDNS0_SUBNET); ok {
				if a, ok := netip.AddrFromSlice(sn.Address); ok {
					if p, err := a.Unmap().Prefix(int(sn.SourceNetmask)); err == nil {
						ev.ECS, ev.Flags.ECS = p, true
					}
				}
			}
		}
	}
}

func applyResponse(ev *querylog.Event, r *dns.Msg, m *Message) {
	ev.Rcode = r.Rcode
	ev.Answers = len(r.Answer)
	ev.ReplyType = qlog.ReplyType(ev.QType, r)
	ev.Answer = qlog.AnswerSummary(r.Answer)
	ev.Flags.TC, ev.Flags.AD = r.Truncated, r.AuthenticatedData
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if e, ok := o.(*dns.EDNS0_EDE); ok {
				ev.EDE, ev.HasEDE = e.InfoCode, true
				break
			}
		}
	}
	if !m.QueryTime.IsZero() && !m.ResponseTime.IsZero() {
		if d := m.ResponseTime.Sub(m.QueryTime); d >= 0 && d <= maxLatency {
			ev.Latency = d
		}
	}
}

func proto(p SocketProtocol) pipeline.Proto {
	switch p {
	case ProtocolTCP, 6: // 6 is DNSCryptTCP
		return pipeline.ProtoTCP
	case ProtocolDOT:
		return pipeline.ProtoDoT
	case ProtocolDOH:
		return pipeline.ProtoDoH
	case ProtocolDOQ:
		return pipeline.ProtoDoQ
	}
	return pipeline.ProtoUDP
}

// ingest pairs queries with their responses, holds every row for Hold and
// records what the dedup index does not claim. It is the only goroutine
// that touches the pairing state.
func (l *Listener) ingest(ctx context.Context) {
	waiting := map[pairKey]*ingest{}
	var queries, rows fifo
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	hold := func(it *ingest, now time.Time) {
		if rows.len() >= l.o.Queue {
			l.frames[ResultOverflow].Inc()
			return
		}
		it.deadline = now.Add(l.o.Hold)
		rows.push(it)
	}
	flushQuery := func(it *ingest, now time.Time) {
		delete(waiting, it.key)
		it.done = true
		// The query waited at least PairWait, longer than Hold.
		if rows.len() >= l.o.Queue {
			l.frames[ResultOverflow].Inc()
			return
		}
		row := *it
		row.done, row.deadline = false, now
		rows.push(&row)
	}
	for {
		now := time.Now()
		for it := queries.peek(); it != nil && (it.done || !it.deadline.After(now)); it = queries.peek() {
			queries.pop()
			if !it.done {
				flushQuery(it, now)
			}
		}
		for it := rows.peek(); it != nil && !it.deadline.After(now); it = rows.peek() {
			rows.pop()
			l.record(it)
		}
		next := time.Hour
		if it := queries.peek(); it != nil {
			next = min(next, it.deadline.Sub(now))
		}
		if it := rows.peek(); it != nil {
			next = min(next, it.deadline.Sub(now))
		}
		timer.Reset(max(next, time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case it := <-l.items:
			now = time.Now()
			if it.query {
				if old := waiting[it.key]; old != nil {
					flushQuery(old, now) // a repeated query: the first is answered no more
				}
				if len(waiting) >= l.o.Queue {
					// The oldest waiting query goes to the log unanswered.
					for old := queries.peek(); old != nil; old = queries.peek() {
						queries.pop()
						if !old.done {
							flushQuery(old, now)
							break
						}
					}
				}
				it.deadline = it.received.Add(l.o.PairWait)
				waiting[it.key] = it
				queries.push(it)
				continue
			}
			if q := waiting[it.key]; q != nil {
				delete(waiting, it.key)
				q.done = true
				l.frames[ResultPaired].Inc()
				merged := *q.event
				resp := it.event
				merged.Rcode, merged.Answers, merged.ReplyType, merged.Answer = resp.Rcode, resp.Answers, resp.ReplyType, resp.Answer
				merged.Flags.TC, merged.Flags.AD, merged.EDE, merged.HasEDE = resp.Flags.TC, resp.Flags.AD, resp.EDE, resp.HasEDE
				if !it.responseTime.IsZero() && !q.queryTime.IsZero() {
					if d := it.responseTime.Sub(q.queryTime); d >= 0 && d <= maxLatency {
						merged.Latency = d
					}
				} else {
					merged.Latency = resp.Latency
				}
				row := &ingest{key: it.key, event: &merged, received: it.received, queryTime: q.queryTime, responseTime: it.responseTime}
				hold(row, now)
				continue
			}
			hold(it, now)
		}
	}
}

// record checks a row against the queries this node answered and enqueues
// it when none claims it.
func (l *Listener) record(it *ingest) {
	ev := it.event
	from, to := it.queryTime, it.responseTime
	if from.IsZero() {
		from = ev.Time
	}
	if to.IsZero() || to.Before(from) {
		to = from
	}
	if l.o.Dedup.Claim(it.key.query, from.Add(-dedupSkew), to.Add(dedupSkew)) {
		l.frames[ResultDuplicate].Inc()
		return
	}
	if l.o.Collector.Enqueue(ev) {
		l.frames[ResultRecorded].Inc()
	} else {
		l.frames[ResultDropped].Inc()
	}
}

// fifo is a queue of ingests in deadline order (every entry of one queue
// waits the same time).
type fifo struct {
	items []*ingest
	head  int
}

func (q *fifo) push(it *ingest) { q.items = append(q.items, it) }

func (q *fifo) len() int { return len(q.items) - q.head }

func (q *fifo) peek() *ingest {
	if q.head == len(q.items) {
		return nil
	}
	return q.items[q.head]
}

func (q *fifo) pop() {
	q.items[q.head] = nil
	q.head++
	if q.head == len(q.items) {
		q.items, q.head = q.items[:0], 0
	} else if q.head > 1024 && q.head*2 > len(q.items) {
		q.items, q.head = append(q.items[:0], q.items[q.head:]...), 0
	}
}
