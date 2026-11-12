package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// edgeClientIDImage is the pinned dnsdist 2.1.3 image of the live edge tests.
const edgeClientIDImage = "sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e"

// TestRunServeEdgeClientIDLiveDnsdist runs the pinned dnsdist 2.1.3 image
// with the configuration kudurru renders (YAML and Lua) in front of a real
// runServe (D389). The same client ID over native DoT, DoQ, DoH and DoH3 and
// over the edge's DoT, DoQ, DoH and DoH3 selects the same ClientGroup and
// policy: the phone group's block list answers NXDOMAIN, any other or no ID
// gets the upstream's answer. Paths a kudurru listener refuses get HTTP 404
// from both. A client-ID TLV without the shared secret, or with a wrong one,
// is still dropped by the backend. An ID-carrying query is never answered
// from the edge packet cache: once the edge has cached blocked.example
// without an ID, the phone ID still gets NXDOMAIN.
func TestRunServeEdgeClientIDLiveDnsdist(t *testing.T) {
	if os.Getenv("KUDURRU_EDGE_LIVE") != "1" {
		t.Skip("set KUDURRU_EDGE_LIVE=1 and KUDURRU_EDGE_LIVE_WORKDIR for the pinned dnsdist client-ID acceptance")
	}
	parent := os.Getenv("KUDURRU_EDGE_LIVE_WORKDIR")
	if !filepath.IsAbs(parent) {
		t.Fatal("an absolute scratch parent the Docker daemon can read is required")
	}
	dir, err := os.MkdirTemp(parent, "edge-client-id-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })  //nolint:gosec // the directory MkdirTemp just made
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the daemon reads the config as another user
		t.Fatal(err)
	}
	roots := writeEdgeClientIDCert(t, dir)
	files := config.TLSFiles{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}

	upstream := startEdgeClientIDUpstream(t)
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"public=" + upstream.addr}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	secret := []byte(strings.Repeat("5e", 20))
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	const domain = "dot.example.com"
	cfg.Listeners.DoT = []config.TLSListener{{Address: offlineFreeAddress(t, "tcp"), TLS: files, ClientIDDomain: domain}}
	cfg.Listeners.DoQ = []config.TLSListener{{Address: offlineFreeAddress(t, "udp"), TLS: files, ClientIDDomain: domain}}
	cfg.Listeners.DoH = []config.HTTPListener{{Address: offlineFreeAddress(t, "tcp"), Path: "/dns-query", TLS: files, ClientIDs: true}}
	cfg.Listeners.DoH3 = []config.HTTPListener{{Address: offlineFreeAddress(t, "udp"), Path: "/dns-query", TLS: files, ClientIDs: true}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	seedEdgeClientIDPolicy(t, cfg)
	stop := startStaticMount(t, cfg)
	defer stop()

	native := edgeClientIDTargets{side: "native", dot: cfg.Listeners.DoT[0].Address, doq: cfg.Listeners.DoQ[0].Address, doh: cfg.Listeners.DoH[0].Address, doh3: cfg.Listeners.DoH3[0].Address}
	cases := []struct {
		id   string
		want int
	}{{"phone", dns.RcodeNameError}, {"PHONE", dns.RcodeNameError}, {"laptop", dns.RcodeSuccess}, {"", dns.RcodeSuccess}}
	check := func(t *testing.T, label string, targets edgeClientIDTargets) {
		t.Helper()
		for _, c := range cases {
			for proto, rcode := range targets.exchange(t, roots, c.id, "blocked.example.") {
				if rcode != c.want {
					t.Errorf("%s %s with ID %q: rcode %s, want %s", label, proto, c.id, dns.RcodeToString[rcode], dns.RcodeToString[c.want])
				}
			}
		}
		if label == "edge" {
			// The no-ID answers above (TTL 300) are now in the edge packet
			// cache: asking again reaches neither kudurru nor its upstream.
			// The phone ID must still get the phone group's NXDOMAIN, so
			// it may not be answered from that cache (dq.skipCache, D389).
			before := upstream.queries("blocked.example.")
			for proto, rcode := range targets.exchange(t, roots, "", "blocked.example.") {
				if rcode != dns.RcodeSuccess {
					t.Errorf("edge %s cached no-ID answer: rcode %s", proto, dns.RcodeToString[rcode])
				}
			}
			if after := upstream.queries("blocked.example."); after != before {
				t.Fatalf("the edge did not answer the repeated no-ID queries from its cache: %d upstream queries, want %d", after, before)
			}
			for proto, rcode := range targets.exchange(t, roots, "phone", "blocked.example.") {
				if rcode != dns.RcodeNameError {
					t.Errorf("edge %s with ID phone after a cached no-ID answer: rcode %s, want NXDOMAIN", proto, dns.RcodeToString[rcode])
				}
			}
		}
		// A FIN in its own empty STREAM frame ends a QUIC request stream
		// (RFC 9000 section 19.8). kudurru answers such a request; the
		// pinned dnsdist 2.1.3 never does (known issue, see quicAttempt).
		for _, proto := range []string{"DoQ", "DoH3"} {
			answered := targets.finAloneAnswered(t, roots, proto)
			if label == "native" && !answered {
				t.Errorf("native %s did not answer a request whose FIN came alone", proto)
			}
			if label == "edge" && answered {
				t.Errorf("the edge now answers a %s request whose FIN came alone: the dnsdist defect is fixed, drop the retry in quicAttempt and the known issue", proto)
			}
		}
		// Two labels before the suffix are no ID, as in clientid.FromSNI.
		if rcode := targets.dotSNI(t, roots, "a.phone."+domain, "blocked.example."); rcode != dns.RcodeSuccess {
			t.Errorf("%s DoT with a two-label ID: rcode %s", label, dns.RcodeToString[rcode])
		}
		for _, path := range []string{"/dns-query/", "/dns-query/phone/extra", "/dns-query/ph%6fne", "/dns-query/phone%2Fextra", "/dns-query/-phone"} {
			for proto, status := range targets.httpStatus(t, roots, path) {
				if status != http.StatusNotFound {
					t.Errorf("%s %s path %s: HTTP %d, want 404", label, proto, path, status)
				}
			}
		}
	}
	check(t, "native", native)

	for _, renderer := range []dnsdist.Renderer{dnsdist.YAMLRenderer{}, dnsdist.LuaRenderer{}} {
		t.Run(renderer.Name(), func(t *testing.T) {
			edgeTargets := runEdgeClientIDDnsdist(t, dir, renderer, cfg, secret)
			check(t, "edge", edgeTargets)
		})
	}

	// The backend still drops an ID TLV that does not come with the secret.
	backend := cfg.Listeners.DNS[0].Address
	if rcode, ok := proxiedUDPExchange(t, backend, proxyproto.AppendTLV(proxyproto.AppendTLV(nil, proxyproto.TLVTypeSecret, secret), proxyproto.TLVTypeClientID, []byte("phone"))); !ok || rcode != dns.RcodeNameError {
		t.Fatalf("authenticated ID TLV: answered %v rcode %s", ok, dns.RcodeToString[rcode])
	}
	for name, tlvs := range map[string][]byte{
		"ID without secret":  proxyproto.AppendTLV(nil, proxyproto.TLVTypeClientID, []byte("phone")),
		"ID with bad secret": proxyproto.AppendTLV(proxyproto.AppendTLV(nil, proxyproto.TLVTypeSecret, bytes.Repeat([]byte{'x'}, len(secret))), proxyproto.TLVTypeClientID, []byte("phone")),
	} {
		if rcode, ok := proxiedUDPExchange(t, backend, tlvs); ok {
			t.Errorf("%s: answered with rcode %s, want a drop", name, dns.RcodeToString[rcode])
		}
	}
}

// seedEdgeClientIDPolicy stores the phone ClientGroup (clientID phone) and a
// policy that blocks blocked.example for it.
func seedEdgeClientIDPolicy(t *testing.T, cfg *config.Config) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	for _, obj := range []store.Object{
		&public.ClientGroup{ObjectMeta: meta("phone"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{ClientID: "phone"}}}},
		&public.BlockList{ObjectMeta: meta("phone-blocks"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"blocked.example"}, Format: "domains"}}}},
		&public.Policy{ObjectMeta: meta("phone"), Spec: public.PolicySpec{Priority: 10, ClientGroupRefs: []string{"phone"}, BlockListRefs: []string{"phone-blocks"}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// runEdgeClientIDDnsdist renders the edge with client IDs for the runServe
// backend and starts the pinned dnsdist on it; it returns the edge frontends
// once the edge answers.
func runEdgeClientIDDnsdist(t *testing.T, dir string, renderer dnsdist.Renderer, cfg *config.Config, secret []byte) edgeClientIDTargets {
	t.Helper()
	// dnsdist binds Do53 on UDP and TCP: every port is free on both and
	// none is used twice.
	used := map[int]bool{}
	port := func() int {
		for {
			_, p, _ := net.SplitHostPort(offlineFreeAddress(t, "tcp"))
			n, _ := strconv.Atoi(p)
			if used[n] {
				continue
			}
			if pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", p)); err == nil {
				_ = pc.Close()
				used[n] = true
				return n
			}
		}
	}
	ports := map[edge.Protocol]int{edge.Do53: port(), edge.DoT: port(), edge.DoH: port(), edge.DoQ: port(), edge.DoH3: port()}
	creds, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	top, err := edge.Build(edge.Options{
		ServerID: "edge-client-id", Listen: []string{"127.0.0.1"}, Protocols: edge.AllProtocols, Ports: ports,
		CertFile: "/cfg/tls.crt", KeyFile: "/cfg/tls.key", PoolResolver: "firstAvailable",
		API: net.JoinHostPort("127.0.0.1", strconv.Itoa(port())), Console: net.JoinHostPort("127.0.0.1", strconv.Itoa(port())), ProxyProtocolSecret: string(secret),
		ClientIDs: edge.ClientIDs{Domain: "dot.example.com", Paths: true},
	}, []edge.Backend{{Name: cfg.Node.Name, Address: cfg.Listeners.DNS[0].Address}}, creds)
	if err != nil {
		t.Fatal(err)
	}
	// Static rendering leaves the resolver pool uncached; the supervisor
	// attaches this cache at run time once it is qualified. Attach it here
	// so that the cache skip of ID-carrying queries is exercised.
	cached := false
	for i := range top.Pools {
		if top.Pools[i].Name == "resolver" {
			top.Pools[i].Cache, cached = edge.DefaultResolverCache(), true
		}
	}
	if !cached {
		t.Fatal("the edge topology has no resolver pool")
	}
	rendered, err := renderer.Render(top)
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, renderer.Name())
	if err := os.Mkdir(conf, 0o755); err != nil { //nolint:gosec // read by the daemon's user
		t.Fatal(err)
	}
	for _, f := range rendered {
		if err := os.WriteFile(filepath.Join(conf, f.Name), f.Data, 0o644); err != nil { //nolint:gosec // read by the daemon's user; fixture credentials
			t.Fatal(err)
		}
	}
	name := "edge-client-id-live-" + renderer.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	docker := func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append([]string{"--host", "unix:///tmp/kudurru-lab-docker.sock"}, args...)...).CombinedOutput() //nolint:gosec // fixed test daemon, owned fixture
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if t.Failed() {
			logs, _ := docker(ctx, "logs", "--tail", "40", name)
			t.Logf("dnsdist log tail:\n%s", logs)
		}
		_, _ = docker(ctx, "rm", "--force", name)
	})
	if out, err := docker(t.Context(), "run", "-d", "--name", name, "--network", "host", "-v", dir+":/cfg:ro", "--entrypoint", "dnsdist", edgeClientIDImage,
		"--supervised", "--disable-syslog", "-C", "/cfg/"+renderer.Name()+"/"+renderer.Main()); err != nil {
		t.Fatalf("start dnsdist: %v %s", err, out)
	}
	host := func(p edge.Protocol) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[p])) }
	q := new(dns.Msg)
	q.SetQuestion("ready.example.", dns.TypeA)
	deadline := time.Now().Add(30 * time.Second)
	for {
		answer, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(q, host(edge.Do53))
		if err == nil && answer.Rcode == dns.RcodeSuccess && len(answer.Answer) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the edge never answered: %v %v", err, answer)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return edgeClientIDTargets{side: "edge " + renderer.Name(), dot: host(edge.DoT), doq: host(edge.DoQ), doh: host(edge.DoH), doh3: host(edge.DoH3)}
}

// edgeClientIDTargets are the four TLS transports of one side (native or edge).
type edgeClientIDTargets struct{ side, dot, doq, doh, doh3 string }

func edgeClientIDTLS(roots *x509.CertPool, server string, protos ...string) *tls.Config {
	return &tls.Config{RootCAs: roots, ServerName: server, NextProtos: protos, MinVersion: tls.VersionTLS12}
}

// exchange asks name over every transport with the ID in the server name
// (DoT, DoQ) or the path (DoH, DoH3) and returns each rcode.
func (x edgeClientIDTargets) exchange(t *testing.T, roots *x509.CertPool, id, name string) map[string]int {
	t.Helper()
	server, path := "dns.example.com", "/dns-query"
	if id != "" {
		server, path = id+".dot.example.com", "/dns-query/"+id
	}
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	q.Id = 0
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{"DoT": x.dotSNI(t, roots, server, name)}
	var body []byte
	x.quicAttempt(t, "DoQ "+x.doq+" as "+server, func(ctx context.Context, watch *quicFINWatch) error {
		var err error
		body, err = x.doqQuery(ctx, roots, server, wire, watch, false)
		return err
	})
	answer := new(dns.Msg)
	if len(body) < 2 || answer.Unpack(body[2:]) != nil {
		t.Fatalf("DoQ %s with ID %q: %x", x.doq, id, body)
	}
	out["DoQ"] = answer.Rcode
	for _, proto := range []string{"DoH", "DoH3"} {
		resp, address := x.post(t, roots, proto, path, wire)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answer := new(dns.Msg)
		if resp.StatusCode != http.StatusOK || answer.Unpack(body) != nil {
			t.Fatalf("%s %s%s: %s", proto, address, path, resp.Status)
		}
		out[proto] = answer.Rcode
	}
	return out
}

// doqQuery sends wire on a fresh DoQ connection and returns the
// length-prefixed answer. finAlone holds the stream FIN back for 200 ms so
// that it leaves in its own empty STREAM frame.
func (x edgeClientIDTargets) doqQuery(ctx context.Context, roots *x509.CertPool, server string, wire []byte, watch *quicFINWatch, finAlone bool) ([]byte, error) {
	conn, err := quic.DialAddr(ctx, x.doq, edgeClientIDTLS(roots, server, "doq"), &quic.Config{Tracer: watch.tracer})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.CloseWithError(0, "") }()
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseWithError(0, "canceled") })
	defer stop()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := stream.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(wire))), wire...)); err != nil { //nolint:gosec // a small fixture message
		return nil, err
	}
	if finAlone {
		time.Sleep(200 * time.Millisecond)
	}
	_ = stream.Close()
	return io.ReadAll(stream)
}

// quicFINWatch is a quic-go tracer that records whether the client sent a
// stream FIN in an empty STREAM frame, apart from the stream's data.
type quicFINWatch struct{ alone atomic.Bool }

func (w *quicFINWatch) tracer(context.Context, bool, quic.ConnectionID) qlogwriter.Trace { return w }
func (w *quicFINWatch) AddProducer() qlogwriter.Recorder                                 { return w }
func (w *quicFINWatch) SupportsSchemas(string) bool                                      { return false }
func (w *quicFINWatch) Close() error                                                     { return nil }
func (w *quicFINWatch) RecordEvent(e qlogwriter.Event) {
	var frames []qlog.Frame
	switch p := e.(type) {
	case qlog.PacketSent:
		frames = p.Frames
	case *qlog.PacketSent:
		frames = p.Frames
	}
	for _, f := range frames {
		if s, ok := f.Frame.(*qlog.StreamFrame); ok && s.Fin && s.Length == 0 {
			w.alone.Store(true)
		}
	}
}

// quicAttempt runs one DoQ or DoH3 request on a fresh connection per
// attempt. The pinned dnsdist 2.1.3 (and the upstream 2.1.3 image) never
// answers a QUIC request whose FIN arrives in its own empty STREAM frame
// rather than with the data; quic-go sends it that way when the data leaves
// before the stream is closed (evidence fix-probe03 and fix-probe04: every
// lost edge request, and only those, had the FIN alone; none reached the
// backend). Only on the edge, and only when the tracer saw the FIN go
// alone, is such a request cut off after 3 s and tried again on a fresh
// connection. Any other failure, and every native failure, is final.
func (x edgeClientIDTargets) quicAttempt(t *testing.T, what string, do func(context.Context, *quicFINWatch) error) {
	t.Helper()
	const attempts = 6
	edgeSide := strings.HasPrefix(x.side, "edge")
	for attempt := 1; ; attempt++ {
		watch := new(quicFINWatch)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cut := time.AfterFunc(3*time.Second, func() {
			if edgeSide && watch.alone.Load() {
				cancel()
			}
		})
		err := do(ctx, watch)
		cut.Stop()
		cancel()
		if err == nil {
			return
		}
		if !edgeSide || !watch.alone.Load() || attempt == attempts {
			t.Fatalf("%s %s (attempt %d, FIN alone %v): %v", x.side, what, attempt, watch.alone.Load(), err)
		}
		t.Logf("%s %s: the FIN left alone and the pinned dnsdist did not answer (known issue); one more fresh connection", x.side, what)
	}
}

// finAloneAnswered sends one DoQ or DoH3 query whose FIN leaves 200 ms after
// its data and reports whether it was answered within 3 s.
func (x edgeClientIDTargets) finAloneAnswered(t *testing.T, roots *x509.CertPool, proto string) bool {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("fin-alone.example.", dns.TypeA)
	q.Id = 0
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	watch := new(quicFINWatch)
	if proto == "DoQ" {
		_, err = x.doqQuery(ctx, roots, "dns.example.com", wire, watch, true)
	} else {
		var resp *http.Response
		resp, err = x.h3Post(ctx, roots, "/dns-query", wire, watch, true)
		if err == nil && resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s with the FIN alone: %s", x.side, proto, resp.Status)
		}
	}
	if !watch.alone.Load() {
		t.Fatalf("%s %s: the fixture did not send the FIN alone", x.side, proto)
	}
	if err != nil {
		t.Logf("%s %s with the FIN alone: %v", x.side, proto, err)
	}
	return err == nil
}

// h3Post posts wire to path on a fresh HTTP/3 connection and returns the
// response with its body already read; finAlone ends the body (the stream
// FIN) 200 ms after its data.
func (x edgeClientIDTargets) h3Post(ctx context.Context, roots *x509.CertPool, path string, wire []byte, watch *quicFINWatch, finAlone bool) (*http.Response, error) {
	h3 := &http3.Transport{TLSClientConfig: edgeClientIDTLS(roots, "dns.example.com"), QUICConfig: &quic.Config{Tracer: watch.tracer}}
	defer func() { _ = h3.Close() }()
	var body io.Reader = bytes.NewReader(wire)
	if finAlone {
		r, w := io.Pipe()
		go func() {
			_, _ = w.Write(wire)
			time.Sleep(200 * time.Millisecond)
			_ = w.Close()
		}()
		body = r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+x.doh3+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	resp, err := h3.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

func (x edgeClientIDTargets) dotSNI(t *testing.T, roots *x509.CertPool, server, name string) int {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	client := &dns.Client{Net: "tcp-tls", TLSConfig: edgeClientIDTLS(roots, server), Timeout: 3 * time.Second}
	answer, _, err := client.ExchangeContext(t.Context(), q, x.dot)
	if err != nil {
		t.Fatalf("DoT %s as %s: %v", x.dot, server, err)
	}
	return answer.Rcode
}

// post sends wire to path over a fresh DoH or DoH3 connection. A response
// is always final; DoH3 transport errors follow quicAttempt.
func (x edgeClientIDTargets) post(t *testing.T, roots *x509.CertPool, proto, path string, wire []byte) (*http.Response, string) {
	t.Helper()
	if proto == "DoH3" {
		var resp *http.Response
		x.quicAttempt(t, "DoH3 "+x.doh3+path, func(ctx context.Context, watch *quicFINWatch) error {
			var err error
			resp, err = x.h3Post(ctx, roots, path, wire, watch, false)
			return err
		})
		return resp, x.doh3
	}
	h2 := &http.Transport{TLSClientConfig: edgeClientIDTLS(roots, "dns.example.com"), ForceAttemptHTTP2: true}
	t.Cleanup(h2.CloseIdleConnections)
	resp, err := (&http.Client{Transport: h2, Timeout: 10 * time.Second}).Post("https://"+x.doh+path, "application/dns-message", bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("DoH %s%s: %v", x.doh, path, err)
	}
	return resp, x.doh
}

// httpStatus posts a query to path over DoH and DoH3.
func (x edgeClientIDTargets) httpStatus(t *testing.T, roots *x509.CertPool, path string) map[string]int {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	wire, _ := q.Pack()
	out := map[string]int{}
	for _, proto := range []string{"DoH", "DoH3"} {
		resp, _ := x.post(t, roots, proto, path, wire)
		_ = resp.Body.Close()
		out[proto] = resp.StatusCode
	}
	return out
}

// edgeClientIDUpstream answers every A query with TTL 300, so that the edge
// packet cache keeps no-ID answers, and counts the queries per name.
type edgeClientIDUpstream struct {
	addr string
	mu   sync.Mutex
	hits map[string]int
}

func startEdgeClientIDUpstream(t *testing.T) *edgeClientIDUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &edgeClientIDUpstream{addr: pc.LocalAddr().String(), hits: map[string]int{}}
	started := make(chan struct{})
	srv := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		name := q.Question[0].Name
		u.mu.Lock()
		u.hits[name]++
		u.mu.Unlock()
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 53)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return u
}

func (u *edgeClientIDUpstream) queries(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[name]
}

// proxiedUDPExchange sends blocked.example over UDP behind a PROXYv2 header
// with the given TLVs; ok is false when the backend does not answer.
func proxiedUDPExchange(t *testing.T, backend string, tlvs []byte) (rcode int, ok bool) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("blocked.example.", dns.TypeA)
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	h := proxyproto.Header{Command: proxyproto.CommandProxy, Family: proxyproto.FamilyINET, Transport: proxyproto.TransportDatagram,
		Source: netip.MustParseAddrPort("192.0.2.77:40000"), Destination: netip.MustParseAddrPort(backend), RawTLVs: tlvs}
	header, err := h.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("udp", backend)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(append(header, wire...)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, false
	}
	answer := new(dns.Msg)
	if err := answer.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	return answer.Rcode, true
}

// writeEdgeClientIDCert writes a self-signed certificate for
// *.dot.example.com, *.phone.dot.example.com, dns.example.com and 127.0.0.1 into dir (tls.crt and
// tls.key, readable by the daemon's user) and returns its pool.
func writeEdgeClientIDCert(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dns.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"*.dot.example.com", "*.phone.dot.example.com", "dns.example.com"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // read by the daemon's user
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o644); err != nil { //nolint:gosec // a throwaway fixture key the daemon's user reads
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return roots
}
