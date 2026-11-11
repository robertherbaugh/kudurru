// Package sinktest provides local HTTP doubles that stand where ClickHouse
// and Loki would in tests of the querylog.sinks exporters (D371). A double
// records every request with its body decompressed and answers as the test
// says: a status, a stalled answer, a dropped connection or an answer given
// before the body was read.
package sinktest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Request is one request a double received.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	// Body is the request body after Content-Encoding was undone.
	Body []byte
	// Compressed reports whether the body arrived gzip-encoded.
	Compressed bool
	// CloseAfter reports whether the client asked for the connection to be
	// closed after this request.
	CloseAfter bool
	// SentAfterAnswer is, for a double set with AnswerBeforeBody, how many
	// bytes the client sent after the answer had been written (the body it
	// went on writing). It is zero when the client waited for the answer.
	SentAfterAnswer int
}

// Answer is what a double does with one request.
type Answer struct {
	// Status of the answer; 0 means the double's default.
	Status int
	Header http.Header
	Body   string
	// Hang keeps the request unanswered until the channel is closed or the
	// client gives up.
	Hang <-chan struct{}
	// Drop closes the connection without an answer, after the request was
	// read completely.
	Drop bool
}

// Double is one local server.
type Double struct {
	// URL is the base URL of the server.
	URL string

	server *httptest.Server
	ok     int

	mu       sync.Mutex
	requests []Request
	answer   func(n int, req Request) Answer
	early    *early
}

// early is an answer given before the request body is read.
type early struct {
	status int
	linger time.Duration
}

// New starts a double that answers status ok to every request (200 for
// ClickHouse, 204 for Loki) until Answer says otherwise. With useTLS it
// serves HTTPS with a certificate only CertPEM vouches for.
func New(t testing.TB, ok int, useTLS bool) *Double {
	t.Helper()
	d := &Double{ok: ok}
	d.server = httptest.NewUnstartedServer(http.HandlerFunc(d.serve))
	if useTLS {
		d.server.StartTLS()
	} else {
		d.server.Start()
	}
	d.URL = d.server.URL
	t.Cleanup(d.server.Close)
	return d
}

// CertPEM returns the certificate of a TLS double, for a caRef.
func (d *Double) CertPEM() []byte {
	cert := d.server.Certificate()
	if cert == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// Answer sets what the double does with its n-th request, counted from 1.
func (d *Double) Answer(fn func(n int, req Request) Answer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answer = fn
}

// AnswerBeforeBody makes the double answer every request with status as
// soon as its headers have arrived, without reading the body and without
// sending "100 Continue", the way a server refuses a request at its
// headers. The double then counts, for up to linger, the bytes the client
// still sends, and closes the connection with a reset (SO_LINGER 0): a
// client that was still writing the body can lose the answer. Status 0
// restores normal answers.
func (d *Double) AnswerBeforeBody(status int, linger time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if status == 0 {
		d.early = nil
		return
	}
	d.early = &early{status: status, linger: linger}
}

// Requests returns the requests received so far.
func (d *Double) Requests() []Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Request(nil), d.requests...)
}

// WaitRequests returns the requests once there are at least n. A double set
// with AnswerBeforeBody records a request only when it has closed the
// connection, which can be after the client has its answer.
func (d *Double) WaitRequests(t testing.TB, n int) []Request {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		reqs := d.Requests()
		if len(reqs) >= n {
			return reqs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests, want %d:\n%s", len(reqs), n, Describe(reqs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close stops the double; later connections are refused.
func (d *Double) Close() { d.server.Close() }

func (d *Double) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	e := d.early
	d.mu.Unlock()
	if e != nil {
		d.serveEarly(w, r, e)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: raw, CloseAfter: r.Close}
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			http.Error(w, "gzip", http.StatusBadRequest)
			return
		}
		if req.Body, err = io.ReadAll(io.LimitReader(zr, 256<<20)); err != nil {
			http.Error(w, "gzip", http.StatusBadRequest)
			return
		}
		req.Compressed = true
	}
	d.mu.Lock()
	d.requests = append(d.requests, req)
	n, answer := len(d.requests), d.answer
	d.mu.Unlock()
	var a Answer
	if answer != nil {
		a = answer(n, req)
	}
	if a.Hang != nil {
		select {
		case <-a.Hang:
		case <-r.Context().Done():
			return
		}
	}
	if a.Drop {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	for name, values := range a.Header {
		w.Header()[name] = values
	}
	if a.Status == 0 {
		a.Status = d.ok
	}
	w.WriteHeader(a.Status)
	_, _ = io.WriteString(w, a.Body)
}

func (d *Double) serveEarly(w http.ResponseWriter, r *http.Request, e *early) {
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), CloseAfter: r.Close}
	defer func() {
		d.mu.Lock()
		d.requests = append(d.requests, req)
		d.mu.Unlock()
	}()
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	// Whatever of the body has already arrived counts as sent after the
	// answer: a client that waits sends nothing before it.
	req.SentAfterAnswer = rw.Reader.Buffered()
	_, _ = rw.Reader.Discard(req.SentAfterAnswer)
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", e.status, http.StatusText(e.status)); err != nil {
		return
	}
	if e.linger <= 0 {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(e.linger))
	buf := make([]byte, 32<<10)
	for {
		n, err := rw.Read(buf)
		req.SentAfterAnswer += n
		if err != nil {
			return
		}
	}
}

// Basic returns the Authorization header value of HTTP basic authentication.
func Basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// ClickHouseRows parses the body of an INSERT ... FORMAT JSONEachRow
// request: one JSON object per line.
func ClickHouseRows(t testing.TB, req Request) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for line := range strings.Lines(string(req.Body)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			t.Fatalf("ClickHouse row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// LokiStream is one stream of a push.
type LokiStream struct {
	Labels  map[string]string
	Entries []LokiEntry
}

// LokiEntry is one log line of a stream.
type LokiEntry struct {
	Time time.Time
	Line string
	// Metadata is the structured metadata; nil when the entry has none.
	Metadata map[string]string
}

// LokiStreams parses the body of a push in Loki's JSON format, as strictly
// as Loki reads it: timestamps are strings of Unix nanoseconds, lines are
// strings, and structured metadata is an object of strings.
func LokiStreams(t testing.TB, req Request) []LokiStream {
	t.Helper()
	var body struct {
		Streams []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"streams"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("Loki push: %v", err)
	}
	var out []LokiStream
	for _, s := range body.Streams {
		st := LokiStream{Labels: s.Stream}
		for _, value := range s.Values {
			if len(value) != 2 && len(value) != 3 {
				t.Fatalf("Loki entry has %d elements", len(value))
			}
			var ts, line string
			if err := json.Unmarshal(value[0], &ts); err != nil {
				t.Fatalf("Loki timestamp %s is not a string: %v", value[0], err)
			}
			ns, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				t.Fatalf("Loki timestamp %q: %v", ts, err)
			}
			if err := json.Unmarshal(value[1], &line); err != nil {
				t.Fatalf("Loki line %s is not a string: %v", value[1], err)
			}
			e := LokiEntry{Time: time.Unix(0, ns).UTC(), Line: line}
			if len(value) == 3 {
				if err := json.Unmarshal(value[2], &e.Metadata); err != nil {
					t.Fatalf("Loki structured metadata %s: %v", value[2], err)
				}
			}
			st.Entries = append(st.Entries, e)
		}
		out = append(out, st)
	}
	return out
}

// LineJSON decodes the JSON log line of an entry.
func (e LokiEntry) LineJSON(t testing.TB) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(e.Line))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("Loki line %q is not JSON: %v", e.Line, err)
	}
	return out
}

// Describe summarizes requests for a failure message without their bodies.
func Describe(reqs []Request) string {
	var b strings.Builder
	for i, r := range reqs {
		fmt.Fprintf(&b, "%d: %s %s (%d bytes)\n", i+1, r.Method, r.Path, len(r.Body))
	}
	return b.String()
}
